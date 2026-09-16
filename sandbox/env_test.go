package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnvInjectedReachesEveryBackend proves the injected variables land on a
// command on every backend, through the one [Command.Env] seam they share.
// A second command sees them too: the injection is per command, not a one-shot
// on the first Exec.
func TestEnvInjectedReachesEveryBackend(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, _ backend, p Provider) {
		ep := EnvInjected(p, map[string]string{"INJECTED": "from-operator"})
		sb := openSandbox(t, ep, "env-inject")
		ctx := testCtx(t)

		for _, attempt := range []string{"first", "second"} {
			res, err := sb.Exec(ctx, Shell("echo $INJECTED"))
			if err != nil {
				t.Fatalf("%s Exec: %v", attempt, err)
			}
			if !strings.Contains(res.Stdout, "from-operator") {
				t.Fatalf("%s command did not see the injected env: %q", attempt, res.Stdout)
			}
		}
	})
}

// TestEnvInjectedWinsOverPerCommandEnv pins the precedence: an injected value
// overrides a per-command variable of the same name, because a secret an
// operator fixed must not be clobbered by a command that sets the same key.
func TestEnvInjectedWinsOverPerCommandEnv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ep := EnvInjected(Local(WithLocalRoot(t.TempDir()), WithLocalCleanup()),
		map[string]string{"TOKEN": "operator-secret"})
	sb, err := ep.Open(ctx, "env-precedence")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sb.Close() }()

	res, err := sb.Exec(ctx, Command{
		Args: []string{"sh", "-lc", "echo $TOKEN"},
		Env:  []string{"TOKEN=model-guess"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "operator-secret" {
		t.Fatalf("TOKEN = %q, want the injected value to win", strings.TrimSpace(res.Stdout))
	}
}

// TestEnvInjectedDoesNotMutateCallerEnv checks the merge builds a fresh slice:
// a caller that reuses a Command must not find its Env grown by a call.
func TestEnvInjectedDoesNotMutateCallerEnv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ep := EnvInjected(Local(WithLocalRoot(t.TempDir()), WithLocalCleanup()),
		map[string]string{"A": "1", "B": "2"})
	sb, err := ep.Open(ctx, "env-nomutate")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sb.Close() }()

	callerEnv := []string{"C=3"}
	cmd := Command{Args: []string{"true"}, Env: callerEnv}
	if _, err := sb.Exec(ctx, cmd); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(callerEnv) != 1 || callerEnv[0] != "C=3" {
		t.Fatalf("the caller's Env was mutated: %v", callerEnv)
	}
}

// TestEnvInjectedEmptyIsPassthrough: a wrapper that adds nothing should cost
// nothing, so an empty (or all-empty-key) map returns the provider unwrapped.
func TestEnvInjectedEmptyIsPassthrough(t *testing.T) {
	t.Parallel()
	inner := Local(WithLocalRoot(t.TempDir()))

	if got := EnvInjected(inner, nil); got != Provider(inner) {
		t.Fatal("EnvInjected(nil) wrapped the provider; an empty env must be a no-op")
	}
	if got := EnvInjected(inner, map[string]string{"": "orphan"}); got != Provider(inner) {
		t.Fatal("EnvInjected with only an empty key wrapped the provider; it must be a no-op")
	}
}

// TestEnvInjectedForwardsSandboxDeleter: the wrapped sandbox must still satisfy
// a Deleter type assertion, or a reconciler and the conformance cleanup would
// silently stop reclaiming workspaces.
func TestEnvInjectedForwardsSandboxDeleter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	ep := EnvInjected(Local(WithLocalRoot(root), WithLocalCleanup()),
		map[string]string{"X": "1"})
	sb, err := ep.Open(ctx, "env-deleter")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d, ok := sb.(Deleter)
	if !ok {
		t.Fatal("the env wrapper dropped the Sandbox Deleter interface")
	}
	if err := d.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, safeName("", "env-deleter"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the workspace survived the delete: %v", err)
	}
}

// TestEnvInjectedDoesNotInventDeleter: a backend that cannot delete itself must
// not gain the ability through the wrapper, or a caller would call Delete on a
// sandbox that has no such operation.
func TestEnvInjectedDoesNotInventDeleter(t *testing.T) {
	t.Parallel()
	ep := EnvInjected(newStubProvider("stub"), map[string]string{"X": "1"})
	sb, err := ep.Open(context.Background(), "env-nodeleter")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := sb.(Deleter); ok {
		t.Fatal("the wrapper advertises Deleter over a backend that has none")
	}
}

// The wrapper must not break the optional provider interfaces, mirroring the
// guarantee [Seeded] makes: a wrapped host keeps network enforcement, sandbox
// reclamation, and the working-directory the prompt names.
func TestEnvInjectedForwardsOptionalInterfaces(t *testing.T) {
	t.Parallel()

	env := map[string]string{"K": "v"}

	t.Run("Networked refusal survives the wrapper", func(t *testing.T) {
		t.Parallel()
		p := EnvInjected(Local(), env)
		n, ok := p.(Networked)
		if !ok {
			t.Fatal("the wrapper dropped the Networked interface")
		}
		// Local cannot control egress; the wrapper must refuse, not pretend.
		if err := n.SetNetworkPolicy(NetworkPolicy{Mode: NetworkDenyAll}); !errors.Is(err, ErrPolicyUnsupported) {
			t.Fatalf("err = %v, want ErrPolicyUnsupported", err)
		}
	})

	t.Run("ExistenceChecker and RunDeleter are forwarded", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		root := t.TempDir()
		p := EnvInjected(Local(WithLocalRoot(root), WithLocalCleanup()), env)

		ec, ok := p.(ExistenceChecker)
		if !ok {
			t.Fatal("the wrapper dropped the ExistenceChecker interface")
		}
		if _, err := p.Open(ctx, "env-fwd"); err != nil {
			t.Fatalf("Open: %v", err)
		}
		exists, err := ec.SandboxExists(ctx, "env-fwd")
		if err != nil || !exists {
			t.Fatalf("SandboxExists = %v, %v; want true, nil", exists, err)
		}

		rd, ok := p.(RunDeleter)
		if !ok {
			t.Fatal("the wrapper dropped the RunDeleter interface")
		}
		deleted, err := rd.DeleteRun(ctx, "env-fwd")
		if err != nil || !deleted {
			t.Fatalf("DeleteRun = %v, %v; want true, nil", deleted, err)
		}
	})

	t.Run("WorkingDir is forwarded", func(t *testing.T) {
		t.Parallel()
		inner := Landlock(WithLandlockRoot(t.TempDir()))
		wrapped := EnvInjected(inner, env)
		r, ok := wrapped.(WorkingDirReporter)
		if !ok {
			t.Fatal("the wrapper dropped WorkingDirReporter: the prompt would name " +
				"a directory the tools do not use")
		}
		want := inner.WorkingDir("run-abc")
		if got := r.WorkingDir("run-abc"); got != want {
			t.Fatalf("wrapper reports %q, the backend runs at %q", got, want)
		}
	})

	t.Run("WorkingDir over a guest backend reports Workspace", func(t *testing.T) {
		t.Parallel()
		wrapped := EnvInjected(&stubProvider{}, env)
		if got := promptWorkingDir(wrapped, "run-abc"); got != Workspace {
			t.Fatalf("prompt working directory = %q, want %q", got, Workspace)
		}
	})
}
