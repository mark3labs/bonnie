package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// The environment variables that carry a jailed command from the parent
// process to the re-executed child. They are an internal protocol, not an
// interface a host configures: the child strips them before it hands control
// to the model's command, so a tool call never sees them.
const (
	envLandlockExec = "BONNIE_LANDLOCK_EXEC"
	envLandlockRW   = "BONNIE_LANDLOCK_RW"
	envLandlockRO   = "BONNIE_LANDLOCK_RO"
	envLandlockArgv = "BONNIE_LANDLOCK_ARGV"
)

// landlockProtocol versions the handshake above. A binary that re-execs
// itself is always the same build, so a mismatch means something else set the
// variable and the child refuses rather than guessing.
const landlockProtocol = "1"

// systemPaths are the host directories a command needs in order to run at
// all: the shell, the C library, and the resolver configuration. They are
// granted read-only, and every one is optional — a distribution that does not
// have /lib64, or a NixOS host that keeps its tools in /nix/store, must not
// fail to start a shell.
//
// /run and /var/run are deliberately absent. /var/run is a symlink to /run on
// most distributions, so granting it exposes the whole runtime tree — pid
// files, session state, and every daemon socket. Nothing a shell needs lives
// there. Note that withholding it does NOT stop a command connecting to a
// socket whose path it knows: see [landlockSandbox.Exec] and
// TestLandlockDoesNotConfineUnixSockets.
var systemPaths = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
	"/etc", "/opt", "/nix/store", "/run/current-system",
	"/proc", "/sys/devices/system/cpu",
}

// deviceFiles are the character devices a shell pipeline needs to write to.
// /dev as a whole is deliberately not granted: a command has no business in
// /dev/mem or a raw disk.
var deviceFiles = []string{
	"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty",
}

// LandlockProvider confines tool calls to one directory per run using the
// Linux Landlock LSM, with no daemon, no image, and no root.
//
// # What it contains, and what it does not
//
// It is CONTAINMENT, NOT ISOLATION, and the difference is not pedantic:
//
//   - The filesystem IS confined. A command, and every process it starts, can
//     read and write only the run's workspace plus the read-only system paths
//     a shell needs. An absolute path out of the workspace is refused by the
//     kernel, which is what separates this from [LocalProvider].
//   - Host credentials are NOT passed. The child gets a minimal environment,
//     so a provider API key in the server's environment does not reach a
//     model-chosen command.
//   - The network is NOT confined. A command can reach anything this host can
//     reach. The provider therefore does not implement [Networked], so
//     asking it for a network policy is refused rather than silently ignored.
//   - **A unix socket is NOT confined, and this is the sharpest edge.**
//     Landlock ABI 1 mediates opening a file, not connecting to a socket, so
//     a command that knows a socket's path can talk to the daemon behind it
//     even though the path is not granted. If the BONNIE process can reach
//     /var/run/docker.sock — which it can whenever its user is in the docker
//     group — then so can a tool call, and `docker run -v /:/host` is a
//     complete escape. Found by a live model, which reported the vector
//     itself after every filesystem attempt failed. Run BONNIE as a user
//     that holds no such membership, or use [MicrosandboxProvider].
//   - The process table, the kernel, and every other namespace are shared.
//     A local privilege-escalation bug in the kernel is not contained.
//
// For hostile code, use [MicrosandboxProvider] (a microVM with its own
// kernel) or [DockerProvider] (namespaces). This backend exists so that the
// default configuration on a bare machine is confined rather than open: it
// needs nothing installed, so "no sandbox" never has to be the convenient
// choice.
//
// # How it works
//
// Landlock restricts the calling process irreversibly, so BONNIE cannot apply
// it to the server — that would take away the journal. Each command is run in
// a child that re-executes this same binary, applies the restriction to
// itself, and only then replaces itself with the command. The restriction
// survives that exec and is inherited by every descendant, so a subshell
// cannot escape what its parent accepted.
type LandlockProvider struct {
	root string

	mu      sync.Mutex
	opened  map[string]*landlockSandbox
	cleanup bool
}

var _ Provider = (*LandlockProvider)(nil)

// LandlockOption configures a [LandlockProvider].
type LandlockOption func(*LandlockProvider)

// WithLandlockRoot sets the directory that holds per-run workspaces. The
// default is ".bonnie/workspaces".
func WithLandlockRoot(dir string) LandlockOption {
	return func(p *LandlockProvider) { p.root = dir }
}

// WithLandlockCleanup removes a run's workspace when its sandbox is deleted.
// Tests use it; a real run wants its files to survive.
func WithLandlockCleanup() LandlockOption {
	return func(p *LandlockProvider) { p.cleanup = true }
}

