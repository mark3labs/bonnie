package sandbox

import (
	"context"
	"errors"
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

// TestMicrosandboxRefusesUnenforcedPolicy is the honesty rule applied to
// BONNIE's own adapter. microsandbox is the one backend that *can* enforce a
// domain allow-list, but this adapter does not yet pass the policy to msb. An
// earlier version stored the policy and returned nil, so an operator who asked
// for deny-all got a success and full egress.
//
// Delete this test when T-013 wires the policy up — and replace it with one
// that proves egress is actually blocked.
func TestMicrosandboxRefusesUnenforcedPolicy(t *testing.T) {
	t.Parallel()
	p := Microsandbox()

	if err := p.SetNetworkPolicy(NetworkPolicy{Mode: NetworkAllowAll}); err != nil {
		t.Fatalf("allow-all is the default and must be accepted: %v", err)
	}
	for _, mode := range []NetworkMode{NetworkDenyAll, NetworkAllowList} {
		err := p.SetNetworkPolicy(NetworkPolicy{Mode: mode})
		if !errors.Is(err, ErrPolicyUnsupported) {
			t.Fatalf("mode %q returned %v, want ErrPolicyUnsupported: accepting a "+
				"policy this adapter cannot apply tells an operator they are "+
				"protected when they are not", mode, err)
		}
	}
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
