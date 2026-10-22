package bonnie

import (
	"context"
	"fmt"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

func namedTool(name string) ToolFactory {
	return func(context.Context, ToolScope) ([]kit.Tool, error) {
		return []kit.Tool{kit.NewTool(name, name, func(context.Context, struct{}) (kit.ToolOutput, error) {
			return kit.TextResult(name), nil
		})}, nil
	}
}

// A named factory is replaced in place by a later one with the same name.
// That is how an explicit selfmod.Local in main.go replaces the default
// that `bonnie dev` registers, without a second modify_agent tool.
func TestNamedToolFactoryReplacesInPlace(t *testing.T) {
	t.Parallel()
	cfg := resolve(
		WithNamedToolFactory("slot", namedTool("first")),
		WithToolFactory(namedTool("anonymous")),
		WithNamedToolFactory("slot", namedTool("second")),
		WithNamedToolFactory("", namedTool("unnamed")),
	)
	var got []string
	for _, entry := range cfg.toolFactories {
		tools, err := entry.factory(context.Background(), ToolScope{})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, tools[0].Info().Name)
	}
	if want := fmt.Sprint([]string{"second", "anonymous", "unnamed"}); fmt.Sprint(got) != want {
		t.Fatalf("factories = %v, want %v", got, want)
	}
}

// Registered defaults apply before New's own options, so main.go wins.
// It changes package state, so it does not run in parallel.
func TestRegisterDefaultsApplyFirst(t *testing.T) {
	treeMu.Lock()
	saved := defaultOpts
	defaultOpts = nil
	treeMu.Unlock()
	t.Cleanup(func() {
		treeMu.Lock()
		defaultOpts = saved
		treeMu.Unlock()
	})
	RegisterDefaults(WithName("default"), WithModel("test/default"))
	cfg := resolve(WithName("explicit"))
	if cfg.name != "explicit" || cfg.model != "test/default" {
		t.Fatalf("name %q model %q", cfg.name, cfg.model)
	}

	// A default tool factory must not stop a tree whose host owns the agent.
	defaultOpts = nil
	RegisterDefaults(WithNamedToolFactory("slot", namedTool("default")))
	host := func(context.Context, *runtime.Session) (runtime.Agent, error) { return nil, nil }
	cfg = resolve(WithAgentFactory(host))
	if _, err := cfg.agentFactory(context.Background(), "", nil); err != nil {
		t.Fatalf("default tool factory beside WithAgentFactory: %v", err)
	}
	// An explicit factory is still refused.
	cfg = resolve(WithAgentFactory(host), WithNamedToolFactory("slot", namedTool("explicit")))
	if _, err := cfg.agentFactory(context.Background(), "", nil); err == nil {
		t.Fatal("explicit tool factory beside WithAgentFactory was accepted")
	}
}

// Each turn's tools see the sender of that turn's input, as the channel layer
// passed it with the input. A later message from someone else cannot change
// it: the CodeRabbit finding on #53 was that a principal read from the
// address journal after admission could be a newer sender's. New input with
// no sender clears it, and a restored session (a second process) has the
// latest turn's caller.
func TestTurnCallerReachesToolFactories(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := runtime.NewMemoryJournal()
	var seen []*runtime.Caller
	cfg := resolve(
		WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
		WithKit(completionKitOptions(fakemodel.New(fakemodel.Say("one"), fakemodel.Say("two"), fakemodel.Say("three")))...),
		WithToolFactory(func(_ context.Context, scope ToolScope) ([]kit.Tool, error) {
			seen = append(seen, scope.Session.Caller())
			return nil, nil
		}),
	)
	factory, err := cfg.agentFactory(ctx, "", cfg.kitOptions("", ""))
	if err != nil {
		t.Fatal(err)
	}
	alice := &runtime.Caller{Authenticator: "http", ID: "alice", Verified: true}
	bob := &runtime.Caller{Authenticator: "http", ID: "bob"}
	for _, in := range []runtime.Input{{Text: "a", Caller: alice}, {Text: "b", Caller: bob}} {
		if _, err := runtime.NewRunner(journal, factory).Start(ctx, "r", in); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := runtime.Restore(ctx, "r", journal)
	if err != nil {
		t.Fatal(err)
	}
	if c := restored.Caller(); c == nil || c.ID != "bob" || c.Verified {
		t.Fatalf("restored caller = %+v, want unverified bob", c)
	}
	if _, err := runtime.NewRunner(journal, factory).Start(ctx, "r", runtime.Input{Text: "c"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0].ID != "alice" || !seen[0].Verified || seen[1].ID != "bob" || seen[2] != nil {
		t.Fatalf("callers = %+v, want alice, bob, nil", seen)
	}
}
