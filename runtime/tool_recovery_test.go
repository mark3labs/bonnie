package runtime

import (
	"context"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// Intent must be committed before the action can see the outside world. The
// completed outcome precedes Kit's normal lossless step write.
func TestKitToolIntentPrecedesAction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	called := false
	tool := kit.NewTool("act", "act", func(context.Context, struct{}) (kit.ToolOutput, error) {
		recs, err := j.Replay(ctx, "r")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, rec := range recs {
			if rec.Kind == RecordToolIntent {
				found = true
			}
		}
		if !found {
			t.Fatal("action preceded durable intent")
		}
		called = true
		return kit.TextResult("result"), nil
	})
	r := NewRunner(j, scriptedKit(fakemodel.New(fakemodel.Call("act", `{}`), fakemodel.Say("done")), kit.WithTools(tool)))
	if _, err := r.Start(ctx, "r", Input{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("tool did not execute")
	}
	recs, err := j.Replay(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	intent, outcome, step := 0, 0, 0
	for _, rec := range recs {
		if rec.Kind == RecordToolIntent {
			intent = rec.Seq
		}
		if rec.Kind == RecordToolOutcome {
			outcome = rec.Seq
		}
		if rec.Kind == RecordMessage && rec.Role == "tool" {
			step = rec.Seq
		}
	}
	if intent == 0 || outcome <= intent || step <= outcome {
		t.Fatalf("intent=%d outcome=%d step=%d", intent, outcome, step)
	}
}

// The model cannot repeat an uncertain action merely by issuing a new call ID
// or changing JSON whitespace. A trusted host must first verify the effect.
func TestUnknownToolActionIsBlocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	s := NewSession("r", j)
	if err := appendToolRecord(ctx, s, RecordToolIntent, toolIntent{ID: "old", Name: "act", Args: `{"value":1}`}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	tool := kit.NewTool("act", "act", func(context.Context, struct {
		Value int `json:"value"`
	}) (kit.ToolOutput, error) {
		calls++
		return kit.TextResult("sent"), nil
	})
	model := fakemodel.New(fakemodel.Call("act", `{ "value": 1 }`), fakemodel.Say("done"))
	r := NewRunner(j, scriptedKit(model, kit.WithTools(tool)))
	if _, err := r.Start(ctx, "r", Input{Text: "continue"}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("repeated uncertain action")
	}
	factory := KitAgentWithSetup(false, func(ctx context.Context, _ *kit.Kit, s *Session) error {
		return s.ResolveInterruptedTool(ctx, "old", "Verified: no action occurred")
	}, hermetic(), fakemodel.New(fakemodel.Call("act", `{"value":1}`), fakemodel.Say("done")).Option(), kit.WithTools(tool))
	r = NewRunner(j, factory)
	if _, err := r.Start(ctx, "r", Input{Text: "approved retry"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("verified action calls=%d", calls)
	}
}
