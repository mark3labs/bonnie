package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// ExistenceChecker is implemented by a [Provider] that can report whether a
// run's sandbox still exists, without opening one. Opening would create, and
// the whole point of the question is whether the old workspace is gone.
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

// SandboxExists implements [ExistenceChecker].
func (p *DockerProvider) SandboxExists(ctx context.Context, runID string) (bool, error) {
	state, err := p.inspectState(ctx, safeName("bonnie-", runID))
	if err != nil {
		return false, err
	}
	return state != "", nil
}

// DeleteRun implements [RunDeleter].
func (p *DockerProvider) DeleteRun(ctx context.Context, runID string) (bool, error) {
	name := safeName("bonnie-", runID)
	state, err := p.inspectState(ctx, name)
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
	return p.exists(ctx, safeName("bonnie-", runID)), nil
}

// DeleteRun implements [RunDeleter]. exists() reports running and stopped
// sandboxes alike (--all), so a stopped sandbox is reaped too, and --force
// stops a running one first.
func (p *MicrosandboxProvider) DeleteRun(ctx context.Context, runID string) (bool, error) {
	name := safeName("bonnie-", runID)
	if !p.exists(ctx, name) {
		return false, nil
	}
	_, stderr, code, err := runCLI(ctx, nil, p.bin, "rm", "--force", name)
	if cerr := cliError("rm "+name, msbError(stderr), code, err); cerr != nil {
		return true, cerr
	}
	return true, nil
}

// SandboxExists implements [ExistenceChecker]. A Local workspace is a
// directory under the provider root.
func (p *LocalProvider) SandboxExists(_ context.Context, runID string) (bool, error) {
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
func (p *LocalProvider) DeleteRun(_ context.Context, runID string) (bool, error) {
	dir := filepath.Join(p.root, safeName("", runID))
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return false, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return true, fmt.Errorf("bonnie: sandbox: remove workspace: %w", err)
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
