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

func TestSandboxCleanupRejectsNegativeRetention(t *testing.T) {
	t.Parallel()
	r := NewRunner(NewMemoryJournal(), nil)
	if err := r.CleanupSandboxes(context.Background(), SandboxCleanupPolicy{CompletedAfter: -time.Second}, func(context.Context, string) (bool, error) {
		t.Fatal("invalid policy reached deletion")
		return false, nil
	}); err == nil {
		t.Fatal("negative retention accepted")
	}
}

func TestSandboxCleanupRetention(t *testing.T) {
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
			if err := r.CleanupSandboxes(ctx, SandboxCleanupPolicy{}, delete); err != nil || len(calls) != 0 {
				t.Fatalf("zero policy: calls=%v err=%v", calls, err)
			}
			policy := SandboxCleanupPolicy{
				CompletedAfter: time.Hour, FailedAfter: time.Hour,
				CancelledAfter: time.Hour, RetiredAfter: time.Hour,
			}
			for range 2 {
				if err := r.CleanupSandboxes(ctx, policy, delete); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(calls, []string{"old"}) {
				t.Fatalf("deleted %v, want only old", calls)
			}
			policy.RecheckDeleted = true
			if err := NewRunner(j, nil).CleanupSandboxes(ctx, policy, delete); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, []string{"old", "old"}) {
				t.Fatalf("rechecked %v, want only old twice", calls)
			}
		})
	}
}

// A fresh process must see the receipt without changing the replayed messages.
// A later turn creates a new terminal checkpoint and permits cleanup again.
func TestSandboxCleanupSQLiteRestart(t *testing.T) {
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
	policy := SandboxCleanupPolicy{CompletedAfter: time.Minute}
	if err := r.CleanupSandboxes(ctx, policy, delete); err != nil {
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
	if err := r.CleanupSandboxes(ctx, policy, delete); err != nil || calls != 1 {
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
	if err := r.CleanupSandboxes(ctx, policy, delete); err != nil || calls != 2 {
		t.Fatalf("new terminal checkpoint: calls=%d err=%v", calls, err)
	}
}

func TestSandboxCleanupExcludesActiveTurns(t *testing.T) {
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
	policy := SandboxCleanupPolicy{CompletedAfter: time.Minute, RecheckDeleted: true}
	calls := 0
	delete := func(ctx context.Context, id string) (bool, error) {
		calls++
		if _, err := r.Start(ctx, id, Input{Text: "racing turn"}); !errors.Is(err, ErrRunActive) {
			t.Errorf("Start during cleanup = %v", err)
		}
		return true, nil
	}
	err := r.CleanupSandboxes(ctx, policy, delete)
	close(ag.block)
	if turnErr := <-done; turnErr != nil {
		t.Fatal(turnErr)
	}
	if err != nil || calls != 0 {
		t.Fatalf("active cleanup: calls=%d err=%v", calls, err)
	}
	cleanupCheckpoint(t, j, "run", RunCompleted, now().Add(-time.Hour))
	if err := r.CleanupSandboxes(ctx, policy, delete); err != nil || calls != 1 || r.IsActive("run") {
		t.Fatalf("cleanup lock: calls=%d err=%v", calls, err)
	}
}

func TestSandboxCleanupRetriesAndJoinsErrors(t *testing.T) {
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
	policy := SandboxCleanupPolicy{FailedAfter: time.Minute}
	err := r.CleanupSandboxes(context.Background(), policy, delete)
	if !errors.Is(err, first) || !errors.Is(err, second) || calls["c"] != 1 {
		t.Fatalf("errors=%v calls=%v", err, calls)
	}
	if err := r.CleanupSandboxes(context.Background(), policy, delete); err != nil {
		t.Fatal(err)
	}
	if calls["a"] != 2 || calls["b"] != 2 || calls["c"] != 1 {
		t.Fatalf("retry calls=%v", calls)
	}
}

// A new owner can recheck an absent sandbox without changing run history or state.
// Each successful recheck writes a receipt, including when the sandbox is absent.
func TestSandboxCleanupRecheckDeletedAcrossRunners(t *testing.T) {
	t.Parallel()
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "exists"}[exists], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			j := NewMemoryJournal()
			cleanupCheckpoint(t, j, "run", RunCompleted, now().Add(-time.Hour))
			policy := SandboxCleanupPolicy{CompletedAfter: time.Minute}
			calls := 0
			delete := func(context.Context, string) (bool, error) {
				calls++
				return exists, nil
			}
			if err := NewRunner(j, nil).CleanupSandboxes(ctx, policy, delete); err != nil {
				t.Fatal(err)
			}
			r := NewRunner(j, nil)
			if err := r.CleanupSandboxes(ctx, policy, delete); err != nil || calls != 1 {
				t.Fatalf("default: calls=%d err=%v", calls, err)
			}
			policy.RecheckDeleted = true
			if err := r.CleanupSandboxes(ctx, policy, delete); err != nil || calls != 2 {
				t.Fatalf("recheck: calls=%d err=%v", calls, err)
			}
			recs, err := j.Replay(ctx, "run")
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != 3 || recs[0].State != RunCompleted || recs[1].Kind != RecordSandboxDeleted || recs[2].Kind != RecordSandboxDeleted {
				t.Fatalf("recheck records=%v", recs)
			}
			state, err := j.State(ctx, "run")
			if err != nil || state != RunCompleted {
				t.Fatalf("state=%s err=%v", state, err)
			}
		})
	}
}

