package sandbox

import (
	"context"
	"testing"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Both variants must change the tools that the model actually sees.
func TestAgentHumanInputOptions(t *testing.T) {
	t.Parallel()
	for _, human := range []bool{true, false} {
		t.Run(map[bool]string{true: "with human", false: "without human"}[human], func(t *testing.T) {
			t.Parallel()
			model := fakemodel.New(fakemodel.Say("done"))
			p := Local(WithLocalRoot(t.TempDir()))
			clean := func(o *kit.Options) {
				o.SkipConfig = true
				o.NoContextFiles = true
				o.NoSkills = true
				o.NoExtensions = true
				o.NoAgents = true
				o.Quiet = true
			}
			factory := AgentWithoutHumanInput(p, clean, model.Option())
			if human {
				factory = Agent(p, clean, model.Option())
			}
			r := runtime.NewRunner(runtime.NewMemoryJournal(), factory)
			if _, err := r.Start(context.Background(), "tools", runtime.Input{Text: "hi"}); err != nil {
				t.Fatal(err)
			}
			reqs := model.Requests()
			if len(reqs) != 1 {
				t.Fatalf("model requests = %d, want 1", len(reqs))
			}
			for _, name := range []string{"ask_human", "request_approval"} {
				if got := reqs[0].HasTool(name); got != human {
					t.Errorf("offers %s = %v, want %v", name, got, human)
				}
			}
		})
	}
}
