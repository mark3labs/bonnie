package runtime

import (
	"context"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// A running delivery must resume, but a turn that already finished before its
// settlement write must not execute twice. Pending markers cannot inherit an
// older turn's completed state.
func TestRunningSubmissionRecovery(t *testing.T) {
	t.Parallel()
	for _, phase := range []RunState{RunPending, RunRunning, RunCompleted, RunWaiting} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			j := NewMemoryJournal()
			f, _ := fakeFactory()
			r := NewRunner(j, f)
			item, err := r.Submit(ctx, "r", "one", Input{Text: "original"}, BusyQueue)
			if err != nil {
				t.Fatal(err)
			}
			item.State = SubmissionRunning
			if err := r.writeSubmission(ctx, RecordSubmissionState, *item); err != nil {
				t.Fatal(err)
			}
			if err := j.Checkpoint(ctx, "r", phase); err != nil {
				t.Fatal(err)
			}
			if phase == RunRunning {
				s := NewSession("r", j)
				if _, err := s.AppendMessage(kit.NewLLMUserMessage("original")); err != nil {
					t.Fatal(err)
				}
			}
			f, a := fakeFactory()
			r = NewRunner(j, f)
			startTestScheduler(t, r)
			wait, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			got, err := r.WaitSubmission(wait, "r", item.ID)
			if err != nil || got.State != SubmissionDone {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			s, err := Restore(ctx, "r", j)
			if err != nil {
				t.Fatal(err)
			}
			original := 0
			for _, m := range s.GetMessages() {
				if string(m.Role) == "user" && messageText(m) == "original" {
					original++
				}
			}
			if phase == RunRunning || phase == RunPending {
				if original != 1 {
					t.Fatalf("original inputs=%d", original)
				}
			}
			if phase == RunCompleted || phase == RunWaiting {
				if a.call != 0 || len(s.GetMessages()) != 0 {
					t.Fatal("settled turn was executed again")
				}
			}
		})
	}
}

// Durable steering placed in the transcript must not become a second input
// when a new scheduler starts. An unplaced steer remains queued for recovery.
func TestDurableSteerPlacementRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	f, _ := fakeFactory()
	r := NewRunner(j, f)
	item, err := r.Submit(ctx, "r", "steer", Input{Text: "use pnpm"}, BusySteer)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession("r", j)
	if _, err := s.AppendMessage(kit.NewLLMUserMessage(steerText(*item))); err != nil {
		t.Fatal(err)
	}
	if err := j.Checkpoint(ctx, "r", RunCompleted); err != nil {
		t.Fatal(err)
	}
	f, _ = fakeFactory()
	r = NewRunner(j, f)
	startTestScheduler(t, r)
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	got, err := r.WaitSubmission(wait, "r", item.ID)
	if err != nil || got.State != SubmissionDone {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	s, err = Restore(ctx, "r", j)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.GetMessages()) != 1 {
		t.Fatalf("steer delivered twice: %d", len(s.GetMessages()))
	}
}

// Foreground child work completes independently while its parent is waiting.
// A scheduler must not execute one conversation at a time globally.
func TestParentWaitsForScheduledChild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	var r *Runner
	factory := func(_ context.Context, s *Session) (Agent, error) {
		a := &fakeAgent{session: s, turns: []*kit.TurnResult{{Response: "done"}}}
		if s.RunID() == "parent" {
			a.onPrompt = func(*Session) error {
				_, err := r.SpawnChild(ctx, "parent", "worker", Input{Text: "child"}, false)
				return err
			}
		}
		return a, nil
	}
	r = NewRunner(j, factory)
	item, err := r.Submit(ctx, "parent", "p", Input{Text: "delegate"}, BusyQueue)
	if err != nil {
		t.Fatal(err)
	}
	startTestScheduler(t, r)
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	got, err := r.WaitSubmission(wait, "parent", item.ID)
	if err != nil || got.State != SubmissionDone {
		t.Fatalf("parent=%+v err=%v", got, err)
	}
	children, err := r.Children(ctx, "parent")
	if err != nil || len(children) != 1 {
		t.Fatalf("children=%v err=%v", children, err)
	}
	state, err := j.State(ctx, children[0].ID)
	if err != nil || state != RunCompleted {
		t.Fatalf("child=%s err=%v", state, err)
	}
}
