package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// TestKitStepLimitCompletesWithoutAnAnswer records a silent-stop case: Kit
// reaches its step limit after a tool call, returns no error, and BONNIE marks
// the run completed without a final answer. The live turn-end event is the
// only turn-level stop reason; the Run result does not carry it.
func TestKitStepLimitCompletesWithoutAnAnswer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tool := kit.NewTool("work", "Do one unit of work.",
		func(context.Context, struct{}) (kit.ToolOutput, error) {
			return kit.TextResult("work done"), nil
		})
	model := fakemodel.New(fakemodel.Call("work", `{}`), fakemodel.Say("Finished."))
	runner := NewRunner(NewMemoryJournal(), scriptedKit(model,
		kit.WithExtraTools(tool), func(o *kit.Options) { o.MaxSteps = 1 }))
	events, unsub := runner.bus.Subscribe("step-limit", 0)
	defer unsub()

	run, err := runner.Start(ctx, "step-limit", Input{Text: "Do the work, then report."})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.State != RunCompleted || run.Response != "" {
		t.Fatalf("run = %+v, want completed with no answer", run)
	}
	if len(model.Requests()) != 1 || model.Remaining() != 1 {
		t.Fatal("the model must stop before it supplies the final answer")
	}
	// The state event follows the live turn-end event. Read through it to
	// verify that a stop reason exists even though the final answer is empty.
	for {
		select {
		case ev := <-events:
			if ev.Type == string(kit.EventTurnEnd) {
				var end kit.TurnEndEvent
				if err := json.Unmarshal(ev.Data, &end); err != nil {
					t.Fatalf("decode turn end: %v", err)
				}
				if end.StopReason != kit.FinishReasonToolCalls {
					t.Fatalf("stop reason = %q, want %q", end.StopReason, kit.FinishReasonToolCalls)
				}
				t.Logf("step limit: state=%s response=%q stop_reason=%q", run.State, run.Response, end.StopReason)
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
