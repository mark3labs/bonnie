package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestParseMarkerFindsExitCode covers the mechanism that makes a CLI backend
// trustworthy. See execWithMarker for why the CLI's own exit code cannot be
// used.
func TestParseMarkerFindsExitCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		stdout   string
		nonce    string
		wantCode int
		wantOut  string
		wantOK   bool
	}{
		{
			name:     "clean output",
			stdout:   "hello\n" + markerPrefix + "abc:0\n",
			nonce:    "abc",
			wantCode: 0, wantOut: "hello", wantOK: true,
		},
		{
			name:     "non-zero exit",
			stdout:   "oops\n" + markerPrefix + "abc:42\n",
			nonce:    "abc",
			wantCode: 42, wantOut: "oops", wantOK: true,
		},
		{
			name:     "no output at all",
			stdout:   "\n" + markerPrefix + "abc:0\n",
			nonce:    "abc",
			wantCode: 0, wantOut: "", wantOK: true,
		},
		{
			// The CLI died before the guest ran. Reporting the CLI's code
			// as the command's would tell the model its command failed.
			name:   "marker absent",
			stdout: "Error: No such container: bonnie-x\n",
			nonce:  "abc",
			wantOK: false,
		},
		{
			// A command that prints something marker-shaped must not be
			// mistaken for the real marker, which is why the nonce exists.
			name:     "output imitates a marker",
			stdout:   markerPrefix + "deadbeef:99\nreal output\n" + markerPrefix + "abc:7\n",
			nonce:    "abc",
			wantCode: 7,
			wantOut:  markerPrefix + "deadbeef:99\nreal output",
			wantOK:   true,
		},
		{
			name:     "trailing whitespace after the code",
			stdout:   "x\n" + markerPrefix + "abc:3  \n",
			nonce:    "abc",
			wantCode: 3, wantOut: "x", wantOK: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out, code, ok := parseMarker(c.stdout, c.nonce)
			if ok != c.wantOK {
				t.Fatalf("found = %v, want %v", ok, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if code != c.wantCode {
				t.Fatalf("code = %d, want %d", code, c.wantCode)
			}
			if out != c.wantOut {
				t.Fatalf("stdout = %q, want %q", out, c.wantOut)
			}
		})
	}
}

// TestExecWithMarkerPassesArgvThroughShell documents why the argv goes after
// the script rather than being interpolated into it: the shell does the
// quoting, so no escaping is needed here.
func TestExecWithMarkerPassesArgvThroughShell(t *testing.T) {
	t.Parallel()
	argv, nonce, err := execWithMarker(Command{Args: []string{"echo", "a b", `"c"`}})
	if err != nil {
		t.Fatalf("execWithMarker: %v", err)
	}
	if nonce == "" {
		t.Fatal("no nonce")
	}
	if argv[0] != "sh" || argv[1] != "-lc" {
		t.Fatalf("argv starts %v", argv[:2])
	}
	if !strings.Contains(argv[2], `"$@"`) {
		t.Fatalf("script does not expand the argv safely: %q", argv[2])
	}
	// argv[3] is $0, then the user's command follows unmodified.
	if argv[3] != "bonnie" || argv[4] != "echo" || argv[5] != "a b" || argv[6] != `"c"` {
		t.Fatalf("argv was rewritten: %v", argv[3:])
	}
}

func TestExecWithMarkerRejectsEmptyCommand(t *testing.T) {
	t.Parallel()
	if _, _, err := execWithMarker(Command{}); err == nil {
		t.Fatal("want an error for an empty command")
	}
}

func TestNoncesDiffer(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for range 100 {
		n, err := randomNonce()
		if err != nil {
			t.Fatalf("randomNonce: %v", err)
		}
		if seen[n] {
			t.Fatalf("nonce %q repeated", n)
		}
		seen[n] = true
	}
}

