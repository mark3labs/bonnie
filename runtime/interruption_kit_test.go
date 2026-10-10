package runtime

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// Unsupported custom agents must fail instead of submitting empty input.
func TestSavedContinuationUnsupported(t *testing.T) {
	t.Parallel()
	agent := &fakeAgent{}
	_, err := generateCompletion(context.Background(), agent, "", nil, true)
	if !errors.Is(err, ErrContinuationUnsupported) {
		t.Fatalf("error = %v, want ErrContinuationUnsupported", err)
	}
	if agent.call != 0 {
		t.Fatal("unsupported agent was prompted")
	}
}

// Each restart shares only the reopened journal. Completed external effects
// must not run again, and neither saved messages nor provider calls may contain
// an empty or repeated task prompt, with or without completion checks.
func TestKitRepeatedInterruptedRecovery(t *testing.T) {
	t.Parallel()
	for _, checked := range []bool{false, true} {
		name := "plain"
		if checked {
			name = "completion"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			const runID = "interrupted-kit"
			const prompt = "original task prompt"
			model := fakemodel.New(fakemodel.Call("effect", `{}`), fakemodel.Call("effect", `{}`), fakemodel.Say("finished"))
			effects := 0
			tool := kit.NewTool("effect", "Record a completed effect", func(context.Context, struct{}) (kit.ToolOutput, error) {
				effects++
				return kit.TextResult("effect done"), nil
			})
			for attempt := range 3 {
				journal, err := OpenSQLiteJournal(dir)
				if err != nil {
					t.Fatal(err)
				}
				var runner *Runner
				factory := KitAgentWithSetup(false, func(_ context.Context, k *kit.Kit, _ *Session) error {
					if attempt < 2 {
						k.OnStepFinish(func(kit.StepFinishEvent) {
							if err := runner.Interrupt(runID); err != nil {
								t.Errorf("interrupt: %v", err)
							}
						})
					}
					return nil
				}, hermetic(), model.Option(), kit.WithTools(tool))
				var opts []RunnerOption
				if checked {
					opts = append(opts, WithCompletionHook(func(context.Context, CompletionCandidate) (CompletionFeedback, error) {
						return CompletionFeedback{}, nil
					}, 0))
				}
				runner = NewRunner(journal, factory, opts...)
				run, err := runner.Start(ctx, runID, Input{Text: prompt})
				if err != nil {
					t.Fatal(err)
				}
				want := RunInterrupted
				if attempt == 2 {
					want = RunCompleted
					if run.Response != "finished" {
						t.Fatalf("response = %q", run.Response)
					}
				}
				if run.State != want {
					t.Fatalf("attempt %d: state = %s, want %s", attempt, run.State, want)
				}
				recs, err := journal.Replay(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				users := 0
				for _, rec := range recs {
					if rec.Kind == RecordMessage && rec.Role == "user" {
						users++
						if rec.Text != prompt {
							t.Errorf("durable user message = %q", rec.Text)
						}
					}
				}
				if users != 1 {
					t.Errorf("attempt %d: durable user messages = %d, want 1", attempt, users)
				}
				if err := journal.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if effects != 2 {
				t.Errorf("external effects = %d, want 2", effects)
			}
			requests := model.Requests()
			if len(requests) != 3 {
				t.Fatalf("provider calls = %d, want 3", len(requests))
			}
			for i, request := range requests {
				users := 0
				calls, results := 0, 0
				for _, msg := range request.Messages {
					for _, part := range msg.Content {
						switch part.(type) {
						case kit.LLMToolCallPart:
							calls++
						case kit.LLMToolResultPart:
							results++
						}
					}
					if msg.Role == "user" {
						users++
						if messageText(msg) != prompt {
							t.Errorf("call %d: user message = %q", i, messageText(msg))
						}
					}
				}
				if users != 1 {
					t.Errorf("call %d: user messages = %d, want 1", i, users)
				}
				if calls != i || results != i {
					t.Errorf("call %d: saved tool calls/results = %d/%d, want %d/%d", i, calls, results, i, i)
				}
			}
		})
	}
}
