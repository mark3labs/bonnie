package runtime

import (
	"context"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// A real Kit continues a crashed turn from the saved conversation with no new
// user message, and a tool call whose effect is unknown reaches the model as
// interrupted instead of running again.
func TestKitRecoverContinuesWithoutRepeatingTools(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j, reopen := admissionJournal(t, true)
	s := NewSession("r", j)
	turnStarted(RunRunning)(t, j, s)
	if _, err := s.AppendMessage(kit.NewLLMUserMessage("send the invoice")); err != nil {
		t.Fatal(err)
	}
	if err := appendToolRecord(ctx, s, RecordToolIntent, toolIntent{ID: "call-1", Name: "send", Args: `{}`}); err != nil {
		t.Fatal(err)
	}

	j = reopen()
	calls := 0
	tool := kit.NewTool("send", "send", func(context.Context, struct{}) (kit.ToolOutput, error) {
		calls++
		return kit.TextResult("sent"), nil
	})
	model := fakemodel.New(fakemodel.Say("I could not confirm the invoice was sent."))
	r := NewRunner(j, scriptedKit(model, kit.WithTools(tool)))
	var outcome RecoveredRun
	if err := r.Recover(ctx, RecoverOptions{Done: func(run RecoveredRun) { outcome = run }}); err != nil {
		t.Fatal(err)
	}
	if outcome.Err != nil || outcome.Run == nil || outcome.Run.State != RunCompleted {
		t.Fatalf("outcome = %+v", outcome)
	}
	if calls != 0 {
		t.Fatalf("tool ran again %d times", calls)
	}
	requests := model.Requests()
	if len(requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(requests))
	}
	text := requests[0].Text()
	for _, msg := range requests[0].Messages {
		for _, part := range msg.Content {
			if result, ok := part.(kit.LLMToolResultPart); ok {
				value, _ := toolResultText(result.Output)
				text += value
				if e, ok := result.Output.(kit.LLMToolResultOutputContentError); ok && e.Error != nil {
					text += e.Error.Error()
				}
			}
		}
	}
	if !strings.Contains(text, "send the invoice") || !strings.Contains(text, "external effect is unknown") {
		t.Fatalf("request does not carry the saved conversation:\n%s", text)
	}
	if strings.Count(text, "send the invoice") != 1 {
		t.Fatalf("input sent twice:\n%s", text)
	}
}