func TestResolveAnchorsToWorkspace(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"", Workspace},
		{"a.txt", Workspace + "/a.txt"},
		{"sub/a.txt", Workspace + "/sub/a.txt"},
		{"./a.txt", Workspace + "/a.txt"},
		{"/etc/hosts", "/etc/hosts"},
		{Workspace + "/a.txt", Workspace + "/a.txt"},
		{"a/../b.txt", Workspace + "/b.txt"},
	}
	for _, c := range cases {
		if got := Resolve(c.in); got != c.want {
			t.Errorf("Resolve(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSafeNameIsInjective guards the property that keeps two runs from
// sharing one container. An earlier version mapped every unsafe character to a
// dash, so "a.b", "a/b", and "a b" all became one name — and one run could
// read another's files.
func TestSafeNameIsInjective(t *testing.T) {
	t.Parallel()

	ids := []string{
		"a.b", "a-b", "a/b", "a b", "a_b",
		"run-1", "run.1", "run 1",
		"../escape", "..-escape",
		strings.Repeat("x", 80), strings.Repeat("x", 81),
	}

	seen := make(map[string]string, len(ids))
	for _, id := range ids {
		name := safeName("bonnie-", id)
		if prev, clash := seen[name]; clash {
			t.Fatalf("run IDs %q and %q both map to %q: two runs would share a sandbox",
				prev, id, name)
		}
		seen[name] = id

		if len(name) > maxNameLen {
			t.Errorf("safeName(%q) is %d bytes, over the %d limit", id, len(name), maxNameLen)
		}
		for _, r := range name {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
				r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
			if !ok {
				t.Errorf("safeName(%q) = %q contains %q", id, name, string(r))
			}
		}
	}
}

// TestSafeNameIsStable matters for reattachment. The name is derived, not
// stored, so a run finds its sandbox again only if the derivation never
// changes. Golden values catch an algorithm change that would silently orphan
// every live sandbox across a BONNIE upgrade.
func TestSafeNameIsStable(t *testing.T) {
	t.Parallel()
	golden := map[string]string{
		"run-1":   "bonnie-run-1",
		"ok_id.9": "bonnie-ok_id.9",
		"a/b":     "bonnie-a-b-c14cddc0",
		"a b":     "bonnie-a-b-c8687a08",
	}
	for id, want := range golden {
		if got := safeName("bonnie-", id); got != want {
			t.Errorf("safeName(%q) = %q, want %q.\n"+
				"If this change is deliberate, every sandbox created by an "+
				"earlier version becomes unreachable.", id, got, want)
		}
	}
}

func TestSafeNamePassesCleanIDsThrough(t *testing.T) {
	t.Parallel()
	// A clean ID must not gain a digest: the name stays readable for an
	// operator running `docker ps`.
	cases := []struct{ in, want string }{
		{"run-1", "bonnie-run-1"},
		{"ok_id.9", "bonnie-ok_id.9"},
	}
	for _, c := range cases {
		if got := safeName("bonnie-", c.in); got != c.want {
			t.Errorf("safeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCommandValidation(t *testing.T) {
	t.Parallel()
	if err := (Command{Args: []string{"ls"}}).validate(); err != nil {
		t.Fatalf("valid command rejected: %v", err)
	}
	if err := (Command{}).validate(); err == nil {
		t.Fatal("empty args must be rejected")
	}
	if err := (Command{Args: []string{"ls"}, Env: []string{"NOEQUALS"}}).validate(); err == nil {
		t.Fatal("malformed env must be rejected")
	}
}

func TestResultHelpers(t *testing.T) {
	t.Parallel()
	ok := &Result{ExitCode: 0, Stdout: "out"}
	if !ok.OK() || ok.Output() != "out" {
		t.Fatalf("result = %+v", ok)
	}
	// Stderr is what the model needs when stdout is empty.
	bad := &Result{ExitCode: 1, Stderr: "boom"}
	if bad.OK() || bad.Output() != "boom" {
		t.Fatalf("result = %+v", bad)
	}
	var nilRes *Result
	if nilRes.OK() || nilRes.Output() != "" {
		t.Fatal("a nil result must be safe to inspect")
	}
}

// TestDockerRefusesAllowList is the honesty contract for network policy. A
// backend that cannot enforce a policy must say so, not run with open egress
// while the operator believes otherwise.
func TestDockerRefusesAllowList(t *testing.T) {
	t.Parallel()
	p := Docker()

	if err := p.SetNetworkPolicy(NetworkPolicy{Mode: NetworkDenyAll}); err != nil {
		t.Fatalf("deny-all must be supported: %v", err)
	}
	err := p.SetNetworkPolicy(NetworkPolicy{
		Mode:  NetworkAllowList,
		Allow: []string{"github.com"},
	})
	if !errors.Is(err, ErrPolicyUnsupported) {
		t.Fatalf("err = %v, want ErrPolicyUnsupported", err)
	}
}

// TestMicrosandboxAcceptsEveryPolicy pins the acceptance side of the
// honesty contract. The adapter now enforces every mode, so it must accept
// every mode; the enforcement side is TestMicrosandboxEnforcesNetworkPolicy.
func TestMicrosandboxAcceptsEveryPolicy(t *testing.T) {
	t.Parallel()
	p := Microsandbox()

	for _, mode := range []NetworkMode{NetworkAllowAll, NetworkDenyAll, NetworkAllowList} {
		policy := NetworkPolicy{Mode: mode}
		if mode == NetworkAllowList {
			policy.Allow = []string{"example.com", "*.github.com"}
		}
		if err := p.SetNetworkPolicy(policy); err != nil {
			t.Fatalf("mode %q must be accepted: %v", mode, err)
		}
	}
	// An empty host in an allow-list is a caller bug, not a policy the
	// backend could apply, so it is rejected before any sandbox exists.
	err := p.SetNetworkPolicy(NetworkPolicy{Mode: NetworkAllowList, Allow: []string{"example.com", " "}})
	if !strings.Contains(err.Error(), "empty host") {
		t.Fatalf("err = %v, want empty-host rejection", err)
	}
	if err := p.SetNetworkPolicy(NetworkPolicy{Mode: NetworkMode("nope")}); !errors.Is(err, ErrPolicyUnsupported) {
		t.Fatalf("err = %v, want ErrPolicyUnsupported", err)
	}
}

// TestNetArgs pins the create flags each policy maps to. These flags are the
// enforcement itself, so the mapping is tested on its own, cheaply, on every
// machine — not only where msb is installed.
func TestNetArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		policy NetworkPolicy
		want   []string
	}{
		{
			name:   "allow-all adds no flags",
			policy: NetworkPolicy{Mode: NetworkAllowAll},
			want:   nil,
		},
		{
			name:   "deny-all",
			policy: NetworkPolicy{Mode: NetworkDenyAll},
			want:   []string{"--no-net"},
		},
		{
			name: "allow-list keeps no-net and adds a rule per host",
			policy: NetworkPolicy{
				Mode:  NetworkAllowList,
				Allow: []string{"example.com", "*.github.com"},
			},
			want: []string{"--no-net", "--net-rule", "allow@example.com",
				"--net-rule", "allow@*.github.com"},
		},
		{
			name:   "an allow-list with no hosts permits nothing",
			policy: NetworkPolicy{Mode: NetworkAllowList},
			want:   []string{"--no-net"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := netArgs(tc.policy)
			if err != nil {
				t.Fatalf("netArgs: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("netArgs = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := netArgs(NetworkPolicy{Mode: NetworkMode("nope")}); !errors.Is(err, ErrPolicyUnsupported) {
		t.Fatal("an unknown mode must be refused, not silently treated as allow-all")
	}
}

// TestPolicyMatches pins the shapes `msb inspect --format json` produces for
// each policy. The fixtures below are recorded output from msb 0.6.18, not
// documentation paraphrase: if msb changes the shape, this test says so and
// checkPolicy is the code to revisit.
func TestPolicyMatches(t *testing.T) {
	t.Parallel()

	// From a sandbox created with no network flags.
	const nullPolicy = `null`
	// From `msb create --no-net`.
	const denyAll = `{"default_egress":"deny","default_ingress":"deny","rules":[]}`
	// From `msb create --no-net --net-rule allow@example.com`.
	const allowPlain = `{"default_egress":"deny","default_ingress":"deny","rules":[` +
		`{"action":"allow","destination":{"domain":"example.com"},` +
		`"direction":"egress","ports":[],"protocols":[]}]}`
	// From `msb create --no-net --net-rule 'allow@*.github.com'`.
	const allowWildcard = `{"default_egress":"deny","default_ingress":"deny","rules":[` +
		`{"action":"allow","destination":{"domain_suffix":"github.com"},` +
		`"direction":"egress","ports":[],"protocols":[]}]}`

	decode := func(t *testing.T, raw string) *msbNetworkPolicy {
		t.Helper()
		var got *msbNetworkPolicy
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("fixture does not decode: %v", err)
		}
		return got
	}

	allowList := func(hosts ...string) NetworkPolicy {
		return NetworkPolicy{Mode: NetworkAllowList, Allow: hosts}
	}

	t.Run("allow-all matches only a null policy", func(t *testing.T) {
		t.Parallel()
		if !policyMatches(decode(t, nullPolicy), NetworkPolicy{Mode: NetworkAllowAll}) {
			t.Fatal("a sandbox created with no flags must match allow-all")
		}
		if policyMatches(decode(t, denyAll), NetworkPolicy{Mode: NetworkAllowAll}) {
			t.Fatal("a deny-all sandbox must not pass as allow-all")
		}
	})

	t.Run("deny-all matches empty rules and deny egress", func(t *testing.T) {
		t.Parallel()
		if !policyMatches(decode(t, denyAll), NetworkPolicy{Mode: NetworkDenyAll}) {
			t.Fatal("the real deny-all shape must match")
		}
		if policyMatches(decode(t, allowPlain), NetworkPolicy{Mode: NetworkDenyAll}) {
			t.Fatal("a sandbox with rules must not pass as deny-all")
		}
		if policyMatches(decode(t, nullPolicy), NetworkPolicy{Mode: NetworkDenyAll}) {
			t.Fatal("a default sandbox must not pass as deny-all")
		}
	})

	t.Run("allow-list matches the same host set", func(t *testing.T) {
		t.Parallel()
		if !policyMatches(decode(t, allowPlain), allowList("example.com")) {
			t.Fatal("the real plain-domain shape must match")
		}
		if !policyMatches(decode(t, allowWildcard), allowList("*.github.com")) {
			t.Fatal("the real wildcard shape must match")
		}
		// A wildcard and a plain domain are different rules in msb, so one
		// must never satisfy the other.
		if policyMatches(decode(t, allowWildcard), allowList("github.com")) {
			t.Fatal("a suffix rule must not pass as a plain-domain allow")
		}
		// Same count, different hosts.
		if policyMatches(decode(t, allowPlain), allowList("github.com")) {
			t.Fatal("a different host must not pass")
		}
		// Fewer rules than hosts.
		if policyMatches(decode(t, allowPlain), allowList("example.com", "github.com")) {
			t.Fatal("a missing host must not pass")
		}
	})
}

// TestSelectNeverFallsBackToLocal is a security property, not a convenience.
// Dropping from isolation to none must be a decision someone wrote down.
func TestSelectNeverFallsBackToLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	unavailable := Microsandbox(WithMicrosandboxBinary("definitely-not-a-real-binary"))
	if _, err := Select(ctx, unavailable); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}

	// Named explicitly, local is a fine last resort.
	p, err := Select(ctx, unavailable, Local())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name() != "local" {
		t.Fatalf("selected %q, want local", p.Name())
	}
}

func TestSelectNeedsCandidates(t *testing.T) {
	t.Parallel()
	if _, err := Select(context.Background()); err == nil {
		t.Fatal("want an error with no candidates")
	}
}

// TestSelectReportsEveryReason keeps the failure message useful: an operator
// needs to know why each backend was rejected, not just that none worked.
func TestSelectReportsEveryReason(t *testing.T) {
	t.Parallel()
	_, err := Select(context.Background(),
		Microsandbox(WithMicrosandboxBinary("no-such-msb")),
		Docker(WithDockerBinary("no-such-docker")),
	)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"microsandbox", "docker", "no-such-msb", "no-such-docker"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error does not mention %q: %s", want, msg)
		}
	}
}

func TestUnavailableBackendIsDetected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if err := Docker(WithDockerBinary("no-such-docker")).Available(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	err := Microsandbox(WithMicrosandboxBinary("no-such-msb")).Available(ctx)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// The message must tell the user how to fix it.
	if !strings.Contains(err.Error(), "github.com/superradcompany/microsandbox") {
		t.Fatalf("message does not say where to get msb: %s", err)
	}
}

func TestShellBuildsACommandLine(t *testing.T) {
	t.Parallel()
	cmd := Shell("ls | wc -l")
	if len(cmd.Args) != 3 || cmd.Args[0] != "sh" || cmd.Args[2] != "ls | wc -l" {
		t.Fatalf("Shell produced %v", cmd.Args)
	}
}

func TestTruncateMarksDroppedOutput(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", maxToolOutput+500)
	got := truncate(long)
	if len(got) <= maxToolOutput {
		t.Fatal("truncate returned nothing")
	}
	// The model must know the text is partial, or it reasons about a file
	// that appears to end early.
	if !strings.Contains(got, "truncated") {
		t.Fatal("truncated output does not say so")
	}
	if short := truncate("fine"); short != "fine" {
		t.Fatalf("short output was changed: %q", short)
	}
}

func TestRenderResultShowsExitCodeOnlyOnFailure(t *testing.T) {
	t.Parallel()
	if got := renderResult(&Result{Stdout: "done\n"}); got != "done" {
		t.Fatalf("success rendered as %q", got)
	}
	got := renderResult(&Result{ExitCode: 2, Stdout: "partial\n", Stderr: "bad\n"})
	for _, want := range []string{"partial", "stderr: bad", "exit code 2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q: %q", want, got)
		}
	}
	if got := renderResult(&Result{}); got != "(no output)" {
		t.Fatalf("empty result rendered as %q", got)
	}
}

// TestMsbMissingPathSeparatesAbsentFileFromAbsentSandbox pins a distinction
// the microsandbox adapter got wrong.
//
// msb reports an absent guest path as "error: stat <path>", which the generic
// isMissingPath does not recognise, so ReadFile returned a bare error instead
// of ErrNotFound and no caller could tell the two apart. The opposite mistake
// is worse: "sandbox not found" contains "not found" and so satisfies the
// generic helper, which would report a vanished workspace to the model as an
// ordinary missing file.
func TestMsbMissingPathSeparatesAbsentFileFromAbsentSandbox(t *testing.T) {
	t.Parallel()
	const path = "/workspace/does/not/exist.txt"

	if !msbMissingPath("error: stat "+path+"\n", path) {
		t.Fatal("the real msb wording for an absent path was not recognised")
	}
	// A gone workspace is a different failure and must not be flattened
	// into ErrNotFound, even though the generic helper matches its wording.
	if msbMissingPath("error: sandbox not found: bonnie-run-1\n", path) {
		t.Fatal("an absent sandbox was reported as an absent file")
	}
	if !isMissingPath("error: sandbox not found: bonnie-run-1") {
		t.Skip("generic helper no longer matches; the guard above is moot")
	}
	// A failure naming a different path is not this path going missing.
	if msbMissingPath("error: stat /workspace/other.txt\n", path) {
		t.Fatal("a failure on another path was read as this one missing")
	}
}

// requireMsb returns the microsandbox provider and skips when msb cannot run
// here. The policy tests below need real hardware: enforcement is a property
// of a running microVM, not of the argv that requests it.
func requireMsb(t *testing.T) *MicrosandboxProvider {
	t.Helper()
	p := Microsandbox()
	if err := p.Available(context.Background()); err != nil {
		t.Skipf("microsandbox unavailable: %v", err)
	}
	return p
}

// TestMicrosandboxEnforcesNetworkPolicy proves the enforcement claim with real
// egress, on real hardware. It replaced a test that proved only the refusal.
//
// A control sandbox runs first: when this host has no egress at all, no
// policy can be verified here, and the test skips rather than report a block
// it did not see.
func TestMicrosandboxEnforcesNetworkPolicy(t *testing.T) {
	t.Parallel()
	p := requireMsb(t)
	ctx := testCtx(t)

	fetch := func(sb Sandbox, url string) *Result {
		t.Helper()
		res, err := sb.Exec(ctx, Shell("wget -q -T 10 -O /dev/null "+url))
		if err != nil {
			t.Fatalf("exec %s: %v", url, err)
		}
		return res
	}

	// Control: the default policy must reach the internet.
	control := openSandbox(t, p, "net-control")
	if res := fetch(control, "http://example.com"); res.ExitCode != 0 {
		t.Skipf("no egress from this host through msb (exit %d, stderr %q): "+
			"cannot verify policy enforcement here", res.ExitCode, res.Stderr)
	}

	// deny-all: no reachability at all.
	deny := Microsandbox()
	if err := deny.SetNetworkPolicy(NetworkPolicy{Mode: NetworkDenyAll}); err != nil {
		t.Fatalf("SetNetworkPolicy deny-all: %v", err)
	}
	denied := openSandbox(t, deny, "net-deny")
	if res := fetch(denied, "http://example.com"); res.ExitCode == 0 {
		t.Fatal("a deny-all sandbox reached the internet: the policy is not enforced")
	}

	// allow-list: the listed host answers, an unlisted one does not.
	allow := Microsandbox()
	if err := allow.SetNetworkPolicy(NetworkPolicy{
		Mode:  NetworkAllowList,
		Allow: []string{"example.com"},
	}); err != nil {
		t.Fatalf("SetNetworkPolicy allow-list: %v", err)
	}
	allowed := openSandbox(t, allow, "net-allow")
	if res := fetch(allowed, "http://example.com"); res.ExitCode != 0 {
		t.Fatalf("the allowed host must be reachable (exit %d, stderr %q)",
			res.ExitCode, res.Stderr)
	}
	if res := fetch(allowed, "http://github.com"); res.ExitCode == 0 {
		t.Fatal("the sandbox reached a host that is not on the allow-list")
	}
}

// TestMicrosandboxRefusesReattachPolicyMismatch covers the one way a policy
// can silently stop applying: the sandbox already exists, and network policy
// is fixed at create time. A host that reconfigured its policy must hear
// about the divergence, not reattach as if nothing differed.
func TestMicrosandboxRefusesReattachPolicyMismatch(t *testing.T) {
	t.Parallel()
	requireMsb(t)
	ctx := testCtx(t)

	// Create the sandbox under a strict policy.
	strict := Microsandbox()
	if err := strict.SetNetworkPolicy(NetworkPolicy{Mode: NetworkDenyAll}); err != nil {
		t.Fatalf("SetNetworkPolicy: %v", err)
	}
	openSandbox(t, strict, "net-mismatch") // cleanup deletes with the same policy

	// A host configured allow-all now must not silently reattach to a
	// sandbox that was created deny-all.
	open := Microsandbox()
	if _, err := open.Open(ctx, "net-mismatch"); !errors.Is(err, ErrPolicyMismatch) {
		t.Fatalf("err = %v, want ErrPolicyMismatch: reattaching under a different "+
			"policy must be loud, not silent", err)
	}

	// The same policy reattaches without complaint.
	same := Microsandbox()
	if err := same.SetNetworkPolicy(NetworkPolicy{Mode: NetworkDenyAll}); err != nil {
		t.Fatalf("SetNetworkPolicy: %v", err)
	}
	if _, err := same.Open(ctx, "net-mismatch"); err != nil {
		t.Fatalf("reattach with the same policy must work: %v", err)
	}
}
