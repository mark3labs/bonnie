package bonnie

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// A scoped tool can call a child model. A second runner gets a new scope and
// agent, with only the journal shared. The child itself is not durable.
func TestScopedToolChildCallAndResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	model := fakemodel.New(
		fakemodel.Call("review", `{}`), fakemodel.Say("child answer"),
		fakemodel.Call("ask_human", `{"question":"Continue?"}`),
		fakemodel.Call("review", `{}`), fakemodel.Say("second child answer"),
		fakemodel.Say("done"),
	)
	var agents []*kit.Kit
	cfg := resolve(
		WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
		WithKit(completionKitOptions(model)...),
		WithToolFactory(func(_ context.Context, scope ToolScope) ([]kit.Tool, error) {
			if scope.RunID != "scoped" || scope.Session == nil || scope.Exec == nil || scope.Kit == nil {
				return nil, fmt.Errorf("missing scope")
			}
			agents = append(agents, scope.Kit)
			return []kit.Tool{kit.NewTool("review", "Review with a child", func(ctx context.Context, _ struct{}) (kit.ToolOutput, error) {
				result, err := scope.Kit.Subagent(ctx, kit.SubagentConfig{Prompt: "review text", NoSession: true, Tools: []kit.Tool{}})
				if err != nil {
					return kit.ToolOutput{}, err
				}
				return kit.TextResult(result.Response), nil
			})}, nil
		}),
		WithKitSetup(func(_ context.Context, k *kit.Kit, _ RunScope) error {
			if len(agents) == 0 || agents[len(agents)-1] != k {
				return fmt.Errorf("setup ran before tool factory")
			}
			return nil
		}),
	)
	factory, err := cfg.agentFactory(ctx, "", cfg.kitOptions("", ""))
	if err != nil {
		t.Fatal(err)
	}
	journal := runtime.NewMemoryJournal()
	run, err := runtime.NewRunner(journal, factory).Start(ctx, "scoped", runtime.Input{Text: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != runtime.RunWaiting {
		t.Fatalf("state = %s", run.State)
	}
	run, err = runtime.NewRunner(journal, factory).Resume(ctx, "scoped", []runtime.InputResponse{{Text: "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != runtime.RunCompleted || run.Response != "done" {
		t.Fatalf("run = %+v", run)
	}
	if len(agents) != 2 || agents[0] == agents[1] {
		t.Fatalf("agents = %v", agents)
	}
	requests := model.Requests()
	if len(requests) != 6 || !requests[0].HasTool("review") || len(requests[1].Tools) != 0 {
		t.Fatalf("requests = %+v", requests)
	}
}

// Factory errors stop construction. A custom agent cannot ignore factories.
func TestToolFactoryErrors(t *testing.T) {
	t.Parallel()
	want := errors.New("factory failed")
	for _, tc := range []struct {
		name    string
		factory ToolFactory
		text    string
	}{
		{"nil factory", nil, "nil tool factory"},
		{"error", func(context.Context, ToolScope) ([]kit.Tool, error) { return nil, want }, "create scoped tools"},
		{"nil tool", func(context.Context, ToolScope) ([]kit.Tool, error) { return []kit.Tool{nil}, nil }, "nil tool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := resolve(WithToolFactory(tc.factory), WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))), WithKit(completionKitOptions(fakemodel.New())...))
			factory, err := cfg.agentFactory(context.Background(), "", cfg.kitOptions("", ""))
			if err != nil {
				t.Fatal(err)
			}
			_, err = runtime.NewRunner(runtime.NewMemoryJournal(), factory).Start(context.Background(), tc.name, runtime.Input{Text: "go"})
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error = %v", err)
			}
			if tc.name == "error" && !errors.Is(err, want) {
				t.Fatalf("lost error: %v", err)
			}
		})
	}
	cfg := resolve(WithToolFactory(nil), WithAgentFactory(func(context.Context, *runtime.Session) (runtime.Agent, error) { return nil, nil }))
	if got := cfg.agentConflicts(); got != "WithToolFactory" {
		t.Fatalf("conflict = %q", got)
	}
}
