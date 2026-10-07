package bonnie

import (
	"context"
	"testing"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// The public option must reach the real model through the sandbox factory,
// also when a second Runner opens the journal. It must keep the sandbox tools
// and caller-supplied tools, not replace the whole tool set.
func TestWithoutHumanInput(t *testing.T) {
	t.Parallel()
	for _, disabled := range []bool{false, true} {
		name := "default"
		if disabled {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			model := fakemodel.New(fakemodel.Say("done"), fakemodel.Say("done again"))
			custom := kit.NewTool("custom", "A caller-supplied tool.", func(context.Context, struct{}) (kit.ToolOutput, error) {
				return kit.TextResult("ok"), nil
			})
			opts := []Option{
				WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
				WithTools(custom),
				WithKit(model.Option(), func(o *kit.Options) {
					o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.Quiet = true, true, true, true, true, true
				}),
			}
			if disabled {
				opts = append(opts, WithoutHumanInput())
			}
			c := resolve(opts...)
			factory, err := c.agentFactory(ctx, "", c.kitOptions("", ""))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			for i := range 2 {
				journal, err := runtime.OpenSQLiteJournal(dir)
				if err != nil {
					t.Fatal(err)
				}
				runner := runtime.NewRunner(journal, factory)
				if i == 0 {
					_, err = runner.Start(ctx, "human-input", runtime.Input{Text: "hi"})
				} else {
					_, err = runner.Start(ctx, "human-input", runtime.Input{Text: "hi again"})
				}
				closeErr := journal.Close()
				if err != nil {
					t.Fatal(err)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			requests := model.Requests()
			if len(requests) != 2 {
				t.Fatalf("requests = %d, want 2", len(requests))
			}
			for _, req := range requests {
				for _, tool := range []string{"ask_human", "request_approval"} {
					if req.HasTool(tool) == disabled {
						t.Errorf("%s present = %v, want %v", tool, req.HasTool(tool), !disabled)
					}
				}
				for _, tool := range []string{"shell", "read_file", "write_file", "list_files", "custom"} {
					if !req.HasTool(tool) {
						t.Errorf("missing tool %s", tool)
					}
				}
			}
		})
	}
}
