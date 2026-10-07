package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/internal/shelltest"
)

// TestLandlockDanglingSymlink refuses writes through a link whose target does
// not exist. A check of the nearest existing ancestor used to miss this link.
func TestLandlockDanglingSymlink(t *testing.T) {
	t.Parallel()
	p := Landlock(WithLandlockRoot(t.TempDir()), WithLandlockCleanup())
	sb := openSandbox(t, p, "dangling")
	s := sb.(*landlockSandbox)
	outside := filepath.Join(t.TempDir(), "missing")
	if err := os.Symlink(outside, filepath.Join(s.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(context.Background(), "link", []byte("secret")); !errors.Is(err, ErrOutsideWorkDir) {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file: %v", err)
	}
}

// TestLandlockSymlinkSwap races a file operation against a symlink replacement.
// No operation may use a host path checked before the replacement.
func TestLandlockSymlinkSwap(t *testing.T) {
	t.Parallel()
	sb := openSandbox(t, Landlock(WithLandlockRoot(t.TempDir()), WithLandlockCleanup()), "swap")
	s := sb.(*landlockSandbox)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "value"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.dir, "inside"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.dir, "link")
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(done)
		for range 1000 {
			_ = os.Remove(link)
			_ = os.Symlink(outside, link)
			_ = os.Remove(link)
			_ = os.Symlink("inside", link)
		}
	})
	for {
		select {
		case <-done:
			wg.Wait()
			goto finished
		default:
		}
		_ = sb.WriteFile(context.Background(), "link/value", []byte("inside"))
		data, err := sb.ReadFile(context.Background(), "link/value")
		if err == nil && string(data) == "outside" {
			t.Error("read escaped root")
			break
		}
	}
	wg.Wait()
finished:
	data, err := os.ReadFile(filepath.Join(outside, "value"))
	if err != nil || string(data) != "outside" {
		t.Fatalf("outside file changed: %q, %v", data, err)
	}
}

// TestLandlockScratchNamespace keeps a run named x.tmp out of run x's scratch.
func TestLandlockScratchNamespace(t *testing.T) {
	t.Parallel()
	p := Landlock(WithLandlockRoot(t.TempDir()), WithLandlockCleanup())
	a := openSandbox(t, p, "x").(*landlockSandbox)
	b := openSandbox(t, p, "x.tmp").(*landlockSandbox)
	if a.tmp == b.dir || strings.HasPrefix(a.tmp, b.dir+string(filepath.Separator)) {
		t.Fatal("scratch aliases another work directory")
	}
	if err := os.WriteFile(filepath.Join(a.tmp, "scratch"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadFile(context.Background(), "scratch"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scratch visible: %v", err)
	}
}

// TestSafeNameGeneratedPlainCollision covers both current and old generated
// names, plus directory names that would address the provider root itself.
func TestSafeNameGeneratedPlainCollision(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"a/b", "a b", strings.Repeat("a", 100), ".", "..", ".scratch", ""} {
		generated := safeName("", id)
		if generated == safeName("", generated) {
			t.Fatalf("generated/plain collision for %q", id)
		}
		if generated == "." || generated == ".." || generated == "" {
			t.Fatalf("unsafe name %q", generated)
		}
	}
	if safeName("", "a/b") == safeName("", legacySafeName("", "a/b")) {
		t.Fatal("old generated/plain collision")
	}
}

// TestHostLegacyAdoptionRefused prevents an unlabelled old lossy work directory
// from being assigned to the wrong ID. Clean IDs still reopen without change.
func TestHostLegacyAdoptionRefused(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, legacySafeName("", "a/b")), 0700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Provider{Local(WithLocalRoot(root)), Landlock(WithLandlockRoot(root))} {
		for _, id := range []string{"a/b", legacySafeName("", "a/b")} {
			if _, err := p.Open(context.Background(), id); !errors.Is(err, ErrPolicyMismatch) {
				t.Fatalf("%s adoption %q: %v", p.Name(), id, err)
			}
		}
	}
}

