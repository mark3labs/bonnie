package runtime

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// A model failure cannot supply a completion candidate, even when the model
// wrote a partial response before it failed.
func TestCompletionSkipsModelError(t *testing.T) {
	t.Parallel()
	failure := errors.New("model failed")
	f, a := fakeFactory(&kit.TurnResult{Response: "partial"})
	a.onPrompt = func(_ *Session) error { return failure }
	checks := 0
	r := NewRunner(nil, f, WithCompletionHook(func(_ context.Context, _ CompletionCandidate) (CompletionFeedback, error) {
		checks++
		return CompletionFeedback{}, nil
	}, 1))
	run, err := r.Start(context.Background(), "model-error", Input{})
	if !errors.Is(err, failure) || run.State != RunFailed || checks != 0 {
		t.Fatalf("run = %+v, err = %v, checks = %d", run, err, checks)
	}
}

func TestCompletionRunnerHookPrecedence(t *testing.T) {
	t.Parallel()
	factory := func(_ context.Context, s *Session) (Agent, error) {
		return &completionTestAgent{fakeAgent: &fakeAgent{session: s, turns: []*kit.TurnResult{{Response: "answer"}}}, hook: func(_ context.Context, _ CompletionCandidate) (CompletionFeedback, error) {
			t.Error("agent check called despite Runner hook")
			return CompletionFeedback{}, nil
		}}, nil
	}
	checks := 0
	r := NewRunner(nil, factory, WithCompletionHook(func(_ context.Context, _ CompletionCandidate) (CompletionFeedback, error) {
		checks++
		return CompletionFeedback{}, nil
	}, -1))
	run, err := r.Start(context.Background(), "precedence", Input{})
	if err != nil || run.State != RunCompleted || checks != 1 || r.completionLimit != 0 {
		t.Fatalf("run = %+v, err = %v, checks = %d", run, err, checks)
	}
}

// A durable human answer must survive the gap before the model starts.
func TestCompletionResumeRecordRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	r1 := NewRunner(j, nil)
	s := NewSession("resume-gap", j)
	if err := r1.checkpoint(ctx, s.runID, RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := r1.saveCompletion(ctx, s, &completionRecord{Phase: "pending", Candidate: CompletionCandidate{ContinuationsUsed: 1}, Prompt: "old prompt"}); err != nil {
		t.Fatal(err)
	}
	if err := r1.checkpoint(ctx, s.runID, RunWaiting); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(ctx, Record{RunID: s.runID, Kind: RecordResume, Text: "human answer"}); err != nil {
		t.Fatal(err)
	}
	if err := r1.checkpoint(ctx, s.runID, RunRunning); err != nil {
		t.Fatal(err)
	}
	f, _ := fakeFactory(&kit.TurnResult{Response: "finished"})
	r2 := NewRunner(j, f, WithCompletionHook(func(_ context.Context, c CompletionCandidate) (CompletionFeedback, error) {
		if c.ContinuationsUsed != 1 {
			t.Errorf("count = %d", c.ContinuationsUsed)
		}
		return CompletionFeedback{}, nil
	}, 1))
	if _, err := r2.Start(ctx, s.runID, Input{Text: "ignored"}); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(ctx, s.runID, j)
	if err != nil {
		t.Fatal(err)
	}
	if text := messageText(restored.GetMessages()[0]); text != "human answer" {
		t.Fatalf("prompt = %q", text)
	}
}