// Landlock returns a provider that confines each run to its own directory
// with the Linux Landlock LSM. Read the limits on [LandlockProvider] before
// using it: it confines the filesystem, not the network.
func Landlock(opts ...LandlockOption) *LandlockProvider {
	p := &LandlockProvider{
		root:   filepath.Join(".bonnie", "workspaces"),
		opened: make(map[string]*landlockSandbox),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name implements [Provider].
func (p *LandlockProvider) Name() string { return "landlock" }

// Available implements [Provider]. It asks the kernel for its Landlock ABI
// version rather than assuming: the LSM can be compiled out or disabled at
// boot, and a backend that cannot enforce its promise must say so before the
// first tool call, not after.
func (p *LandlockProvider) Available(context.Context) error {
	if err := landlockSupported(); err != nil {
		return err
	}
	return lookPath("sh")
}

// Open implements [Provider].
func (p *LandlockProvider) Open(_ context.Context, runID string) (Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if sb, ok := p.opened[runID]; ok && !sb.isClosed() {
		return sb, nil
	}

	dir, err := filepath.Abs(filepath.Join(p.root, safeName("", runID)))
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: resolve workspace: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: create workspace: %w", err)
	}

	// The temporary directory is a sibling of the workspace, not a child:
	// a command needs somewhere to write scratch files, and the model must
	// not have to look at them when it lists its own workspace.
	tmp := dir + ".tmp"
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: create scratch directory: %w", err)
	}

	sb := &landlockSandbox{id: runID, dir: dir, tmp: tmp, cleanup: p.cleanup}
	p.opened[runID] = sb
	return sb, nil
}

// landlockSandbox is one confined host directory.
type landlockSandbox struct {
	id      string
	dir     string
	tmp     string
	cleanup bool

	mu     sync.RWMutex
	closed bool
}

var (
	_ Sandbox = (*landlockSandbox)(nil)
	_ Deleter = (*landlockSandbox)(nil)
)

// ID implements [Sandbox].
func (s *landlockSandbox) ID() string { return s.id }

func (s *landlockSandbox) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// host maps a sandbox path onto the host directory that backs it, refusing
// anything that would leave the workspace.
//
// This is the jail for the file tools, which run in the BONNIE process and so
// are not covered by the kernel restriction applied to a command's child.
// [Exec] is confined by Landlock instead, because no amount of string
// inspection can contain a shell.
func (s *landlockSandbox) host(p string) (string, error) {
	resolved := Resolve(p)

	var rel string
	switch {
	case resolved == Workspace:
		rel = ""
	case strings.HasPrefix(resolved, Workspace+"/"):
		rel = resolved[len(Workspace)+1:]
	default:
		// Resolve has already cleaned the path, so a "../" escape arrives
		// here as an absolute path outside the workspace.
		return "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, p)
	}

	target := filepath.Join(s.dir, filepath.FromSlash(rel))
	if err := s.within(target); err != nil {
		return "", err
	}
	return target, nil
}

// within refuses a path that leaves the workspace once symlinks are resolved.
//
// The target itself usually does not exist yet — a write creates it — so the
// nearest ancestor that does exist is the one resolved. That is the link that
// could point outside; a component that does not exist cannot.
func (s *landlockSandbox) within(target string) error {
	root, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		return fmt.Errorf("bonnie: sandbox: resolve workspace: %w", err)
	}

	probe := target
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
				return fmt.Errorf("%w: %s resolves outside the workspace", ErrOutsideWorkspace, target)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("bonnie: sandbox: resolve %s: %w", target, err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("%w: %s", ErrOutsideWorkspace, target)
		}
		probe = parent
	}
}

// Exec implements [Sandbox]. The command runs in a child that confines itself
// with Landlock before becoming the command, so the restriction covers the
// command and everything it starts.
func (s *landlockSandbox) Exec(ctx context.Context, cmd Command) (*Result, error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	if err := cmd.validate(); err != nil {
		return nil, err
	}

	dir, err := s.host(cmd.workdir())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: create workdir: %w", err)
	}

	return s.execJailed(ctx, cmd, dir)
}

// childEnv is the environment a jailed command receives.
//
// It is built from nothing rather than filtered from the host's, which is the
// only version of this that stays correct: a deny-list has to be updated
// every time a provider invents a new variable name, and the one it misses is
// the one that leaks. PATH is inherited because a command cannot find a shell
// without it, and it names directories, not secrets.
func (s *landlockSandbox) childEnv(extra []string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + s.dir,
		"TMPDIR=" + s.tmp,
		"PWD=" + s.dir,
		"SHELL=/bin/sh",
	}
	for _, keep := range []string{"LANG", "LC_ALL", "TZ", "TERM"} {
		if v := os.Getenv(keep); v != "" {
			env = append(env, keep+"="+v)
		}
	}
	return append(env, extra...)
}

