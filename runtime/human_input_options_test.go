package runtime

import (
	"context"
	"testing"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// Both variants must change the tools that the model actually sees.
func TestKitAgentHumanInputOptions(t *testing.T) {
	t.Parallel()
	for _, human := range []bool{true, false} {
		t.Run(map[bool]string{true: "with human", false: "without human"}[human], func(t *testing.T) {
			t.Parallel()
			model := fakemodel.New(fakemodel.Say("done"))
			factory := KitAgentWithoutHumanInput(hermetic(), model.Option())
			if human {
				factory = KitAgent(hermetic(), model.Option())
			}
			r := NewRunner(NewMemoryJournal(), factory)
			if _, err := r.Start(context.Background(), "tools", Input{Text: "hi"}); err != nil {
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
