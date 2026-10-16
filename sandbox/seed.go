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
// version control, not seed content, and never reaches a work directory.
const gitkeep = ".gitkeep"

// Seeded wraps a [Provider] and mirrors a local directory into every sandbox
// it opens. It is how a manifest's context files reaches the run: the files
// an author wrote under `work directory/` are present at [WorkDir] before the
// model's first command runs, on every backend, because the mirror travels
// over the [Sandbox] interface and not over any backend's own tooling.
//
// Seeding never overwrites a file the sandbox already has. A run that
// resumes — the same sandbox reopened — keeps every edit the model made; a
// seed file the model deleted reappears, which is the honest behaviour for a
// seed and stated here so nobody mistakes it for a bug.
//
// The wrapper forwards the optional provider interfaces: [Networked],
// [ExistenceChecker], [RunDeleter], [RunCleanupValidator], [WorkingDirReporter],
// and [Imaged]. Opened sandboxes preserve [DirectoryReader], [Deleter], [JobSandbox], and
// [JobOutputReader] when the backend supports them. A
// backend that cannot enforce a network policy still refuses one — the
// wrapper refuses on its behalf — so wrapping never widens what a caller
// can request.
func Seeded(p Provider, dir string) Provider {
	s := &seeded{forwarder{p: p}, dir}
	if _, ok := p.(Imaged); ok {
		return &seededImaged{s}
	}
	return s
}

// seeded is the [Seeded] wrapper.
type seeded struct {
	forwarder
	dir string
}

var _ Provider = (*seeded)(nil)

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
		return nil, fmt.Errorf("bonnie: sandbox: seed work directory: %w", err)
	}
	return newSeededJobSandbox(sb), nil
}

func newSeededJobSandbox(sb Sandbox) Sandbox {
	result := newSeededJobSandboxBase(sb)
	if reader, ok := sb.(DirectoryReader); ok {
		return preserveSkillDiscovery(preserveDirectoryReader(result, reader), sb)
	}
	return preserveSkillDiscovery(result, sb)
}

func newSeededJobSandboxBase(sb Sandbox) Sandbox {
	if jobs, ok := sb.(JobSandbox); ok {
		base := &seededJobSandbox{Sandbox: sb, jobs: jobs}
		_, deleter := sb.(Deleter)
		_, reader := sb.(JobOutputReader)
		switch {
		case deleter && reader:
			return &seededJobOutputDeleterSandbox{seededJobOutputSandbox{base}}
		case deleter:
			return &seededJobDeleterSandbox{base}
		case reader:
			return &seededJobOutputSandbox{base}
		default:
			return base
		}
	}
	return sb
}

type seededJobSandbox struct {
	Sandbox
	jobs JobSandbox
}

func (s *seededJobSandbox) StartJob(ctx context.Context, cmd Command) (string, error) {
	return s.jobs.StartJob(ctx, cmd)
}
func (s *seededJobSandbox) JobStatus(ctx context.Context, id string) (*JobStatus, error) {
	return s.jobs.JobStatus(ctx, id)
}
func (s *seededJobSandbox) CancelJob(ctx context.Context, id string) error {
	return s.jobs.CancelJob(ctx, id)
}

var _ JobSandbox = (*seededJobSandbox)(nil)

type seededJobDeleterSandbox struct{ *seededJobSandbox }

var _ Deleter = (*seededJobDeleterSandbox)(nil)

func (s *seededJobDeleterSandbox) Delete(ctx context.Context) error {
	return s.Sandbox.(Deleter).Delete(ctx)
}

type seededJobOutputSandbox struct{ *seededJobSandbox }

var _ JobOutputReader = (*seededJobOutputSandbox)(nil)

func (s *seededJobOutputSandbox) ReadJobOutput(ctx context.Context, id string, limit int64) (string, string, bool, error) {
	return s.Sandbox.(JobOutputReader).ReadJobOutput(ctx, id, limit)
}

type seededJobOutputDeleterSandbox struct{ seededJobOutputSandbox }

var _ Deleter = (*seededJobOutputDeleterSandbox)(nil)

func (s *seededJobOutputDeleterSandbox) Delete(ctx context.Context) error {
	return s.Sandbox.(Deleter).Delete(ctx)
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
// the method there is the defect [forwarder.WorkingDir] records for the
// neighbouring interface, one interface over.
type seededImaged struct{ *seeded }

var _ Imaged = (*seededImaged)(nil)

// Image implements [Imaged] by forwarding to the wrapped provider.
func (s *seededImaged) Image() string { return s.p.(Imaged).Image() }

// seedInto mirrors dir into the sandbox, relative to [WorkDir], skipping
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