// jailSpec is what the parent tells the child to enforce.
type jailSpec struct {
	rw   []string
	ro   []string
	argv []string
}

// env renders the spec as the control variables the child reads.
func (j jailSpec) env() ([]string, error) {
	rw, err := json.Marshal(j.rw)
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: encode jail: %w", err)
	}
	ro, err := json.Marshal(j.ro)
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: encode jail: %w", err)
	}
	argv, err := json.Marshal(j.argv)
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: encode command: %w", err)
	}
	return []string{
		envLandlockExec + "=" + landlockProtocol,
		envLandlockRW + "=" + string(rw),
		envLandlockRO + "=" + string(ro),
		envLandlockArgv + "=" + string(argv),
	}, nil
}

// runChild runs the prepared child process and turns its outcome into a
// [Result]. It keeps the same contract every backend keeps: a command that
// ran and failed is a Result with a non-zero exit code, and only a failure to
// run it at all is an error.
func runChild(ctx context.Context, c *exec.Cmd, cmd Command) (*Result, error) {
	if len(cmd.Stdin) > 0 {
		c.Stdin = bytes.NewReader(cmd.Stdin)
	}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb

	err := c.Run()
	res := &Result{Stdout: out.String(), Stderr: errb.String()}

	var ee *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		// The command ran and failed. That is a result, not an error.
		res.ExitCode = ee.ExitCode()
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("bonnie: sandbox: command timed out after %s: %w", cmd.Timeout, ctx.Err())
	default:
		return nil, fmt.Errorf("bonnie: sandbox: exec: %w", err)
	}
	return res, nil
}

// ReadFile implements [Sandbox].
func (s *landlockSandbox) ReadFile(_ context.Context, p string) ([]byte, error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	target, err := s.host(p)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: read %s: %w", p, err)
	}
	return data, nil
}

// WriteFile implements [Sandbox].
func (s *landlockSandbox) WriteFile(_ context.Context, p string, data []byte) error {
	if s.isClosed() {
		return ErrClosed
	}
	target, err := s.host(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("bonnie: sandbox: create parent of %s: %w", p, err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return fmt.Errorf("bonnie: sandbox: write %s: %w", p, err)
	}
	return nil
}

// Stop implements [Sandbox]. A jailed command is an ordinary child process
// that has already exited, so nothing is held between commands and the
// workspace is untouched.
func (s *landlockSandbox) Stop(context.Context) error { return nil }

// Close implements [Sandbox].
func (s *landlockSandbox) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

// Delete implements [Deleter]. It removes the workspace only when the
// provider was built with [WithLandlockCleanup], because deleting a
// developer's files by surprise is worse than leaving them.
func (s *landlockSandbox) Delete(context.Context) error {
	_ = s.Close()
	if !s.cleanup {
		return nil
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return fmt.Errorf("bonnie: sandbox: delete workspace: %w", err)
	}
	if err := os.RemoveAll(s.tmp); err != nil {
		return fmt.Errorf("bonnie: sandbox: delete scratch directory: %w", err)
	}
	return nil
}

// SandboxExists implements [ExistenceChecker]. A workspace is a directory
// under the provider root.
func (p *LandlockProvider) SandboxExists(_ context.Context, runID string) (bool, error) {
	dir := filepath.Join(p.root, safeName("", runID))
	_, err := os.Stat(dir)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("bonnie: sandbox: stat workspace: %w", err)
	}
}

// DeleteRun implements [RunDeleter].
func (p *LandlockProvider) DeleteRun(_ context.Context, runID string) (bool, error) {
	dir := filepath.Join(p.root, safeName("", runID))
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return false, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return true, fmt.Errorf("bonnie: sandbox: remove workspace: %w", err)
	}
	_ = os.RemoveAll(dir + ".tmp")
	return true, nil
}

var (
	_ ExistenceChecker = (*LandlockProvider)(nil)
	_ RunDeleter       = (*LandlockProvider)(nil)
)

// WorkingDir implements [WorkingDirReporter]. A landlock sandbox is a host
// directory, so that path — not [Workspace] — is what `pwd` reports and what
// the system prompt must name.
func (p *LandlockProvider) WorkingDir(runID string) string {
	dir, err := filepath.Abs(filepath.Join(p.root, safeName("", runID)))
	if err != nil {
		return filepath.Join(p.root, safeName("", runID))
	}
	return dir
}

var _ WorkingDirReporter = (*LandlockProvider)(nil)
