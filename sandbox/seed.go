package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// gitkeep is the scaffold's empty-directory marker. It is bookkeeping for
// version control, not seed content, and never reaches a workspace.
const gitkeep = ".gitkeep"

// Seeded wraps a [Provider] and mirrors a local directory into every sandbox
// it opens. It is how a manifest's workspace seed reaches the run: the files
// an author wrote under `workspace/` are present at [Workspace] before the
// model's first command runs, on every backend, because the mirror travels
// over the [Sandbox] interface and not over any backend's own tooling.
//
// Seeding never overwrites a file the sandbox already has. A run that
// resumes — the same sandbox reopened — keeps every edit the model made; a
// seed file the model deleted reappears, which is the honest behaviour for a
// seed and stated here so nobody mistakes it for a bug.
//
// The wrapper forwards the optional provider interfaces: [Networked],
// [ExistenceChecker], [RunDeleter], [WorkingDirReporter], and [Imaged]. A
// backend that cannot enforce a network policy still refuses one — the
// wrapper refuses on its behalf — so wrapping never widens what a caller
// can request.
func Seeded(p Provider, dir string) Provider {
	s := &seeded{p: p, dir: dir}
	if _, ok := p.(Imaged); ok {
		return &seededImaged{s}
	}
	return s
}

// seeded is the [Seeded] wrapper.
type seeded struct {
	p   Provider
	dir string
}

var _ Provider = (*seeded)(nil)

// Name implements [Provider].
func (s *seeded) Name() string { return s.p.Name() }

// Available implements [Provider].
func (s *seeded) Available(ctx context.Context) error { return s.p.Available(ctx) }

// Open implements [Provider]. The seed lands after the underlying open, so a
// failed seed fails the open: the tool call that triggered it reports the
// error, the model retries, and the seed lands when the fault clears. It is
// never silent and never permanent.
func (s *seeded) Open(ctx context.Context, runID string) (Sandbox, error) {
	sb, err := s.p.Open(ctx, runID)
	if err != nil {
		return nil, err
	}
	if err := seedInto(ctx, sb, s.dir); err != nil {
		return nil, fmt.Errorf("bonnie: sandbox: seed workspace: %w", err)
	}
	return sb, nil
}

// SetNetworkPolicy forwards to the wrapped provider. When that provider
// cannot enforce policies at all, the wrapper refuses — the same refusal the
// backend would have made, kept intact through the wrapper.
func (s *seeded) SetNetworkPolicy(policy NetworkPolicy) error {
	n, ok := s.p.(Networked)
	if !ok {
		return fmt.Errorf("%w: the %s sandbox cannot control the network", ErrPolicyUnsupported, s.p.Name())
	}
	return n.SetNetworkPolicy(policy)
}

// SandboxExists forwards to the wrapped provider. A backend that cannot
// report existence returns an error rather than a false "no": the caller
// treats an error as "cannot report" and a bare false as a verified loss,
// and confusing the two would record a loss that never happened.
func (s *seeded) SandboxExists(ctx context.Context, runID string) (bool, error) {
	ec, ok := s.p.(ExistenceChecker)
	if !ok {
		return false, fmt.Errorf("bonnie: sandbox: the %s backend cannot report sandbox existence", s.p.Name())
	}
	return ec.SandboxExists(ctx, runID)
}

// DeleteRun forwards to the wrapped provider. A backend that cannot delete
// reports that, rather than claiming a reclaim that never happened.
func (s *seeded) DeleteRun(ctx context.Context, runID string) (bool, error) {
	rd, ok := s.p.(RunDeleter)
	if !ok {
		return false, fmt.Errorf("bonnie: sandbox: the %s backend cannot delete sandboxes", s.p.Name())
	}
	return rd.DeleteRun(ctx, runID)
}

// WorkingDir forwards to the wrapped provider, so the system prompt still
// names the directory the tools really use when a seed is in play.
//
// Forgetting this was a live defect: seeding is applied to the DEFAULT
// backend in run.go, so the wrapper stood between the agent and the only
// provider that could report a host path. The prompt fell back to
// /workspace, a directory that does not exist under a host-mapped backend,
// and a live model said so — "my workspace is not actually /workspace".
//
// A provider that runs at [Workspace] reports nothing and the caller's
// fallback applies, which is why this returns "" rather than guessing.
func (s *seeded) WorkingDir(runID string) string {
	r, ok := s.p.(WorkingDirReporter)
	if !ok {
		return ""
	}
	return r.WorkingDir(runID)
}

// seededImaged is a [seeded] whose backend runs a named image.
//
// The capability is mirrored rather than always advertised, the way
// [newEnvSandbox] mirrors [Deleter]: for a REPORT, "" and "no such method"
// mean different things — an unnamed image against no image at all — and a
// wrapper that always answers turns [LandlockProvider], which runs no image,
// into one that claims to run a nameless one.
//
// Forwarding it at all is the point. run.go wraps the default backend in
// [EnvInjected] and then [Seeded] before the agent sees it, so a host that
// type-asserts [Imaged] after configuration reaches the wrapper. Dropping
// the method there is the defect [Seeded.WorkingDir] records for the
// neighbouring interface, one interface over.
type seededImaged struct{ *seeded }

var _ Imaged = (*seededImaged)(nil)

// Image implements [Imaged] by forwarding to the wrapped provider.
func (s *seededImaged) Image() string { return s.p.(Imaged).Image() }

// seedInto mirrors dir into the sandbox, relative to [Workspace], skipping
// files that are already there and skipping [gitkeep].
func seedInto(ctx context.Context, sb Sandbox, dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == gitkeep {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		target := Resolve(filepath.ToSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, err := sb.ReadFile(ctx, target); err == nil {
			return nil // the model's file wins; a resume never reverts
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		return sb.WriteFile(ctx, target, data)
	})
}