// fakeSecurityCLI supplies refusal diagnostics without a live daemon.
func fakeSecurityCLI(t *testing.T, script string) string {
	t.Helper()
	if os.PathSeparator != '/' {
		t.Skip("shell fixture needs Unix")
	}
	bin := filepath.Join(t.TempDir(), "cli")
	shelltest.Write(t, bin, script)
	return bin
}

// TestLifecycleUncertainExistence is not allowed to become false,nil.
func TestLifecycleUncertainExistence(t *testing.T) {
	t.Parallel()
	for _, script := range []string{"echo daemon-down >&2; exit 1", "echo invalid-json", "echo '{}'"} {
		bin := fakeSecurityCLI(t, script)
		p := Microsandbox(WithMicrosandboxBinary(bin))
		if _, err := p.SandboxExists(context.Background(), "run"); err == nil {
			t.Fatal("uncertain existence became absence")
		}
		if _, err := p.DeleteRun(context.Background(), "run"); err == nil {
			t.Fatal("uncertain delete became success")
		}
	}
	d := Docker(WithDockerBinary(fakeSecurityCLI(t, "echo daemon-down >&2; exit 1")))
	if _, err := d.SandboxExists(context.Background(), "run"); err == nil {
		t.Fatal("docker daemon failure became absence")
	}
}

// TestCLIExitStatusRefused covers successful process launch with failed stop,
// delete, or guest work directory setup. These failures used to be discarded.
func TestCLIExitStatusRefused(t *testing.T) {
	t.Parallel()
	bin := fakeSecurityCLI(t, "echo refused >&2; exit 9")
	for _, p := range []Provider{Docker(WithDockerBinary(bin)), Microsandbox(WithMicrosandboxBinary(bin))} {
		// Inspect can report missing, but Open must surface the create refusal.
		if _, err := p.Open(context.Background(), "run"); err == nil {
			t.Fatalf("%s Open succeeded", p.Name())
		}
	}
	p := Microsandbox(WithMicrosandboxBinary(bin))
	if err := p.start(context.Background(), "run"); err == nil {
		t.Fatal("start refusal ignored")
	}
	if err := p.ensureWorkDir(context.Background(), &cliSandbox{name: "run"}); err == nil {
		t.Fatal("mkdir refusal ignored")
	}
}

// TestCLICancelKillsGuest confirms cancellation calls a guest-side kill with
// a new context, not the already canceled command context.
func TestCLICancelKillsGuest(t *testing.T) {
	t.Parallel()
	killed := false
	c := &cliSandbox{bin: fakeSecurityCLI(t, "sleep 10"), backend: "fake", execArgs: func(_ *cliSandbox, a []string, _ Command) []string { return a }, killFn: func(ctx context.Context, _ *cliSandbox) error { killed = true; return ctx.Err() }}
	cmd := Shell("sleep 10")
	cmd.Timeout = 30 * time.Millisecond
	if _, err := c.Exec(context.Background(), cmd); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	if !killed {
		t.Fatal("guest kill was not called")
	}
	c.killFn = nil
	if _, err := c.Exec(context.Background(), Shell("true")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no kill support: %v", err)
	}
}

// TestDockerAdoptionControls rejects foreign identity and controls that are
// weaker than a new container would have. A policy change must not attach to
// an old open-egress container.
func TestDockerAdoptionControls(t *testing.T) {
	t.Parallel()
	base := `{"Config":{"Image":"alpine:3.19","User":"","Labels":{"bonnie.run":"run"}},"HostConfig":{"NetworkMode":"default","IpcMode":"private","CgroupnsMode":"private"},"Mounts":[]}`
	cases := []struct {
		name, report string
		deny         bool
		want         bool
	}{
		{"match", base, false, true},
		{"identity", strings.Replace(base, `"bonnie.run":"run"`, `"bonnie.run":"foreign"`, 1), false, false},
		{"image", strings.Replace(base, "alpine:3.19", "foreign", 1), false, false},
		{"privileged", strings.Replace(base, `"NetworkMode"`, `"Privileged":true,"NetworkMode"`, 1), false, false},
		{"mount", strings.Replace(base, `"Mounts":[]`, `"Mounts":[{}]`, 1), false, false},
		{"pid", strings.Replace(base, `"NetworkMode"`, `"PidMode":"host","NetworkMode"`, 1), false, false},
		{"cap", strings.Replace(base, `"NetworkMode"`, `"CapAdd":["SYS_ADMIN"],"NetworkMode"`, 1), false, false},
		{"security", strings.Replace(base, `"NetworkMode"`, `"SecurityOpt":["seccomp=unconfined"],"NetworkMode"`, 1), false, false},
		{"tightened-network", base, true, false},
		{"deny-match", strings.Replace(base, `"default"`, `"none"`, 1), true, true},
		{"bad-json", "{", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := Docker(WithDockerBinary(fakeSecurityCLI(t, "printf '%s\\n' "+shellQuote(tc.report))))
			if tc.deny {
				if err := p.SetNetworkPolicy(NetworkPolicy{Mode: NetworkDenyAll}); err != nil {
					t.Fatal(err)
				}
			}
			err := p.checkContainer(context.Background(), "bonnie-run", "run")
			if (err == nil) != tc.want {
				t.Fatalf("adoption: %v, want success=%v", err, tc.want)
			}
		})
	}
}

