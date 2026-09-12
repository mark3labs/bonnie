package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// journalFactory builds a fresh journal for a conformance case. reopen returns
// a second journal over the same store, which is how the suite states the
// difference between a memory journal and a durable one.
type journalFactory struct {
	name      string
	persisted bool
	open      func(t *testing.T) Journal
	reopen    func(t *testing.T, j Journal) Journal
}

// journalFactories is the list every conformance test runs against. Add an
// implementation here and it inherits the whole suite.
func journalFactories() []journalFactory {
	return []journalFactory{
		{
			name:      "memory",
			persisted: false,
			open:      func(*testing.T) Journal { return NewMemoryJournal() },
			reopen:    func(_ *testing.T, j Journal) Journal { return j },
		},
		{
			name:      "file",
			persisted: true,
			open: func(t *testing.T) Journal {
				t.Helper()
				j, err := OpenFileJournal(t.TempDir())
				if err != nil {
					t.Fatalf("OpenFileJournal: %v", err)
				}
				t.Cleanup(func() { _ = j.Close() })
				return j
			},
			reopen: func(t *testing.T, j Journal) Journal {
				t.Helper()
				fj, ok := j.(*FileJournal)
				if !ok {
					return j
				}
				if err := fj.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				// A brand-new journal over the same directory is the closest
				// a test gets to a process restart.
				next, err := OpenFileJournal(fj.Root())
				if err != nil {
					t.Fatalf("reopen: %v", err)
				}
				t.Cleanup(func() { _ = next.Close() })
				return next
			},
		},
	}
}

func eachJournal(t *testing.T, fn func(t *testing.T, f journalFactory)) {
	t.Helper()
	for _, f := range journalFactories() {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			fn(t, f)
		})
	}
}

func TestJournalAppendAssignsSequence(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		ctx := context.Background()

		for i := 1; i <= 3; i++ {
			seq, err := j.Append(ctx, Record{
				RunID: "r", Kind: RecordMessage, Role: "user",
				Text: fmt.Sprintf("m%d", i), Timestamp: now(),
			})
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			if seq != i {
				t.Fatalf("seq = %d, want %d", seq, i)
			}
		}

		recs, err := j.Replay(ctx, "r")
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		if len(recs) != 3 {
			t.Fatalf("replayed %d records, want 3", len(recs))
		}
		for i, rec := range recs {
			if rec.Seq != i+1 || rec.Text != fmt.Sprintf("m%d", i+1) {
				t.Fatalf("record %d = %+v", i, rec)
			}
		}
	})
}

func TestJournalUnknownRun(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		ctx := context.Background()

		if _, err := j.Replay(ctx, "nope"); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("Replay err = %v, want ErrRunNotFound", err)
		}
		if _, err := j.State(ctx, "nope"); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("State err = %v, want ErrRunNotFound", err)
		}
	})
}

func TestJournalCheckpointMovesState(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		ctx := context.Background()

		if _, err := j.Append(ctx, Record{RunID: "r", Kind: RecordMessage, Text: "hi", Timestamp: now()}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if s, err := j.State(ctx, "r"); err != nil || s != RunPending {
			t.Fatalf("State = %q, %v, want pending", s, err)
		}
		for _, want := range []RunState{RunRunning, RunWaiting, RunCompleted} {
			if err := j.Checkpoint(ctx, "r", want); err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			got, err := j.State(ctx, "r")
			if err != nil || got != want {
				t.Fatalf("State = %q, %v, want %q", got, err, want)
			}
		}

		// Every transition leaves a record, so the timeline is auditable.
		recs, err := j.Replay(ctx, "r")
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		var states int
		for _, rec := range recs {
			if rec.Kind == RecordState {
				states++
			}
		}
		if states != 3 {
			t.Fatalf("journalled %d state records, want 3", states)
		}
	})
}

func TestJournalRunsFilter(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		ctx := context.Background()

		if err := j.Checkpoint(ctx, "a", RunWaiting); err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
		if err := j.Checkpoint(ctx, "b", RunCompleted); err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}

		all, err := j.Runs(ctx, "")
		if err != nil {
			t.Fatalf("Runs: %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("Runs(all) = %v, want 2 runs", all)
		}
		waiting, err := j.Runs(ctx, RunWaiting)
		if err != nil {
			t.Fatalf("Runs: %v", err)
		}
		if len(waiting) != 1 || waiting[0] != "a" {
			t.Fatalf("Runs(waiting) = %v, want [a]", waiting)
		}
	})
}

