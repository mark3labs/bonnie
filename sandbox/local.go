package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// LocalProvider runs commands as the host process, in a directory per run.
//
// It provides NO ISOLATION. A command can read any file the BONNIE process can
// read, reach any network the host can reach, and see every environment
// variable, including provider API keys. It exists so a developer can work
// without Docker, and so the sandbox seam is testable with no daemon.
//
// Do not use it in production. Use [DockerProvider] or
// [MicrosandboxProvider], both of which are real. The distinction is not
// theoretical: BONNIE's own live-model test once ran with host tools and the
// model wrote Terraform files into the repository. See docs/SPEC.md §4.9.
type LocalProvider struct {
	root string

	mu      sync.Mutex
	opened  map[string]*localSandbox
	cleanup bool
}

var _ Provider = (*LocalProvider)(nil)

// LocalOption configures a [LocalProvider].
type LocalOption func(*LocalProvider)

// WithLocalRoot sets the directory that holds per-run workspaces. The default
// is ".bonnie/workspaces".
func WithLocalRoot(dir string) LocalOption {
	return func(p *LocalProvider) { p.root = dir }
}

// WithLocalCleanup removes a run's workspace when its sandbox is deleted.
// Tests use it; a real run wants its files to survive.
func WithLocalCleanup() LocalOption {
	return func(p *LocalProvider) { p.cleanup = true }
}

// Local returns a provider that runs commands on the host with no isolation.
// Read the warning on [LocalProvider] before using it.
func Local(opts ...LocalOption) *LocalProvider {
	p := &LocalProvider{
		root:   filepath.Join(".bonnie", "workspaces"),
		opened: make(map[string]*localSandbox),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name implements [Provider].
func (p *LocalProvider) Name() string { return "local" }

// Available implements [Provider]. The local backend needs only a shell.
func (p *LocalProvider) Available(context.Context) error { return lookPath("sh") }

// Open implements [Provider].
func (p *LocalProvider) Open(_ context.Context, runID string) (Sandbox, error) {
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

	sb := &localSandbox{id: runID, dir: dir, cleanup: p.cleanup}
	p.opened[runID] = sb
	return sb, nil
}

// localSandbox is one host directory standing in for a sandbox.
type localSandbox struct {
	id      string
	dir     string
	cleanup bool

	mu     sync.RWMutex
	closed bool
}

var (
	_ Sandbox = (*localSandbox)(nil)
	_ Deleter = (*localSandbox)(nil)
)

// ID implements [Sandbox].
func (s *localSandbox) ID() string { return s.id }

func (s *localSandbox) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// host maps a sandbox path onto the host directory that backs it.
//
// A path under [Workspace] becomes a path under the run's directory. Anything
// else is used as given, which is the honest behaviour for a backend with no
// isolation: pretending otherwise would suggest a containment this provider
// does not have.
func (s *localSandbox) host(p string) string {
	resolved := Resolve(p)
	if rel, ok := strings.CutPrefix(resolved, Workspace); ok {
		return filepath.Join(s.dir, filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	}
	return filepath.FromSlash(resolved)
}

// Exec implements [Sandbox].
func (s *localSandbox) Exec(ctx context.Context, cmd Command) (*Result, error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	if err := cmd.validate(); err != nil {
		return nil, err
	}

	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}

	c := exec.CommandContext(ctx, cmd.Args[0], cmd.Args[1:]...)
	c.Dir = s.host(cmd.workdir())
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: create workdir: %w", err)
	}
	c.Env = append(os.Environ(), cmd.Env...)
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
func (s *localSandbox) ReadFile(_ context.Context, p string) ([]byte, error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	data, err := os.ReadFile(s.host(p))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	if err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: read %s: %w", p, err)
	}
	return data, nil
}

// WriteFile implements [Sandbox].
func (s *localSandbox) WriteFile(_ context.Context, p string, data []byte) error {
	if s.isClosed() {
		return ErrClosed
	}
	target := s.host(p)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("bonnie: sandbox: create parent of %s: %w", p, err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return fmt.Errorf("bonnie: sandbox: write %s: %w", p, err)
	}
	return nil
}

// Stop implements [Sandbox]. The local backend holds no compute between
// commands, so there is nothing to release and the workspace is untouched.
func (s *localSandbox) Stop(context.Context) error { return nil }

// Close implements [Sandbox].
func (s *localSandbox) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

// Delete implements [Deleter]. It removes the workspace only when the provider
// was built with [WithLocalCleanup], because deleting a developer's files by
// surprise is worse than leaving them.
func (s *localSandbox) Delete(context.Context) error {
	_ = s.Close()
	if !s.cleanup {
		return nil
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return fmt.Errorf("bonnie: sandbox: delete workspace: %w", err)
	}
	return nil
}

// WorkingDir implements [WorkingDirReporter]. A local sandbox is a host
// directory, so that path — not [Workspace] — is what `pwd` reports and what
// the system prompt must name.
func (p *LocalProvider) WorkingDir(runID string) string {
	dir, err := filepath.Abs(filepath.Join(p.root, safeName("", runID)))
	if err != nil {
		return filepath.Join(p.root, safeName("", runID))
	}
	return dir
}

var _ WorkingDirReporter = (*LocalProvider)(nil)
