package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// ExistenceChecker is implemented by a [Provider] that can report whether a
// run's sandbox still exists, without opening one. Opening would create, and
// the whole point of the question is whether the old work directory is gone.
//
// The sandbox.Agent factory uses it on resume: a run whose recorded sandbox
// is gone gets a note in its conversation, so the model knows its files
// vanished instead of finding an empty directory and guessing.
type ExistenceChecker interface {
	SandboxExists(ctx context.Context, runID string) (bool, error)
}

// RunDeleter is implemented by a [Provider] that can delete the sandbox of a
// run without opening it. A reconciler needs exactly this: Open would create
// the sandbox it is about to delete.
type RunDeleter interface {
	// DeleteRun removes the sandbox of a run and reports whether it was
	// there. Deleting an absent sandbox is not an error — the goal is that
	// it is gone.
	DeleteRun(ctx context.Context, runID string) (existed bool, err error)
}

// RunCleanupValidator checks whether a provider's current mode supports
// per-run cleanup. Call it before cleanup starts, even when no runs exist.
// Shared work directories cannot be deleted for one run without affecting others.
type RunCleanupValidator interface {
	ValidateRunCleanup() error
}

// ValidateRunCleanup implements [RunCleanupValidator].
func (p *DockerProvider) ValidateRunCleanup() error { return nil }

// ValidateRunCleanup implements [RunCleanupValidator].
func (p *MicrosandboxProvider) ValidateRunCleanup() error { return nil }

// ValidateRunCleanup implements [RunCleanupValidator].
func (p *LocalProvider) ValidateRunCleanup() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shared {
		return fmt.Errorf("bonnie: sandbox: shared work directory does not support per-run cleanup")
	}
	return nil
}

// ValidateRunCleanup implements [RunCleanupValidator].
func (p *LandlockProvider) ValidateRunCleanup() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shared {
		return fmt.Errorf("bonnie: sandbox: shared work directory does not support per-run cleanup")
	}
	return nil
}

// validateRunCleanup checks the backend capability before forwarding its mode
// check. Wrappers expose DeleteRun even when their backend cannot delete.
func validateRunCleanup(p Provider) error {
	if _, ok := p.(RunDeleter); !ok {
		return fmt.Errorf("bonnie: sandbox: the %s backend cannot delete sandboxes", p.Name())
	}
	if v, ok := p.(RunCleanupValidator); ok {
		return v.ValidateRunCleanup()
	}
	return nil
}

// SandboxExists implements [ExistenceChecker].
func (p *DockerProvider) SandboxExists(ctx context.Context, runID string) (bool, error) {
	p.policyMu.RLock()
	defer p.policyMu.RUnlock()
	_, state, err := p.locate(ctx, runID)
	if err != nil {
		return false, err
	}
	return state != "", nil
}

// DeleteRun implements [RunDeleter].
func (p *DockerProvider) DeleteRun(ctx context.Context, runID string) (bool, error) {
	p.policyMu.RLock()
	defer p.policyMu.RUnlock()
	name, state, err := p.locate(ctx, runID)
	if err != nil {
		return false, err
	}
	if state == "" {
		return false, nil
	}
	_, stderr, code, rerr := runCLI(ctx, nil, p.bin, "rm", "--force", name)
	if cerr := cliError("rm "+name, firstLine(stderr), code, rerr); cerr != nil {
		return true, cerr
	}
	return true, nil
}

// SandboxExists implements [ExistenceChecker].
func (p *MicrosandboxProvider) SandboxExists(ctx context.Context, runID string) (bool, error) {
	return p.runKnown(ctx, runID)
}

// DeleteRun implements [RunDeleter]. An uncertain inspect result is an
// error, not proof of absence. --force stops a running sandbox first.
func (p *MicrosandboxProvider) DeleteRun(ctx context.Context, runID string) (bool, error) {
	name := safeName("bonnie-", runID)
	exists, err := p.runKnown(ctx, runID)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	_, stderr, code, err := runCLI(ctx, nil, p.bin, "rm", "--force", name)
	if cerr := cliError("rm "+name, msbError(stderr), code, err); cerr != nil {
		return true, cerr
	}
	return true, nil
}

// SandboxExists implements [ExistenceChecker]. A Local work directory is a
// directory under the provider root.
func (p *LocalProvider) SandboxExists(_ context.Context, runID string) (bool, error) {
	if err := refuseUncheckedLegacy(p.root, runID); err != nil {
		return false, err
	}
	dir := filepath.Join(p.root, safeName("", runID))
	_, err := os.Stat(dir)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("bonnie: sandbox: stat work directory: %w", err)
	}
}

// DeleteRun implements [RunDeleter].
func (p *LocalProvider) DeleteRun(_ context.Context, runID string) (bool, error) {
	if err := refuseUncheckedLegacy(p.root, runID); err != nil {
		return false, err
	}
	dir := filepath.Join(p.root, safeName("", runID))
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("bonnie: sandbox: stat work directory: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return true, fmt.Errorf("bonnie: sandbox: remove work directory: %w", err)
	}
	return true, nil
}

var (
	_ ExistenceChecker = (*DockerProvider)(nil)
	_ RunDeleter       = (*DockerProvider)(nil)
	_ ExistenceChecker = (*MicrosandboxProvider)(nil)
	_ RunDeleter       = (*MicrosandboxProvider)(nil)
	_ ExistenceChecker = (*LocalProvider)(nil)
	_ RunDeleter       = (*LocalProvider)(nil)
)
