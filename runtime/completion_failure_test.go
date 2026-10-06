package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

type completionFailJournal struct {
	Journal
	phase string
	err   error
}

func (j *completionFailJournal) Append(ctx context.Context, rec Record) (int, error) {
	if rec.Kind == RecordCompletion {
		var c completionRecord
		if err := json.Unmarshal(rec.Payload, &c); err != nil {
			return 0, err
		}
		if c.Phase == j.phase {
			return 0, j.err
		}
	}
	return j.Journal.Append(ctx, rec)
}

// An orchestration write failure must not publish a draft or close the budget.
// Recovery can reuse a journalled final message and retry an uncommitted check.
func TestCompletionJournalFailureRecovery(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"pending", "check", "continue", "accepted"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			failure := errors.New("journal unavailable")
			j := &completionFailJournal{Journal: NewMemoryJournal(), phase: phase, err: failure}
			f, _ := fakeFactory(&kit.TurnResult{Response: "draft"})
			r := NewRunner(j, f, WithCompletionHook(func(_ context.Context, _ CompletionCandidate) (CompletionFeedback, error) {
				if phase == "continue" {
					return CompletionFeedback{ContinueWith: "fix"}, nil
				}
				return CompletionFeedback{}, nil
			}, 1))
			run, err := r.Start(context.Background(), "failure", Input{Text: "start"})
			if !errors.Is(err, failure) || run.State != RunRunning {
				t.Fatalf("run = %+v, err = %v", run, err)
			}
			f2, a := fakeFactory(&kit.TurnResult{Response: "recovered"})
			r2 := NewRunner(j.Journal, f2, WithCompletionHook(func(_ context.Context, _ CompletionCandidate) (CompletionFeedback, error) {
				return CompletionFeedback{}, nil
			}, 1))
			run, err = r2.Start(context.Background(), "failure", Input{Text: "start"})
			if err != nil || run.State != RunCompleted {
				t.Fatalf("run = %+v, err = %v", run, err)
			}
			want := 0
			if phase == "pending" {
				want = 1
			}
			if a.call != want {
				t.Fatalf("model calls = %d, want %d", a.call, want)
			}
		})
	}
}

// Cancellation after durable feedback preserves both the prompt and its charged
// budget. Recovery must not ask the hook to recreate that feedback.
func TestCompletionCancelledContinuationRecovery(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	f, a := fakeFactory(&kit.TurnResult{Response: "draft"})
	started := make(chan struct{})
	r := NewRunner(j, f, WithCompletionHook(func(_ context.Context, _ CompletionCandidate) (CompletionFeedback, error) {
		a.block = make(chan struct{})
		a.started = started
		return CompletionFeedback{ContinueWith: "fix"}, nil
	}, 1))
	done := make(chan *Run, 1)
	go func() {
		run, err := r.Start(context.Background(), "cancel-next", Input{})
		if err != nil {
			t.Errorf("Start: %v", err)
		}
		done <- run
	}()
	<-started
	if err := r.Cancel("cancel-next"); err != nil {
		t.Fatal(err)
	}
	if run := <-done; run.State != RunCancelled {
		t.Fatalf("run = %+v", run)
	}
	f2, a2 := fakeFactory(&kit.TurnResult{Response: "fixed"})
	r2 := NewRunner(j, f2, WithCompletionHook(func(_ context.Context, c CompletionCandidate) (CompletionFeedback, error) {
		if c.ContinuationsUsed != 1 || c.Response != "fixed" {
			t.Errorf("candidate = %+v", c)
		}
		return CompletionFeedback{}, nil
	}, 1))
	run, err := r2.Start(context.Background(), "cancel-next", Input{Text: "ignored"})
	if err != nil || run.Response != "fixed" || a2.call != 1 {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	s, err := Restore(context.Background(), "cancel-next", j)
	if err != nil {
		t.Fatal(err)
	}
	msgs := s.GetMessages()
	if text := messageText(msgs[len(msgs)-2]); text != "fix" {
		t.Fatalf("continuation prompt = %q", text)
	}
}
