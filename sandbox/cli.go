package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// cliSandbox is the shared body of every CLI-driven backend. Docker and
// microsandbox differ only in how they build an argv and manage a lifecycle,
// so those parts are function fields and everything else is common.
type cliSandbox struct {
	id      string
	name    string
	bin     string
	backend string

	// execArgs builds the CLI arguments that run argv inside the sandbox.
	execArgs func(c *cliSandbox, argv []string, cmd Command) []string
	// startFn makes sure the sandbox is running before a command.
	startFn func(ctx context.Context, c *cliSandbox) error
	// stopFn releases compute but keeps state.
	stopFn func(ctx context.Context, c *cliSandbox) error
	// deleteFn destroys the sandbox.
	deleteFn func(ctx context.Context, c *cliSandbox) error
	// readFn and writeFn move file bytes. When nil, the sandbox falls back
	// to `cat` through exec, which every POSIX image supports.
	readFn  func(ctx context.Context, c *cliSandbox, path string) ([]byte, error)
	writeFn func(ctx context.Context, c *cliSandbox, path string, data []byte) error

	mu     sync.RWMutex
	closed bool
	// stopped tracks whether compute was released, so the next command can
	// bring the sandbox back without the caller having to know.
	stopped bool
}

var (
	_ Sandbox = (*cliSandbox)(nil)
	_ Deleter = (*cliSandbox)(nil)
)

// ID implements [Sandbox].
func (c *cliSandbox) ID() string { return c.id }

func (c *cliSandbox) isClosed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.closed
}

// ensure restarts the sandbox when a previous Stop released it.
func (c *cliSandbox) ensure(ctx context.Context) error {
	if c.isClosed() {
		return ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped && c.startFn == nil {
		return nil
	}
	if err := c.startFn(ctx, c); err != nil {
		return err
	}
	c.stopped = false
	return nil
}

// Exec implements [Sandbox].
func (c *cliSandbox) Exec(ctx context.Context, cmd Command) (*Result, error) {
	if err := cmd.validate(); err != nil {
		return nil, err
	}
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}

	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}

	argv, nonce, err := execWithMarker(cmd)
	if err != nil {
		return nil, err
	}

	stdout, stderr, code, err := runCLI(ctx, cmd.Stdin, c.bin, c.execArgs(c, argv, cmd)...)
	if err != nil {
		return nil, err
	}

	// The marker is the only trustworthy exit code. Without it the CLI
	// failed before the guest command ran, and the CLI's own exit code
	// would be reported to the model as if the command had failed.
	clean, guestCode, found := parseMarker(stdout, nonce)
	if !found {
		return nil, fmt.Errorf("bonnie: sandbox: %s exec failed before the command ran (exit %d): %s",
			c.backend, code, firstLine(stderr))
	}
	return &Result{ExitCode: guestCode, Stdout: clean, Stderr: stderr}, nil
}

// ReadFile implements [Sandbox].
func (c *cliSandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	if c.readFn != nil {
		return c.readFn(ctx, c, Resolve(p))
	}

	// No marker here: it would corrupt binary content. `cat` exits non-zero
	// for a missing file, which is enough to tell the two cases apart.
	args := c.execArgs(c, []string{"cat", "--", Resolve(p)}, Command{})
	data, stderr, code, err := runCLIRaw(ctx, nil, c.bin, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		if isMissingPath(stderr) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
		}
		return nil, fmt.Errorf("bonnie: sandbox: read %s: %s", p, firstLine(stderr))
	}
	return data, nil
}

// WriteFile implements [Sandbox].
func (c *cliSandbox) WriteFile(ctx context.Context, p string, data []byte) error {
	if err := c.ensure(ctx); err != nil {
		return err
	}
	if c.writeFn != nil {
		return c.writeFn(ctx, c, Resolve(p), data)
	}

	target := Resolve(p)
	dir := parentDir(target)
	// One shell does both steps, so a write never half-succeeds with a
	// created directory and no file.
	script := fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(dir), shellQuote(target))
	args := c.execArgs(c, []string{"sh", "-c", script}, Command{})
	_, stderr, code, err := runCLIRaw(ctx, data, c.bin, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("bonnie: sandbox: write %s: %s", p, firstLine(stderr))
	}
	return nil
}

// Stop implements [Sandbox]. The workspace survives; the next command starts
// the sandbox again.
func (c *cliSandbox) Stop(ctx context.Context) error {
	if c.isClosed() {
		return ErrClosed
	}
	if c.stopFn == nil {
		return nil
	}
	if err := c.stopFn(ctx, c); err != nil {
		return fmt.Errorf("bonnie: sandbox: stop %s: %w", c.name, err)
	}
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	return nil
}

// Close implements [Sandbox]. It drops the handle and leaves the sandbox
// alone, so a later Open reattaches.
func (c *cliSandbox) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

// Delete implements [Deleter].
func (c *cliSandbox) Delete(ctx context.Context) error {
	if c.deleteFn != nil {
		if err := c.deleteFn(ctx, c); err != nil {
			return fmt.Errorf("bonnie: sandbox: delete %s: %w", c.name, err)
		}
	}
	return c.Close()
}

// isMissingPath recognises the shapes a shell uses to report an absent file.
func isMissingPath(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such file") || strings.Contains(s, "not found")
}

// parentDir returns the directory part of a slash path.
func parentDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "/"
}

// shellQuote wraps a string in single quotes for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
