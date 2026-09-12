package sandbox

import (
	"context"
	"fmt"

	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Agent returns a [runtime.AgentFactory] whose tools run inside a sandbox.
//
// It replaces Kit's core tools with the sandboxed set, so the model gets a
// shell and a filesystem that are not the host's. BONNIE's own
// human-in-the-loop tools stay, because they run in the BONNIE process and
// never touch the sandbox.
//
// Use it wherever [runtime.KitAgent] would go:
//
//	runner := runtime.NewRunner(journal, sandbox.Agent(
//	    sandbox.Docker(sandbox.WithDockerImage("python:3.12-slim")),
//	    kit.WithModel("anthropic/claude-sonnet-4-5"),
//	))
//
// The sandbox opens on the first tool call that needs it, not when the run
// starts. A run that parks for human input holds no sandbox compute, and a run
// whose model never calls a tool never starts a container.
func Agent(p Provider, opts ...kit.Option) runtime.AgentFactory {
	return func(ctx context.Context, s *runtime.Session) (runtime.Agent, error) {
		// A resumed run whose workspace vanished must hear it from BONNIE,
		// not discover an empty directory mid-work. This check runs once
		// per agent build — once per Start or Resume — and never when the
		// run never opened a sandbox.
		if err := checkRecordedSandbox(ctx, p, s); err != nil {
			return nil, err
		}

		open := LazyOpener(p, s)

		sandboxed := []kit.Option{
			// Kit's core tools run in the BONNIE process. Leaving them on
			// beside the sandboxed set would give the model two shells,
			// one of them the host's, and it would pick either.
			func(o *kit.Options) { o.DisableCoreTools = true },
			kit.WithExtraTools(Tools(open)...),
		}
		return runtime.KitAgent(append(sandboxed, opts...)...)(ctx, s)
	}
}

// checkRecordedSandbox compares the sandbox a run recorded against what the
// backend reports now, and notes the loss when they disagree. It is the
// reason the sandbox record exists: without it, a run whose container was
// pruned while it was parked resumed in silence with an empty workspace, and
// the model watched files vanish between turns with no way to know why.
//
// The decision about what a vanished workspace means is recorded in
// docs/SPEC.md §4.10: not a failure — a run whose container was pruned by an
// operator can still do useful work — but never silence. The note lands in
// the conversation before the first step of the resumed turn, so the model
// can account for it in what it says next.
//
// A failed check is not fatal: the sandbox may be starting, the backend may
// be busy, and the first tool call would surface a real problem. Silence is
// reserved for "no sandbox was ever recorded".
func checkRecordedSandbox(ctx context.Context, p Provider, s *runtime.Session) error {
	backend, id, gone, ok := s.LastSandbox()
	if !ok || gone {
		return nil
	}
	if backend != p.Name() {
		// Unverified: this provider cannot ask another backend's sandbox
		// whether it exists, so there is no gone record — only the note.
		return s.NoteSandboxUnavailable(ctx, backend, id, false)
	}
	ec, ok := p.(ExistenceChecker)
	if !ok {
		return nil // the provider cannot report; the first tool call will
	}
	exists, err := ec.SandboxExists(ctx, s.RunID())
	if err != nil {
		return nil
	}
	if !exists {
		return s.NoteSandboxUnavailable(ctx, backend, id, true)
	}
	return nil
}

// Select returns the first provider that is available here, in the order
// given. It is how a host says "use a microVM when there is one, a container
// otherwise".
//
// It never falls back to [Local], because falling back from isolation to none
// is a security decision and must be written down, not inferred. Pass
// [Local] explicitly as the last candidate when that is what you want.
func Select(ctx context.Context, candidates ...Provider) (Provider, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("bonnie: sandbox: no candidate providers")
	}
	var reasons []error
	for _, p := range candidates {
		if err := p.Available(ctx); err != nil {
			reasons = append(reasons, fmt.Errorf("%s: %w", p.Name(), err))
			continue
		}
		return p, nil
	}

	err := fmt.Errorf("%w: no candidate backend is usable here", ErrUnavailable)
	for _, r := range reasons {
		err = fmt.Errorf("%w; %v", err, r)
	}
	return nil, err
}
