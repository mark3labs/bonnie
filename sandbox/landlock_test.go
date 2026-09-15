package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// landlockProvider returns a provider for a test, skipping when the kernel
// cannot enforce the restriction. A skip here is honest — the backend really
// is unavailable — but see TestLandlockIsAvailableOnThisKernel, which fails
// rather than skips on a host that should support it.
func landlockProvider(t *testing.T) *LandlockProvider {
	t.Helper()
	p := Landlock(WithLandlockRoot(t.TempDir()), WithLandlockCleanup())
	if err := p.Available(context.Background()); err != nil {
		t.Skipf("landlock unavailable: %v", err)
	}
	return p
}

// secretOutside puts a file where the agent must never reach, standing in for
// the journal the live Slack agent read (docs/SPEC.md §4.9.1).
func secretOutside(t *testing.T) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	file = filepath.Join(dir, "journal.db")
	if err := os.WriteFile(file, []byte("SECRET-JOURNAL-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

// TestLandlockConfinesTheShell is the regression test for the incident in
// issue #1: a model ran `find` over the tree by absolute path and read
// main.go, instructions.md, and .bonnie/journal.db. Rooting the tools at the
// workspace did not stop it, because an absolute path never consults a
// working directory.
//
// The shell is what has to be contained here, not the file tools. A path
// check cannot contain a shell: the path never appears as an argument the
// checker sees, it appears inside a command line the kernel expands.
func TestLandlockConfinesTheShell(t *testing.T) {
	t.Parallel()
	p := landlockProvider(t)
	_, secret := secretOutside(t)

	sb := openSandbox(t, p, "landlock-confines")
	ctx := testCtx(t)

	res, err := sb.Exec(ctx, Shell("cat "+secret))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.OK() {
		t.Fatalf("the shell read a file outside the workspace: %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "SECRET") {
		t.Fatalf("secret content reached the model: %q", res.Stdout)
	}
}

// TestLandlockConfinesDescendants closes the obvious way around the case
// above. Landlock applies to a process and everything it starts, so a
// subshell inherits the restriction; a jail that only covered the first
// process would be worthless to an agent that can write "sh -c".
func TestLandlockConfinesDescendants(t *testing.T) {
	t.Parallel()
	p := landlockProvider(t)
	dir, secret := secretOutside(t)

	sb := openSandbox(t, p, "landlock-descendants")
	ctx := testCtx(t)

	for _, line := range []string{
		"sh -c 'cat " + secret + "'",
		"sh -c \"sh -c 'cat " + secret + "'\"",
		"find " + dir + " -type f",
		"env cat " + secret,
	} {
		res, err := sb.Exec(ctx, Shell(line))
		if err != nil {
			t.Fatalf("Exec(%q): %v", line, err)
		}
		if strings.Contains(res.Stdout, "SECRET") {
			t.Fatalf("%q escaped the jail: %q", line, res.Stdout)
		}
	}
}

// TestLandlockRefusesToWriteOutside is the other half of the original
// incident (docs/SPEC.md §4.9): a model asked to "deploy the app" wrote a
// Dockerfile, a terraform/ directory, and three documents into the checkout.
func TestLandlockRefusesToWriteOutside(t *testing.T) {
	t.Parallel()
	p := landlockProvider(t)
	dir, _ := secretOutside(t)

	sb := openSandbox(t, p, "landlock-write-outside")
	ctx := testCtx(t)

	target := filepath.Join(dir, "Dockerfile")
	res, err := sb.Exec(ctx, Shell("echo FROM scratch > "+target))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.OK() {
		t.Fatal("the shell wrote outside the workspace")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file was created outside the workspace: %v", err)
	}
}

// TestLandlockAllowsTheWorkspace guards the other direction. A jail that also
// stops the agent doing its job would be abandoned, and an abandoned control
// protects nothing.
func TestLandlockAllowsTheWorkspace(t *testing.T) {
	t.Parallel()
	p := landlockProvider(t)

	sb := openSandbox(t, p, "landlock-allows")
	ctx := testCtx(t)

	res, err := sb.Exec(ctx, Shell("echo written > note.txt && cat note.txt && mkdir -p sub/dir && echo deep > sub/dir/f && cat sub/dir/f"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.OK() {
		t.Fatalf("the agent cannot work in its own workspace: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "written") || !strings.Contains(res.Stdout, "deep") {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

// TestLandlockGivesNoHostCredentials pins the second promise of the backend.
// The filesystem jail would be worth much less if the model could read a
// provider API key out of the environment and use it from a command.
func TestLandlockGivesNoHostCredentials(t *testing.T) {
	p := landlockProvider(t) // not parallel: it sets a process-wide variable

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-never-be-visible")
	t.Setenv("SOME_OTHER_SECRET", "hunter2")

	sb := openSandbox(t, p, "landlock-credentials")
	ctx := testCtx(t)

	res, err := sb.Exec(ctx, Shell("env"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(res.Stdout, "sk-ant-should-never-be-visible") {
		t.Fatal("a provider API key reached a model-chosen command")
	}
	if strings.Contains(res.Stdout, "hunter2") {
		t.Fatal("a host secret reached a model-chosen command")
	}
}

// TestLandlockHidesItsOwnProtocol: the control variables are how the parent
// tells the child what to enforce. A command that could read them would learn
// the jail's shape, and one that could set them on a nested BONNIE binary
// could ask for a wider one.
func TestLandlockHidesItsOwnProtocol(t *testing.T) {
	t.Parallel()
	p := landlockProvider(t)

	sb := openSandbox(t, p, "landlock-protocol")
	ctx := testCtx(t)

	res, err := sb.Exec(ctx, Shell("env"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for _, v := range []string{envLandlockExec, envLandlockRW, envLandlockRO, envLandlockArgv} {
		if strings.Contains(res.Stdout, v) {
			t.Fatalf("the jail protocol variable %s is visible to the command", v)
		}
	}
}

// TestLandlockFileToolsRefuseEscape covers the tools that Landlock does NOT
// cover. read_file and write_file run in the BONNIE process, which must stay
// unrestricted because it owns the journal, so their containment is the path
// jail in host(). Both halves are needed: neither one alone is enough.
func TestLandlockFileToolsRefuseEscape(t *testing.T) {
	t.Parallel()
	p := landlockProvider(t)
	_, secret := secretOutside(t)

	sb := openSandbox(t, p, "landlock-file-tools")
	ctx := testCtx(t)

	for _, p := range []string{
		secret,
		"../../../../etc/passwd",
		"a/../../../etc/passwd",
		"/etc/passwd",
	} {
		if _, err := sb.ReadFile(ctx, p); !errors.Is(err, ErrOutsideWorkspace) {
			t.Fatalf("ReadFile(%q) = %v, want ErrOutsideWorkspace", p, err)
		}
		if err := sb.WriteFile(ctx, p, []byte("x")); !errors.Is(err, ErrOutsideWorkspace) {
			t.Fatalf("WriteFile(%q) = %v, want ErrOutsideWorkspace", p, err)
		}
	}
}

// TestLandlockFileToolsRefuseSymlinkEscape is the subtle case. The agent can
// create a symlink inside its own workspace that points out of it, and a
// naive prefix check on the unresolved path would follow it.
func TestLandlockFileToolsRefuseSymlinkEscape(t *testing.T) {
	t.Parallel()
	p := landlockProvider(t)
	dir, secret := secretOutside(t)

	sb := openSandbox(t, p, "landlock-symlink")
	ctx := testCtx(t)

	ls, ok := sb.(*landlockSandbox)
	if !ok {
		t.Fatalf("unexpected sandbox type %T", sb)
	}
	if err := os.Symlink(secret, filepath.Join(ls.dir, "link-to-secret")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(ls.dir, "link-to-dir")); err != nil {
		t.Fatal(err)
	}

	if _, err := sb.ReadFile(ctx, "link-to-secret"); !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("ReadFile through a symlink = %v, want ErrOutsideWorkspace", err)
	}
	if err := sb.WriteFile(ctx, "link-to-dir/planted.txt", []byte("x")); !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("WriteFile through a symlink = %v, want ErrOutsideWorkspace", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "planted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a write followed a symlink out of the workspace")
	}
}

// TestLandlockRefusesNetworkPolicy pins invariant 10. This backend confines
// the filesystem and not the network, so it must not implement [Networked]:
// accepting a policy it cannot enforce would leave an operator believing they
// are protected.
func TestLandlockRefusesNetworkPolicy(t *testing.T) {
	t.Parallel()
	var p Provider = Landlock()
	if _, ok := p.(Networked); ok {
		t.Fatal("LandlockProvider claims to control the network; it does not, " +
			"and a policy it silently ignores is worse than no policy")
	}
}

// TestLandlockIsAvailableOnThisKernel fails rather than skips on Linux.
//
// A skip is how a sandbox defect hides: docs/SPEC.md §4.11 records three real
// microsandbox bugs that CI never saw because every case skipped. Landlock
// needs nothing installed, so on Linux there is no honest reason to skip, and
// a kernel too old to enforce it must be a loud failure on the machine that
// is meant to be the floor.
func TestLandlockIsAvailableOnThisKernel(t *testing.T) {
	t.Parallel()
	if runtimeGOOS() != "linux" {
		t.Skipf("landlock is a Linux facility; this is %s", runtimeGOOS())
	}
	if err := Landlock().Available(context.Background()); err != nil {
		t.Fatalf("the default sandbox cannot run on this kernel: %v\n"+
			"BONNIE's floor is Landlock, so this host would have no default sandbox", err)
	}
}

// runtimeGOOS is a seam so the test above reads the same on every platform.
func runtimeGOOS() string { return runtime.GOOS }

// TestLandlockDoesNotConfineUnixSockets pins a limitation rather than a
// promise, which is why it asserts nothing about the outcome.
//
// Landlock ABI 1 mediates opening a file. It does NOT mediate connect() to a
// pathname unix socket, so a command that knows a socket's path can talk to
// the daemon behind it even when the path is not in the granted set —
// verified by withholding /var/run entirely and watching the connection
// succeed anyway.
//
// This matters because the daemon may hand out the host. A BONNIE process
// whose user is in the docker group can reach /var/run/docker.sock, and so
// can every tool call: `docker run -v /:/host` reads anything. A live model
// found this vector itself, after every filesystem technique failed, and
// stopped short of using it.
//
// The test exists so the limitation is discovered by reading the suite rather
// than by an incident. It logs what it finds and fails only if the backend
// ever claims to control the network, which is the claim that would make the
// gap dishonest (invariant 10).
func TestLandlockDoesNotConfineUnixSockets(t *testing.T) {
	t.Parallel()

	if _, ok := any(Landlock()).(Networked); ok {
		t.Fatal("LandlockProvider claims network control while unix sockets " +
			"are unmediated: a docker.sock reachable from a tool call is a " +
			"full host escape, so the claim would be false")
	}

	p := landlockProvider(t)
	sb := openSandbox(t, p, "landlock-unix-socket")
	ctx := testCtx(t)

	const sock = "/var/run/docker.sock"
	if _, err := os.Stat(sock); err != nil {
		t.Skipf("no docker socket on this host: %v", err)
	}

	res, err := sb.Exec(ctx, Shell(
		"curl -s --max-time 5 --unix-socket "+sock+
			" http://localhost/containers/json 2>&1 | head -c 60"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(res.Stdout, `"Id"`) || strings.Contains(res.Stdout, "Names") {
		t.Logf("CONFIRMED LIMITATION: a jailed command reached %s. "+
			"Landlock does not mediate unix sockets; run BONNIE as a user "+
			"outside the docker group, or use microsandbox. Output: %q",
			sock, strings.TrimSpace(res.Stdout))
	} else {
		t.Logf("the socket was not reachable here (output %q); the limitation "+
			"still stands on a host whose user is in the docker group",
			strings.TrimSpace(res.Stdout))
	}
}
