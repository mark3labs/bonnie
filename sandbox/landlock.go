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
	root         string
	shared       bool
	sharedActive bool

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

// UseSharedWorkspace configures one exact host directory as the shared workspace.
// Shared runs cannot overlap and shared data is never removed by sandbox cleanup.
func (p *LandlockProvider) UseSharedWorkspace(dir string) error {
	if dir == "" {
		return fmt.Errorf("bonnie: sandbox: shared workspace path is empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.opened) != 0 {
		return fmt.Errorf("bonnie: sandbox: cannot enable shared workspace after opening a run")
	}
	p.root, p.shared, p.cleanup = dir, true, false
	return nil
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

	if p.shared && p.sharedActive {
		return nil, fmt.Errorf("bonnie: sandbox: shared workspace is already in use")
	}
	if !p.shared {
		if err := refuseUncheckedLegacy(p.root, runID); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(p.root, safeName("", runID))
	name := safeName("", runID)
	if p.shared {
		path = p.root
		name = filepath.Base(filepath.Clean(p.root))
	}
	dir, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: resolve workspace: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: create provider root: %w", err)
	}
	providerRoot, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: open provider root: %w", err)
	}
	defer func() { _ = providerRoot.Close() }()
	if err := providerRoot.MkdirAll(name, 0o700); err != nil {
		return nil, rootError(name, err)
	}
	if err := refuseRootLink(providerRoot, name); err != nil {
		return nil, err
	}
	root, err := providerRoot.OpenRoot(name)
	if err != nil {
		return nil, rootError(name, err)
	}
	// Scratch has its own namespace. Run names cannot start with a dot.
	tmpRel := filepath.Join(".scratch", safeName("", runID))
	if err := providerRoot.MkdirAll(tmpRel, 0o700); err != nil {
		_ = root.Close()
		return nil, rootError(tmpRel, err)
	}
	// Refuse aliases to another run inside the provider root too.
	if err := refuseRootLink(providerRoot, ".scratch"); err != nil {
		_ = root.Close()
		return nil, err
	}
	if err := refuseRootLink(providerRoot, tmpRel); err != nil {
		_ = root.Close()
		return nil, err
	}
	// Verify the scratch directory is under the same provider root too.
	scratch, err := providerRoot.OpenRoot(tmpRel)
	if err != nil {
		_ = root.Close()
		return nil, rootError(tmpRel, err)
	}
	// Keep both directory identities open for the child restriction.
	tmp := filepath.Join(filepath.Dir(dir), tmpRel)

	sb := &landlockSandbox{id: runID, dir: dir, tmp: tmp, root: root, scratch: scratch, cleanup: p.cleanup, release: func() { p.mu.Lock(); p.sharedActive = false; p.mu.Unlock() }}
	if p.shared {
		p.sharedActive = true
	}
	p.opened[runID] = sb
	return sb, nil
}

// landlockSandbox is one confined host directory.
type landlockSandbox struct {
	id       string
	dir      string
	root     *os.Root
	scratch  *os.Root
	tmp      string
	cleanup  bool
	release  func()
	released bool

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

// relative maps a file-tool path into os.Root. The root checks symlinks during
// the operation, including missing targets and concurrent path changes.
func (s *landlockSandbox) relative(p string) (string, error) {
	resolved := Resolve(p)
	if resolved == Workspace {
		return ".", nil
	}
	if rel, ok := strings.CutPrefix(resolved, Workspace+"/"); ok {
		return filepath.FromSlash(rel), nil
	}
	return "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, p)
}

// refuseRootLink prevents one run from adopting another run's directory.
func refuseRootLink(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if err != nil {
		return rootError(name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: workspace alias %s", ErrOutsideWorkspace, name)
	}
	return nil
}

func rootError(p string, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "escapes from parent") || strings.Contains(err.Error(), "outside root") {
		return fmt.Errorf("%w: %s: %v", ErrOutsideWorkspace, p, err)
	}
	return fmt.Errorf("bonnie: sandbox: file %s: %w", p, err)
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

	rel, err := s.relative(cmd.workdir())
	if err != nil {
		return nil, err
	}
	if err := s.root.MkdirAll(rel, 0o700); err != nil {
		return nil, rootError(rel, err)
	}
	dir, err := s.root.Open(rel)
	if err != nil {
		return nil, rootError(rel, err)
	}
	defer func() { _ = dir.Close() }()
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

	configureProcess(c)
	err := c.Run()
	res := &Result{Stdout: out.String(), Stderr: errb.String()}

	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("bonnie: sandbox: command canceled: %w", ctx.Err())
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		// The command ran and failed. That is a result, not an error.
		res.ExitCode = ee.ExitCode()
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
	target, err := s.relative(p)
	if err != nil {
		return nil, err
	}
	data, err := s.root.ReadFile(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	if err != nil {
		return nil, rootError(p, err)
	}
	return data, nil
}

// WriteFile implements [Sandbox].
func (s *landlockSandbox) WriteFile(_ context.Context, p string, data []byte) error {
	if s.isClosed() {
		return ErrClosed
	}
	target, err := s.relative(p)
	if err != nil {
		return err
	}
	if err := s.root.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return rootError(p, err)
	}
	if err := s.root.WriteFile(target, data, 0o600); err != nil {
		return rootError(p, err)
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
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	release := s.release
	if release != nil && !s.released {
		s.released = true
	}
	s.mu.Unlock()
	if release != nil {
		release()
	}
	return errors.Join(s.root.Close(), s.scratch.Close())
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
	if err := removeScratch(filepath.Dir(s.dir), safeName("", s.id)); err != nil {
		return fmt.Errorf("bonnie: sandbox: delete scratch directory: %w", err)
	}
	return nil
}

// removeScratch must not follow a replaced .scratch link into host files.
func removeScratch(providerDir, name string) error {
	root, err := os.OpenRoot(providerDir)
	if err != nil {
		return fmt.Errorf("bonnie: sandbox: open provider root: %w", err)
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Lstat(".scratch"); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := refuseRootLink(root, ".scratch"); err != nil {
		return err
	}
	return rootError(name, root.RemoveAll(filepath.Join(".scratch", name)))
}

// SandboxExists implements [ExistenceChecker]. A workspace is a directory
// under the provider root.
func (p *LandlockProvider) SandboxExists(_ context.Context, runID string) (bool, error) {
	p.mu.Lock()
	shared := p.shared
	root := p.root
	p.mu.Unlock()
	if shared {
		_, err := os.Stat(root)
		if err == nil {
			return true, nil
		}
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("bonnie: sandbox: stat workspace: %w", err)
	}
	if err := refuseUncheckedLegacy(root, runID); err != nil {
		return false, err
	}
	dir := filepath.Join(root, safeName("", runID))
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
	p.mu.Lock()
	shared := p.shared
	p.mu.Unlock()
	if shared {
		return false, nil
	}
	if err := refuseUncheckedLegacy(p.root, runID); err != nil {
		return false, err
	}
	dir := filepath.Join(p.root, safeName("", runID))
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("bonnie: sandbox: stat workspace: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return true, fmt.Errorf("bonnie: sandbox: remove workspace: %w", err)
	}
	if err := removeScratch(p.root, safeName("", runID)); err != nil {
		return true, fmt.Errorf("bonnie: sandbox: remove scratch: %w", err)
	}
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
	p.mu.Lock()
	shared, root := p.shared, p.root
	p.mu.Unlock()
	path := filepath.Join(root, safeName("", runID))
	if shared {
		path = root
	}
	dir, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return dir
}

var _ WorkingDirReporter = (*LandlockProvider)(nil)