// An old receipt must not suppress a retry after a failed operator recheck.
func TestSandboxCleanupRecheckDeletedRetriesAcrossRunners(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	cleanupCheckpoint(t, j, "run", RunFailed, now().Add(-time.Hour))
	policy := SandboxCleanupPolicy{FailedAfter: time.Minute}
	if err := NewRunner(j, nil).CleanupSandboxes(ctx, policy, func(context.Context, string) (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(j, nil)
	policy.RecheckDeleted = true
	failure := errors.New("delete failed")
	calls := 0
	delete := func(context.Context, string) (bool, error) {
		calls++
		if calls == 1 {
			return false, failure
		}
		return true, nil
	}
	if err := r.CleanupSandboxes(ctx, policy, delete); !errors.Is(err, failure) {
		t.Fatalf("recheck error=%v", err)
	}
	recs, err := j.Replay(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[1].Kind != RecordSandboxDeleted {
		t.Fatalf("failed recheck changed records: %v", recs)
	}
	if err := r.CleanupSandboxes(ctx, policy, delete); err != nil || calls != 2 {
		t.Fatalf("retry: calls=%d err=%v", calls, err)
	}
	recs, err = j.Replay(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[2].Kind != RecordSandboxDeleted {
		t.Fatalf("retry records=%v", recs)
	}
}

// A receipt from a terminal turn does not permit cleanup after a nonterminal turn.
func TestSandboxCleanupRecheckDeletedSkipsNonterminalAcrossRunners(t *testing.T) {
	t.Parallel()
	for _, state := range []RunState{RunPending, RunRunning, RunWaiting} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			j := NewMemoryJournal()
			cleanupCheckpoint(t, j, "run", RunCompleted, now().Add(-time.Hour))
			policy := SandboxCleanupPolicy{CompletedAfter: time.Minute}
			if err := NewRunner(j, nil).CleanupSandboxes(ctx, policy, func(context.Context, string) (bool, error) {
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
			cleanupCheckpoint(t, j, "run", state, now().Add(-time.Hour))
			policy.RecheckDeleted = true
			if err := NewRunner(j, nil).CleanupSandboxes(ctx, policy, func(context.Context, string) (bool, error) {
				t.Error("nonterminal run reached deletion")
				return false, nil
			}); err != nil {
				t.Fatal(err)
			}
			recs, err := j.Replay(ctx, "run")
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != 3 || recs[2].State != state {
				t.Fatalf("nonterminal records=%v", recs)
			}
		})
	}
}
