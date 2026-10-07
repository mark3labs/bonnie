package runtime

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// A delayed cancellation must not stop a replacement turn. Completion is
// separate from the durable acknowledgement, and repeats write no new command.
func TestScopedCancellation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	j := NewMemoryJournal()
	factory, _, release, started := blockingFactory(&kit.TurnResult{})
	r := NewRunner(j, factory)
	done := make(chan *Run, 1)
	go func() {
		run, err := r.Start(ctx, "r", Input{Text: "work"})
		if err != nil {
			t.Error(err)
		}
		done <- run
	}()
	<-started
	snapshot, err := r.Snapshot(ctx, "r")
	if err != nil || snapshot.TurnID == "" {
		t.Fatalf("snapshot: %+v %v", snapshot, err)
	}
	stale, err := r.RequestCancel(ctx, "r", "old")
	if err != nil || stale.Status != CancelStale {
		t.Fatalf("stale: %+v %v", stale, err)
	}
	result, err := r.RequestCancel(ctx, "r", snapshot.TurnID)
	if err != nil || result.Status != CancelRequested {
		t.Fatalf("cancel: %+v %v", result, err)
	}
	if run := <-done; run == nil || run.State != RunCancelled {
		t.Fatalf("boundary: %+v", run)
	}
	close(release)
	recs, err := j.Replay(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, rec := range recs {
		if rec.Kind == RecordCancel {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("commands: %d", count)
	}
	result, err = r.RequestCancel(ctx, "r", snapshot.TurnID)
	if err != nil || result.Status != CancelNotActive {
		t.Fatalf("repeat: %+v %v", result, err)
	}
}

// A second process withdraws a parked approval. A late approval cannot resume
// it, while a new message can continue the conversation.
func TestCancelParkedAcrossRunnerBoundary(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	j, err := OpenSQLiteJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	factory, _ := fakeFactory(&kit.TurnResult{FinalValue: SuspendRequest{Kind: SuspendApproval, Prompt: "delete?"}})
	first := NewRunner(j, factory)
	parked, err := first.Start(ctx, "r", Input{Text: "work"})
	if err != nil || parked.State != RunWaiting {
		t.Fatalf("parked: %+v %v", parked, err)
	}
	factory2, _ := fakeFactory(&kit.TurnResult{Response: "new work"})
	second := NewRunner(j, factory2)
	result, err := second.RequestCancel(ctx, "r", parked.TurnID)
	if err != nil || result.Status != CancelRequested {
		t.Fatalf("cancel: %+v %v", result, err)
	}
	if _, err := second.Resume(ctx, "r", []InputResponse{Approve("")}); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("late approval: %v", err)
	}
	snapshot, err := second.Snapshot(ctx, "r")
	if err != nil || snapshot.State != RunCancelled || snapshot.Suspend != nil {
		t.Fatalf("snapshot: %+v %v", snapshot, err)
	}
	run, err := second.Start(ctx, "r", Input{Text: "next"})
	if err != nil || run.State != RunCompleted || run.TurnID == parked.TurnID {
		t.Fatalf("next: %+v %v", run, err)
	}
}

// A durable command survives a crash before its final state checkpoint.
func TestCancellationCommandRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	for _, rec := range []Record{{RunID: "r", Kind: RecordTurn, Text: "old"}, {RunID: "r", Kind: RecordState, State: RunWaiting}, {RunID: "r", Kind: RecordCancel, Text: "old"}} {
		if _, err := j.Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	factory, _ := fakeFactory(&kit.TurnResult{Response: "next"})
	r := NewRunner(j, factory)
	if _, err := r.Resume(ctx, "r", []InputResponse{Approve("")}); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("resume: %v", err)
	}
	run, err := r.Start(ctx, "r", Input{Text: "next"})
	if err != nil || run.State != RunCompleted {
		t.Fatalf("next: %+v %v", run, err)
	}
}
