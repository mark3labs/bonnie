package sandbox

import (
	"context"
	"fmt"
	"sort"
)

// EnvInjected wraps a [Provider] and adds a fixed set of environment variables
// to every command run in the sandboxes it opens. It is how an operator hands
// a run a credential or a setting the model must not choose: a database URL, a
// registry token, a feature flag.
//
// The variables are configured once, out of band, and the model never sees
// them as values. The model-facing bash tool builds a [Command] with only a
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
// [Networked], [ExistenceChecker], [RunDeleter], [WorkingDirReporter], and
// [Imaged], and forwards the [Sandbox]-level [Deleter] when the backend
// supports it, so wrapping never narrows what a caller can do with the
// provider.
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
	e := &envProvider{p: p, env: kv}
	if _, ok := p.(Imaged); ok {
		return &envImagedProvider{e}
	}
	return e
}

// envProvider is the [EnvInjected] wrapper.
type envProvider struct {
	p   Provider
	env []string // precomputed "KEY=value", sorted
}

var _ Provider = (*envProvider)(nil)

// Name implements [Provider]. The name is the wrapped backend's, unchanged, so
// a journal record and a network error still read as the real backend.
func (e *envProvider) Name() string { return e.p.Name() }

// Available implements [Provider].
func (e *envProvider) Available(ctx context.Context) error { return e.p.Available(ctx) }

// Open implements [Provider]. It wraps the opened sandbox so every command it
// runs carries the injected environment.
func (e *envProvider) Open(ctx context.Context, runID string) (Sandbox, error) {
	sb, err := e.p.Open(ctx, runID)
	if err != nil {
		return nil, err
	}
	return newEnvSandbox(sb, e.env), nil
}

// SetNetworkPolicy forwards to the wrapped provider, refusing on its behalf
// when the backend cannot control the network — the same refusal the backend
// would have made, kept intact through the wrapper. See [Seeded].
func (e *envProvider) SetNetworkPolicy(policy NetworkPolicy) error {
	n, ok := e.p.(Networked)
	if !ok {
		return fmt.Errorf("%w: the %s sandbox cannot control the network", ErrPolicyUnsupported, e.p.Name())
	}
	return n.SetNetworkPolicy(policy)
}

// SandboxExists forwards to the wrapped provider, reporting an error rather
// than a false "no" when the backend cannot answer. See [Seeded].
func (e *envProvider) SandboxExists(ctx context.Context, runID string) (bool, error) {
	ec, ok := e.p.(ExistenceChecker)
	if !ok {
		return false, fmt.Errorf("bonnie: sandbox: the %s backend cannot report sandbox existence", e.p.Name())
	}
	return ec.SandboxExists(ctx, runID)
}

// DeleteRun forwards to the wrapped provider, reporting an error rather than
// claiming a reclaim that never happened. See [Seeded].
func (e *envProvider) DeleteRun(ctx context.Context, runID string) (bool, error) {
	rd, ok := e.p.(RunDeleter)
	if !ok {
		return false, fmt.Errorf("bonnie: sandbox: the %s backend cannot delete sandboxes", e.p.Name())
	}
	return rd.DeleteRun(ctx, runID)
}

// WorkingDir forwards to the wrapped provider so the system prompt still names
// the directory the tools really use. A provider that runs at [Workspace]
// reports nothing and the caller's fallback applies. See [Seeded.WorkingDir].
func (e *envProvider) WorkingDir(runID string) string {
	r, ok := e.p.(WorkingDirReporter)
	if !ok {
		return ""
	}
	return r.WorkingDir(runID)
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
	merged := make([]string, 0, len(cmd.Env)+len(s.env))
	merged = append(merged, cmd.Env...)
	merged = append(merged, s.env...)
	cmd.Env = merged
	return s.Sandbox.Exec(ctx, cmd)
}

// envDeleterSandbox is an [envSandbox] whose backend can delete itself. It
// exists so a wrapped sandbox still satisfies a `sb.(Deleter)` type assertion:
// embedding the interface would forward Delete, but only a backend that has it
// should advertise it, so the wrapper mirrors the backend's own capability.
type envDeleterSandbox struct {
	*envSandbox
}

var _ Deleter = (*envDeleterSandbox)(nil)

// Delete implements [Deleter] by forwarding to the wrapped sandbox.
func (s *envDeleterSandbox) Delete(ctx context.Context) error {
	return s.Sandbox.(Deleter).Delete(ctx)
}

// newEnvSandbox wraps inner so its commands carry env. It advertises [Deleter]
// only when inner does, so the wrapper neither hides nor invents the ability
// to destroy the sandbox.
func newEnvSandbox(inner Sandbox, env []string) Sandbox {
	base := &envSandbox{Sandbox: inner, env: env}
	if _, ok := inner.(Deleter); ok {
		return &envDeleterSandbox{base}
	}
	return base
}
