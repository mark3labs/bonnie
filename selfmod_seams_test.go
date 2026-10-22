package bonnie

import (
	"context"
	"fmt"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
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

// RunScope.Caller returns the principal the channel layer recorded for the
// run's most recent message, and nil for a run with none.
func TestRunScopeCaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := runtime.NewMemoryJournal()
	addresses := chat.NewAddressMap(journal)
	for _, p := range []*channel.Principal{{ID: "first"}, {ID: "latest", Kind: "user"}} {
		if err := addresses.NotePrincipal(ctx, "known", p); err != nil {
			t.Fatal(err)
		}
	}
	if err := addresses.NotePrincipal(ctx, "other", &channel.Principal{ID: "other"}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]*channel.Principal{}
	cfg := resolve(
		WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
		WithKit(completionKitOptions(fakemodel.New(fakemodel.Say("one"), fakemodel.Say("two")))...),
		WithToolFactory(func(ctx context.Context, scope ToolScope) ([]kit.Tool, error) {
			p, err := scope.Caller(ctx)
			seen[scope.RunID] = p
			return nil, err
		}),
	)
	cfg.journalHandle = journal
	factory, err := cfg.agentFactory(ctx, "", cfg.kitOptions("", ""))
	if err != nil {
		t.Fatal(err)
	}
	runner := runtime.NewRunner(journal, factory)
	for _, id := range []string{"known", "unknown"} {
		if _, err := runner.Start(ctx, id, runtime.Input{Text: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	if p := seen["known"]; p == nil || p.ID != "latest" {
		t.Fatalf("known caller = %+v, want latest", p)
	}
	if p, ok := seen["unknown"]; !ok || p != nil {
		t.Fatalf("unknown caller = %+v (%v), want nil", p, ok)
	}
}