func TestJournalPersistedFlag(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		if got := j.Persisted(); got != f.persisted {
			t.Fatalf("Persisted() = %v, want %v", got, f.persisted)
		}
		if got := NewSession("s", j).IsPersisted(); got != f.persisted {
			t.Fatalf("Session.IsPersisted() = %v, want %v", got, f.persisted)
		}
	})
}

// TestJournalConcurrentAppendsAreRaceClean exercises the lock discipline. Runs
// are independent, so they must not serialise on one another's state.
func TestJournalConcurrentAppendsAreRaceClean(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		ctx := context.Background()

		var wg sync.WaitGroup
		for r := range 4 {
			wg.Go(func() {
				runID := fmt.Sprintf("run-%d", r)
				for i := range 10 {
					if _, err := j.Append(ctx, Record{
						RunID: runID, Kind: RecordMessage,
						Text: fmt.Sprintf("m%d", i), Timestamp: now(),
					}); err != nil {
						t.Errorf("Append: %v", err)
						return
					}
				}
			})
		}
		wg.Wait()

		for r := range 4 {
			recs, err := j.Replay(ctx, fmt.Sprintf("run-%d", r))
			if err != nil {
				t.Fatalf("Replay: %v", err)
			}
			if len(recs) != 10 {
				t.Fatalf("run-%d replayed %d records, want 10", r, len(recs))
			}
		}
	})
}

// TestJournalSurvivesReopen is the durability claim, stated as a test the
// memory journal is expected to be unable to satisfy — so it is asserted only
// for journals that claim persistence.
func TestJournalSurvivesReopen(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		if !f.persisted {
			t.Skip("memory journal does not claim to survive a restart")
		}
		j := f.open(t)
		ctx := context.Background()

		s := NewSession("resumable", j)
		mustAppend(t, s, user("deploy the app"))
		mustAppend(t, s, toolCall("Deploying.", "c1", "deploy", `{"region":"eu"}`))
		mustAppend(t, s, toolResult("c1", "ok"))
		if err := j.Checkpoint(ctx, "resumable", RunWaiting); err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}

		next := f.reopen(t, j)
		state, err := next.State(ctx, "resumable")
		if err != nil || state != RunWaiting {
			t.Fatalf("State after reopen = %q, %v, want waiting", state, err)
		}

		restored, err := Restore(ctx, "resumable", next)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		msgs := restored.GetMessages()
		if len(msgs) != 3 {
			t.Fatalf("restored %d messages, want 3", len(msgs))
		}
		assertNoOrphan(t, msgs)

		call, ok := msgs[1].Content[1].(kit.LLMToolCallPart)
		if !ok {
			t.Fatalf("part is %T, want a tool call", msgs[1].Content[1])
		}
		if call.ToolCallID != "c1" || call.Input != `{"region":"eu"}` {
			t.Fatalf("tool call lost detail across the reopen: %+v", call)
		}
	})
}

// TestJournalAppendStepCommitsAsUnit holds a journal that implements
// [StepJournal] to the contract Kit's kit.StepAppender relies on: one call
// commits every record of a step, one sequence number per record, in input
// order, with nothing partial on the way. A journal that does not implement
// the interface skips: the per-record fallback still works, and the
// torn-write repair covers the crash window that leaves.
func TestJournalAppendStepCommitsAsUnit(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		if _, ok := j.(StepJournal); !ok {
			t.Skip("journal does not implement StepJournal; Session uses the " +
				"per-record fallback for it")
		}
		ctx := context.Background()
		const runID = "step-unit"
		s := NewSession(runID, j)

		ids, err := s.AppendStep(ctx, []kit.LLMMessage{
			toolCall("Deploying.", "c1", "deploy", `{"region":"eu"}`),
			toolResult("c1", "ok"),
		})
		if err != nil {
			t.Fatalf("AppendStep: %v", err)
		}
		if len(ids) != 2 || ids[0] == ids[1] {
			t.Fatalf("entry IDs = %v, want two distinct IDs in order", ids)
		}

		recs, err := j.Replay(ctx, runID)
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		if len(recs) != 2 {
			t.Fatalf("journal holds %d records, want 2", len(recs))
		}
		for i, rec := range recs {
			if rec.Seq != i+1 {
				t.Fatalf("record %d has seq %d, want %d: a step must not leave gaps", i, rec.Seq, i+1)
			}
			if rec.EntryID != ids[i] {
				t.Fatalf("record %d has entry %q, want %q", i, rec.EntryID, ids[i])
			}
		}

		// The restored session must see the step whole — the pair is the
		// point, not the records.
		restored, err := Restore(ctx, runID, j)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		msgs := restored.GetMessages()
		if len(msgs) != 2 {
			t.Fatalf("restored %d messages, want 2", len(msgs))
		}
		assertNoOrphan(t, msgs)
	})
}
