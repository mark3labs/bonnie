package sandbox

import (
	"context"
	"fmt"
)

// forwarder passes provider capabilities that a wrapper does not change to
// the wrapped provider. EnvInjected and Seeded embed it so a correction to
// forwarding reaches both.
//
// It keeps only methods both wrappers have always advertised. Do not add
// reports whose absence matters, such as Imaged: see seededImaged for the
// defect that an always-present report causes.
type forwarder struct{ p Provider }

// Name implements [Provider]. The name is the wrapped backend's, unchanged, so
// a journal record and a network error still read as the real backend.
func (f forwarder) Name() string { return f.p.Name() }

// Available implements [Provider].
func (f forwarder) Available(ctx context.Context) error { return f.p.Available(ctx) }

// SetNetworkPolicy forwards to the wrapped provider. When that provider
// cannot enforce policies at all, the wrapper refuses — the same refusal the
// backend would have made, kept intact through the wrapper.
func (f forwarder) SetNetworkPolicy(policy NetworkPolicy) error {
	n, ok := f.p.(Networked)
	if !ok {
		return fmt.Errorf("%w: the %s sandbox cannot control the network", ErrPolicyUnsupported, f.p.Name())
	}
	return n.SetNetworkPolicy(policy)
}

// SandboxExists forwards to the wrapped provider. A backend that cannot
// report existence returns an error rather than a false "no": the caller
// treats an error as "cannot report" and a bare false as a verified loss,
// and confusing the two would record a loss that never happened.
func (f forwarder) SandboxExists(ctx context.Context, runID string) (bool, error) {
	ec, ok := f.p.(ExistenceChecker)
	if !ok {
		return false, fmt.Errorf("bonnie: sandbox: the %s backend cannot report sandbox existence", f.p.Name())
	}
	return ec.SandboxExists(ctx, runID)
}

// DeleteRun forwards to the wrapped provider. A backend that cannot delete
// reports that, rather than claiming a reclaim that never happened.
func (f forwarder) DeleteRun(ctx context.Context, runID string) (bool, error) {
	rd, ok := f.p.(RunDeleter)
	if !ok {
		return false, fmt.Errorf("bonnie: sandbox: the %s backend cannot delete sandboxes", f.p.Name())
	}
	return rd.DeleteRun(ctx, runID)
}

// ValidateRunCleanup implements [RunCleanupValidator]. It checks the wrapped
// backend's deletion capability and forwards its mode check when present.
func (f forwarder) ValidateRunCleanup() error {
	return validateRunCleanup(f.p)
}

// WorkingDir forwards to the wrapped provider, so the system prompt still
// names the directory the tools really use through a wrapper.
//
// Forgetting this was a live defect: seeding is applied to the DEFAULT
// backend in run.go, so the wrapper stood between the agent and the only
// provider that could report a host path. The prompt fell back to
// /workspace, a directory that does not exist under a host-mapped backend,
// and a live model said so — "my work directory is not actually /workspace".
//
// A provider that runs at [WorkDir] reports nothing and the caller's
// fallback applies, which is why this returns "" rather than guessing.
func (f forwarder) WorkingDir(runID string) string {
	r, ok := f.p.(WorkingDirReporter)
	if !ok {
		return ""
	}
	return r.WorkingDir(runID)
}

// providerWrapper is the method set that both wrappers must keep. Open stays
// on each wrapper because it is the Provider behavior that differs.
type providerWrapper interface {
	Provider
	Networked
	ExistenceChecker
	RunDeleter
	RunCleanupValidator
	WorkingDirReporter
}

var (
	_ providerWrapper = (*envProvider)(nil)
	_ providerWrapper = (*seeded)(nil)
)

// The shared forwarder itself must keep all seven forwarded methods. It does
// not implement Provider: each wrapper supplies its own Open.
var _ interface {
	Name() string
	Available(context.Context) error
	Networked
	ExistenceChecker
	RunDeleter
	RunCleanupValidator
	WorkingDirReporter
} = forwarder{}
