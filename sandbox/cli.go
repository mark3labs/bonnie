package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
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
	// killFn stops all guest processes after cancellation. Killing the host
	// CLI alone does not stop guest work.
	killFn func(context.Context, *cliSandbox) error
	// readFn and writeFn move file bytes. When nil, the sandbox falls back
	// to `cat` through exec, which every POSIX image supports.
	readFn  func(ctx context.Context, c *cliSandbox, path string) ([]byte, error)
	writeFn func(ctx context.Context, c *cliSandbox, path string, data []byte) error

	opOnce sync.Once
	opGate chan struct{}

	mu     sync.RWMutex
	closed bool
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

// operation prevents a restart while cancellation stops guest work. Waiting
// callers can cancel without waiting for the active command to finish.
func (c *cliSandbox) operation(ctx context.Context) (func(), error) {
	c.opOnce.Do(func() { c.opGate = make(chan struct{}, 1) })
	select {
	case c.opGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-c.opGate
			return nil, err
		}
		return func() { <-c.opGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ensure checks the live sandbox on each operation.
func (c *cliSandbox) ensure(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if c.startFn == nil {
		return nil
	}
	if err := c.startFn(ctx, c); err != nil {
		return err
	}
	return nil
}

// Exec implements [Sandbox].
func (c *cliSandbox) Exec(ctx context.Context, cmd Command) (*Result, error) {
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}

	release, err := c.operation(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := cmd.validate(); err != nil {
		return nil, err
	}
	if c.killFn == nil {
		return nil, fmt.Errorf("%w: %s cannot stop guest work on cancellation", ErrUnavailable, c.backend)
	}
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}

	argv, nonce, err := execWithMarker(cmd)
	if err != nil {
		return nil, err
	}

	stdout, stderr, code, err := runCLI(ctx, cmd.Stdin, c.bin, c.execArgs(c, argv, cmd)...)
	if ctx.Err() != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		killErr := c.killFn(cleanupCtx, c)
		return nil, errors.Join(fmt.Errorf("bonnie: sandbox: command canceled: %w", ctx.Err()), killErr)
	}
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
	release, err := c.operation(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
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
	release, err := c.operation(ctx)
	if err != nil {
		return err
	}
	defer release()
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
	release, err := c.operation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if c.isClosed() {
		return ErrClosed
	}
	if c.stopFn == nil {
		return nil
	}
	if err := c.stopFn(ctx, c); err != nil {
		return fmt.Errorf("bonnie: sandbox: stop %s: %w", c.name, err)
	}
	return nil
}

// Close implements [Sandbox]. It drops the handle and leaves the sandbox
// alone, so a later Open reattaches.
func (c *cliSandbox) Close() error {
	release, err := c.operation(context.Background())
	if err != nil {
		return err
	}
	defer release()
	return c.closeHandle()
}

func (c *cliSandbox) closeHandle() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

// Delete implements [Deleter].
func (c *cliSandbox) Delete(ctx context.Context) error {
	release, err := c.operation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if c.deleteFn != nil {
		if err := c.deleteFn(ctx, c); err != nil {
			return fmt.Errorf("bonnie: sandbox: delete %s: %w", c.name, err)
		}
	}
	return c.closeHandle()
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
