package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Failures at each journal operation must remain visible and permit a retry.
type cleanupErrorJournal struct {
	*MemoryJournal
	operation string
	err       error
}

func (j *cleanupErrorJournal) Runs(ctx context.Context, state RunState) ([]string, error) {
	if j.operation == "runs" {
		return nil, j.err
	}
	return j.MemoryJournal.Runs(ctx, state)
}

func (j *cleanupErrorJournal) State(ctx context.Context, id string) (RunState, error) {
	if j.operation == "state" && id == "a" {
		return "", j.err
	}
	return j.MemoryJournal.State(ctx, id)
}

func (j *cleanupErrorJournal) Replay(ctx context.Context, id string) ([]Record, error) {
	if j.operation == "replay" && id == "a" {
		return nil, j.err
	}
	return j.MemoryJournal.Replay(ctx, id)
}

func (j *cleanupErrorJournal) Append(ctx context.Context, rec Record) (int, error) {
	if j.operation == "append" && rec.Kind == RecordSandboxDeleted && rec.RunID == "a" {
		return 0, j.err
	}
	return j.MemoryJournal.Append(ctx, rec)
}

func TestSandboxCleanupJournalErrors(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"runs", "state", "replay", "append"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			want := errors.New("journal failure")
			j := &cleanupErrorJournal{MemoryJournal: NewMemoryJournal(), operation: operation, err: want}
			for _, id := range []string{"a", "b"} {
				cleanupCheckpoint(t, j, id, RunCompleted, now().Add(-time.Hour))
			}
			r := NewRunner(j, nil)
			calls := make(map[string]int)
			delete := func(_ context.Context, id string) (bool, error) { calls[id]++; return true, nil }
			policy := SandboxCleanupPolicy{CompletedAfter: time.Minute}
			if err := r.CleanupSandboxes(ctx, policy, delete); !errors.Is(err, want) {
				t.Fatalf("cleanup error=%v", err)
			}
			if operation != "runs" && calls["b"] != 1 {
				t.Fatal("did not continue after journal error")
			}
			if r.IsActive("a") || r.IsActive("b") {
				t.Fatal("cleanup did not release run")
			}
			j.operation = ""
			if err := r.CleanupSandboxes(ctx, policy, delete); err != nil {
				t.Fatal(err)
			}
			wantCalls := 1
			if operation == "append" {
				wantCalls = 2
			}
			if calls["a"] != wantCalls || calls["b"] != 1 {
				t.Fatalf("retry calls=%v", calls)
			}
		})
	}
}

func TestSandboxCleanupCallerContext(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	cleanupCheckpoint(t, j, "run", RunCancelled, now().Add(-time.Hour))
	r := NewRunner(j, nil)
	policy := SandboxCleanupPolicy{CancelledAfter: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.CleanupSandboxes(ctx, policy, func(got context.Context, _ string) (bool, error) {
		if got != ctx {
			t.Error("callback did not receive caller context")
		}
		cancel()
		return false, got.Err()
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup error=%v", err)
	}
	calls := 0
	if err := r.CleanupSandboxes(context.Background(), policy, func(context.Context, string) (bool, error) {
		calls++
		return false, nil
	}); err != nil || calls != 1 {
		t.Fatalf("cancelled deletion was not retried: calls=%d err=%v", calls, err)
	}
}

// A run can change after listing but before cleanup claims it. State must be
// read under the same claim that excludes new turns.
type cleanupChangedJournal struct {
	*MemoryJournal
	runner *Runner
}

func (j *cleanupChangedJournal) Runs(ctx context.Context, state RunState) ([]string, error) {
	ids, err := j.MemoryJournal.Runs(ctx, state)
	if err != nil {
		return nil, err
	}
	if err := j.Checkpoint(ctx, "run", RunWaiting); err != nil {
		return nil, err
	}
	return ids, nil
}

func (j *cleanupChangedJournal) State(ctx context.Context, id string) (RunState, error) {
	if !j.runner.IsActive(id) {
		return "", errors.New("state read without run claim")
	}
	return j.MemoryJournal.State(ctx, id)
}

func TestSandboxCleanupRechecksState(t *testing.T) {
	t.Parallel()
	j := &cleanupChangedJournal{MemoryJournal: NewMemoryJournal()}
	cleanupCheckpoint(t, j, "run", RunCompleted, now().Add(-time.Hour))
	r := NewRunner(j, nil)
	j.runner = r
	calls := 0
	if err := r.CleanupSandboxes(context.Background(), SandboxCleanupPolicy{CompletedAfter: time.Minute}, func(context.Context, string) (bool, error) {
		calls++
		return true, nil
	}); err != nil || calls != 0 {
		t.Fatalf("changed state: calls=%d err=%v", calls, err)
	}
}