// TestBackendLifecycleExitStatus uses handles produced by Open, so the
// backend's stop/delete closures themselves must check exit status.
func TestBackendLifecycleExitStatus(t *testing.T) {
	t.Parallel()
	dockerReport := `{"Config":{"Image":"alpine:3.19","User":"","Labels":{"bonnie.run":"run"}},"HostConfig":{"NetworkMode":"default"},"Mounts":[]}`
	dockerBin := fakeSecurityCLI(t, `case "$1" in
 inspect) if [ "$3" = '{{.State.Status}}' ]; then echo running; else printf '%s\n' `+shellQuote(dockerReport)+`; fi;;
 stop|rm) echo refused >&2; exit 9;;
 *) exit 0;;
 esac`)
	msbBin := fakeSecurityCLI(t, `case "$1" in
 inspect) echo '{"active_config":{"network":{"policy":null}}}';;
 stop|rm) echo refused >&2; exit 9;;
 *) exit 0;;
 esac`)
	for _, p := range []Provider{Docker(WithDockerBinary(dockerBin)), Microsandbox(WithMicrosandboxBinary(msbBin))} {
		sb, err := p.Open(context.Background(), "run")
		if err != nil {
			t.Fatal(err)
		}
		if err := sb.Stop(context.Background()); err == nil {
			t.Fatalf("%s stop ignored refusal", p.Name())
		}
		if err := sb.(Deleter).Delete(context.Background()); err == nil {
			t.Fatalf("%s delete ignored refusal", p.Name())
		}
		if _, err := sb.Exec(context.Background(), Shell("true")); errors.Is(err, ErrClosed) {
			t.Fatal("failed delete closed handle")
		}
	}
}

