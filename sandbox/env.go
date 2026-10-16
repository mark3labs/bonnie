package sandbox

import (
	"context"
	"sort"
)

// EnvInjected wraps a [Provider] and adds a fixed set of environment variables
// to every command run in the sandboxes it opens. It is how an operator hands
// a run a credential or a setting the model must not choose: a database URL, a
// registry token, a feature flag.
//
// The variables are configured once, out of band, and the model never sees
// them as values. The model-facing shell tool builds a [Command] with only a
// working directory, so it cannot set env itself; this wrapper injects below
// the tool, on the [Sandbox] layer, the same way [Seeded] mirrors files below
// it. The injection travels over the [Command.Env] seam every backend already
// honours, so one implementation covers local, Docker, microsandbox, and
// Landlock alike.
//
// The injected values win. They are appended after any [Command.Env] the
// caller set, and every backend resolves a later entry over an earlier one, so
// an operator-configured secret cannot be clobbered by a per-command variable
// of the same name. This is the honest default for a security control: the
// value the operator fixed is the value the command gets.
//
// An empty map is a no-op: the provider is returned unwrapped, because a
// wrapper that changes nothing should cost nothing. A key that is empty is
// dropped; a value may be anything, including the empty string.
//
// Like [Seeded], the wrapper forwards the optional provider interfaces
// [Networked], [ExistenceChecker], [RunDeleter], [RunCleanupValidator], [WorkingDirReporter], and
// [Imaged], and forwards the [Sandbox]-level [DirectoryReader], [Deleter] and [JobSandbox] only
// when the backend supports them, so wrapping never widens what a caller can do.
func EnvInjected(p Provider, env map[string]string) Provider {
	kv := make([]string, 0, len(env))
	for k, v := range env {
		if k == "" {
			continue // an empty key is not a variable; dropping it beats emitting "=value"
		}
		kv = append(kv, k+"="+v)
	}
	if len(kv) == 0 {
		return p
	}
	// Sort so the injected set is deterministic: a journal record, a log
	// line, or a test reads the same order every run.
	sort.Strings(kv)
	e := &envProvider{forwarder{p: p}, kv}
	if _, ok := p.(Imaged); ok {
		return &envImagedProvider{e}
	}
	return e
}

type envProvider struct {
	forwarder
	env []string
}

var _ Provider = (*envProvider)(nil)

// Open implements [Provider]. It wraps the opened sandbox so every command it
// runs carries the injected environment.
func (e *envProvider) Open(ctx context.Context, runID string) (Sandbox, error) {
	sb, err := e.p.Open(ctx, runID)
	if err != nil {
		return nil, err
	}
	return newEnvSandbox(sb, e.env), nil
}

// envImagedProvider is an [envProvider] whose backend runs a named image.
// The capability is mirrored rather than always advertised, for the reason
// given on [seededImaged].
type envImagedProvider struct{ *envProvider }

var _ Imaged = (*envImagedProvider)(nil)

// Image implements [Imaged] by forwarding to the wrapped provider.
func (e *envImagedProvider) Image() string { return e.p.(Imaged).Image() }

// envSandbox adds the injected environment to every command. It embeds the
// wrapped [Sandbox], so ID, ReadFile, WriteFile, Stop, and Close forward
// untouched and only Exec is overridden.
type envSandbox struct {
	Sandbox
	env []string
}

var _ Sandbox = (*envSandbox)(nil)

// Exec implements [Sandbox]. It appends the injected variables after the
// command's own, so an operator-configured value wins over a per-command one
// of the same name. A fresh slice is built rather than appending in place, so
// the caller's Env is never mutated by a call.
func (s *envSandbox) Exec(ctx context.Context, cmd Command) (*Result, error) {
	return s.Sandbox.Exec(ctx, withEnv(cmd, s.env))
}

type envDeleterSandbox struct{ *envSandbox }

var _ Deleter = (*envDeleterSandbox)(nil)

func (s *envDeleterSandbox) Delete(ctx context.Context) error {
	return s.Sandbox.(Deleter).Delete(ctx)
}

type envJobSandbox struct {
	*envSandbox
	jobs JobSandbox
}

var _ JobSandbox = (*envJobSandbox)(nil)

func (s *envJobSandbox) StartJob(ctx context.Context, cmd Command) (string, error) {
	return s.jobs.StartJob(ctx, withEnv(cmd, s.env))
}
func (s *envJobSandbox) JobStatus(ctx context.Context, id string) (*JobStatus, error) {
	return s.jobs.JobStatus(ctx, id)
}
func (s *envJobSandbox) CancelJob(ctx context.Context, id string) error {
	return s.jobs.CancelJob(ctx, id)
}

func newEnvSandbox(inner Sandbox, env []string) Sandbox {
	result := newEnvSandboxBase(inner, env)
	if _, ok := inner.(DirectoryReader); ok {
		reader := directoryReaderFunc((&envSandbox{Sandbox: inner, env: env}).readDirectory)
		return preserveDiscovery(preserveDirectoryReader(result, reader), inner)
	}
	return preserveDiscovery(result, inner)
}

func newEnvSandboxBase(inner Sandbox, env []string) Sandbox {
	base := &envSandbox{Sandbox: inner, env: env}
	_, deleter := inner.(Deleter)
	jobs, hasJobs := inner.(JobSandbox)
	_, reader := inner.(JobOutputReader)
	jobBase := &envJobSandbox{envSandbox: base, jobs: jobs}
	switch {
	case hasJobs && deleter && reader:
		return &envJobDeleterReaderSandbox{jobBase}
	case hasJobs && deleter:
		return &envJobDeleterSandbox{jobBase}
	case hasJobs && reader:
		return &envJobReaderSandbox{jobBase}
	case hasJobs:
		return jobBase
	case deleter && reader:
		return &envOutputDeleterSandbox{&envOutputSandbox{base}}
	case reader:
		return &envOutputSandbox{base}
	case deleter:
		return &envDeleterSandbox{base}
	default:
		return base
	}
}

// withEnv appends configured variables after command variables, so a configured
// value wins. It builds a fresh slice so the caller's Env is never mutated.
func withEnv(cmd Command, env []string) Command {
	merged := make([]string, 0, len(cmd.Env)+len(env))
	merged = append(merged, cmd.Env...)
	merged = append(merged, env...)
	cmd.Env = merged
	return cmd
}
