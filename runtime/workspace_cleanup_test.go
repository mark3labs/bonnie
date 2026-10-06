package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

func cleanupCheckpoint(t *testing.T, j Journal, id string, state RunState, at time.Time) {
	t.Helper()
	if _, err := j.Append(context.Background(), Record{RunID: id, Kind: RecordState, State: state, Timestamp: at}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceCleanupRejectsNegativeRetention(t *testing.T) {
	t.Parallel()
	r := NewRunner(NewMemoryJournal(), nil)
	if err := r.CleanupWorkspaces(context.Background(), WorkspaceCleanupPolicy{CompletedAfter: -time.Second}, func(context.Context, string) (bool, error) {
		t.Fatal("invalid policy reached deletion")
		return false, nil
	}); err == nil {
		t.Fatal("negative retention accepted")
	}
}

func TestWorkspaceCleanupRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, state := range []RunState{RunCompleted, RunFailed, RunCancelled, RunRetired} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			j := NewMemoryJournal()
			old := now().Add(-2 * time.Hour)
			cleanupCheckpoint(t, j, "old", state, old)
			cleanupCheckpoint(t, j, "recent", state, now())
			cleanupCheckpoint(t, j, "latest", state, old)
			cleanupCheckpoint(t, j, "latest", state, now())
			cleanupCheckpoint(t, j, "missing-time", state, time.Time{})
			cleanupCheckpoint(t, j, ReservedRunPrefix+"internal", state, old)
			for _, other := range []RunState{RunPending, RunRunning, RunWaiting} {
				cleanupCheckpoint(t, j, string(other), state, old)
				cleanupCheckpoint(t, j, string(other), other, old)
			}
			// Recent metadata must not reset the terminal retention clock.
			if _, err := j.Append(ctx, Record{RunID: "old", Kind: RecordStep, Timestamp: now()}); err != nil {
				t.Fatal(err)
			}
			r := NewRunner(j, nil)
			var calls []string
			delete := func(_ context.Context, id string) (bool, error) {
				calls = append(calls, id)
				return false, nil // An absent workspace is also a success.
			}
			if err := r.CleanupWorkspaces(ctx, WorkspaceCleanupPolicy{}, delete); err != nil || len(calls) != 0 {
				t.Fatalf("zero policy: calls=%v err=%v", calls, err)
			}
			policy := WorkspaceCleanupPolicy{time.Hour, time.Hour, time.Hour, time.Hour}
			for range 2 {
				if err := r.CleanupWorkspaces(ctx, policy, delete); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(calls, []string{"old"}) {
				t.Fatalf("deleted %v, want only old", calls)
			}
		})
	}
}

// A fresh process must see the receipt without changing the replayed messages.
// A later turn creates a new terminal checkpoint and permits cleanup again.
func TestWorkspaceCleanupSQLiteRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	j, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := fakeFactory(&kit.TurnResult{Response: "answer"})
	r := NewRunner(j, f)
	if _, err := r.Start(ctx, "run", Input{Text: "question"}); err != nil {
		t.Fatal(err)
	}
	before, err := Restore(ctx, "run", j)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCheckpoint(t, j, "run", RunCompleted, now().Add(-time.Hour))
	calls := 0
	delete := func(context.Context, string) (bool, error) { calls++; return true, nil }
	policy := WorkspaceCleanupPolicy{CompletedAfter: time.Minute}
	if err := r.CleanupWorkspaces(ctx, policy, delete); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	f, _ = fakeFactory()
	r = NewRunner(j, f)
	if err := r.CleanupWorkspaces(ctx, policy, delete); err != nil || calls != 1 {
		t.Fatalf("restart: calls=%d err=%v", calls, err)
	}
	after, err := Restore(ctx, "run", j)
	if err != nil {
		t.Fatal(err)
	}
	state, err := j.State(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.GetMessages(), after.GetMessages()) || state != RunCompleted {
		t.Fatal("cleanup changed replay")
	}
	if _, err := r.Start(ctx, "run", Input{Text: "next"}); err != nil {
		t.Fatal(err)
	}
	cleanupCheckpoint(t, j, "run", RunCompleted, now().Add(-time.Hour))
	if err := r.CleanupWorkspaces(ctx, policy, delete); err != nil || calls != 2 {
		t.Fatalf("new terminal checkpoint: calls=%d err=%v", calls, err)
	}
}

func TestWorkspaceCleanupExcludesActiveTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	f, ag := fakeFactory()
	ag.started = make(chan struct{})
	started := ag.started
	ag.block = make(chan struct{})
	r := NewRunner(j, f)
	done := make(chan error, 1)
	go func() {
		_, err := r.Start(ctx, "run", Input{Text: "hello"})
		done <- err
	}()
	<-started
	// Even a stale terminal checkpoint must not permit cleanup of an active run.
	cleanupCheckpoint(t, j, "run", RunCompleted, now().Add(-time.Hour))
	policy := WorkspaceCleanupPolicy{CompletedAfter: time.Minute}
	calls := 0
	delete := func(ctx context.Context, id string) (bool, error) {
		calls++
		if _, err := r.Start(ctx, id, Input{Text: "racing turn"}); !errors.Is(err, ErrRunActive) {
			t.Errorf("Start during cleanup = %v", err)
		}
		return true, nil
	}
	err := r.CleanupWorkspaces(ctx, policy, delete)
	close(ag.block)
	if turnErr := <-done; turnErr != nil {
		t.Fatal(turnErr)
	}
	if err != nil || calls != 0 {
		t.Fatalf("active cleanup: calls=%d err=%v", calls, err)
	}
	cleanupCheckpoint(t, j, "run", RunCompleted, now().Add(-time.Hour))
	if err := r.CleanupWorkspaces(ctx, policy, delete); err != nil || calls != 1 || r.IsActive("run") {
		t.Fatalf("cleanup lock: calls=%d err=%v", calls, err)
	}
}

func TestWorkspaceCleanupRetriesAndJoinsErrors(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	for _, id := range []string{"a", "b", "c"} {
		cleanupCheckpoint(t, j, id, RunFailed, now().Add(-time.Hour))
	}
	r := NewRunner(j, nil)
	first, second := errors.New("first"), errors.New("second")
	calls := make(map[string]int)
	delete := func(_ context.Context, id string) (bool, error) {
		calls[id]++
		if calls[id] == 1 {
			switch id {
			case "a":
				return false, first
			case "b":
				return false, second
			}
		}
		return true, nil
	}
	policy := WorkspaceCleanupPolicy{FailedAfter: time.Minute}
	err := r.CleanupWorkspaces(context.Background(), policy, delete)
	if !errors.Is(err, first) || !errors.Is(err, second) || calls["c"] != 1 {
		t.Fatalf("errors=%v calls=%v", err, calls)
	}
	if err := r.CleanupWorkspaces(context.Background(), policy, delete); err != nil {
		t.Fatal(err)
	}
	if calls["a"] != 2 || calls["b"] != 2 || calls["c"] != 1 {
		t.Fatalf("retry calls=%v", calls)
	}
}