// TestDockerLegacyCheckedAdoption accepts old generated names only when the
// live run label and controls prove compatibility. A mismatched label refuses.
func TestDockerLegacyCheckedAdoption(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"a/b", "foreign"} {
		report := `{"Config":{"Image":"alpine:3.19","User":"","Labels":{"bonnie.run":` + shellQuote(id) + `}},"HostConfig":{"NetworkMode":"default"},"Mounts":[]}`
		// JSON strings use double quotes, not shell quoting.
		report = strings.ReplaceAll(report, "'", `"`)
		old := legacySafeName("bonnie-", "a/b")
		bin := fakeSecurityCLI(t, `case "$1" in
 inspect) case "$4" in
 `+old+`) if [ "$3" = '{{.State.Status}}' ]; then echo running; else printf '%s\n' `+shellQuote(report)+`; fi;;
 *) echo 'error: no such object:' >&2; exit 1;; esac;;
 *) exit 0;; esac`)
		p := Docker(WithDockerBinary(bin))
		sb, err := p.Open(context.Background(), "a/b")
		if id == "foreign" {
			if !errors.Is(err, ErrPolicyMismatch) {
				t.Fatalf("foreign legacy: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if sb.(*cliSandbox).name != old {
			t.Fatal("legacy work directory not adopted")
		}
	}
}

// TestDockerEmptyIDNeedsLabel distinguishes an empty identity from a missing
// label. An unlabeled container must not be adopted by an empty run ID.
func TestDockerEmptyIDNeedsLabel(t *testing.T) {
	t.Parallel()
	report := `{"Config":{"Image":"alpine:3.19","Labels":{}},"HostConfig":{"NetworkMode":"default"}}`
	p := Docker(WithDockerBinary(fakeSecurityCLI(t, "printf '%s\\n' "+shellQuote(report))))
	if err := p.checkContainer(context.Background(), "container", ""); !errors.Is(err, ErrPolicyMismatch) {
		t.Fatalf("missing label: %v", err)
	}
}

// TestMSBPolicyRequiresReport refuses missing fields instead of treating them
// as the default policy (allow-all).
func TestMSBPolicyRequiresReport(t *testing.T) {
	t.Parallel()
	for _, report := range []string{`{"active_config":{}}`, `{"active_config":{"network":null}}`} {
		p := Microsandbox(WithMicrosandboxBinary(fakeSecurityCLI(t, "printf '%s\\n' "+shellQuote(report))))
		if err := p.checkPolicy(context.Background(), &cliSandbox{name: "run"}); !errors.Is(err, ErrPolicyMismatch) {
			t.Fatalf("%s: %v", report, err)
		}
	}
}

// TestMSBPolicyCopiesAllow keeps a caller's slice from changing an accepted
// policy without the provider lock.
func TestMSBPolicyCopiesAllow(t *testing.T) {
	t.Parallel()
	p := Microsandbox()
	allow := []string{"example.com"}
	if err := p.SetNetworkPolicy(NetworkPolicy{Mode: NetworkAllowList, Allow: allow}); err != nil {
		t.Fatal(err)
	}
	allow[0] = "foreign.example"
	if p.policy.Allow[0] != "example.com" {
		t.Fatal("policy aliases caller slice")
	}
}

// TestCLIOperationGate keeps restart and close out of guest cancellation
// cleanup. A waiting operation must still obey its context.
func TestCLIOperationGate(t *testing.T) {
	t.Parallel()
	c := &cliSandbox{}
	release, err := c.operation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.ReadFile(ctx, "memo"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("close bypassed active operation")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	<-done
	if _, err := c.ReadFile(context.Background(), "memo"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
}

// TestLocalWorkDirPrefix maps only the work directory path component, not an
// unrelated absolute path with the same text prefix.
func TestLocalWorkDirPrefix(t *testing.T) {
	t.Parallel()
	s := &localSandbox{dir: t.TempDir()}
	if got := s.host("/workspace-other/file"); got != filepath.FromSlash("/workspace-other/file") {
		t.Fatalf("mapped unrelated path: %s", got)
	}
}

// TestLandlockRootAlias refuses a work directory link to another run, even if the
// target stays inside the provider root.
func TestLandlockRootAlias(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	p := Landlock(WithLandlockRoot(root))
	openSandbox(t, p, "target")
	if err := os.Symlink("target", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(context.Background(), "alias"); !errors.Is(err, ErrOutsideWorkDir) {
		t.Fatalf("alias: %v", err)
	}
}

// TestLandlockPinnedExec keeps a host rename from granting a replacement
// work directory to the child. The file tools and the kernel must use one identity.
func TestLandlockPinnedExec(t *testing.T) {
	t.Parallel()
	p := landlockProvider(t)
	sb := openSandbox(t, p, "pinned").(*landlockSandbox)
	moved := sb.dir + "-moved"
	if err := os.Rename(sb.dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sb.dir, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(sb.dir, "secret")
	if err := os.WriteFile(secret, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), Shell("cat "+shellQuote(secret)))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || strings.Contains(res.Stdout, "foreign") {
		t.Fatalf("replacement readable: %+v", res)
	}
}

// TestLandlockScratchDeleteEscape refuses a scratch parent link that would
// otherwise make DeleteRun remove host files outside the provider root.
func TestLandlockScratchDeleteEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	p := Landlock(WithLandlockRoot(root))
	sb := openSandbox(t, p, "scratch-delete")
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, ".scratch")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "scratch-delete")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".scratch")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeleteRun(context.Background(), "scratch-delete"); !errors.Is(err, ErrOutsideWorkDir) {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("host target removed: %v", err)
	}
}
