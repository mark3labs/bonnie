package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// Hide optional read interfaces to exercise the third-party fallback.
type journalOnly struct {
	Journal
	StepJournal
}
type countedKinds struct {
	Journal
	KindReader
	replays atomic.Int64
	appends atomic.Int64
	sweeps  atomic.Int64
}

func (j *countedKinds) Replay(ctx context.Context, id string) ([]Record, error) {
	j.replays.Add(1)
	return j.Journal.Replay(ctx, id)
}
func (j *countedKinds) Append(ctx context.Context, rec Record) (int, error) {
	j.appends.Add(1)
	return j.Journal.Append(ctx, rec)
}
func (j *countedKinds) RunsWithKinds(ctx context.Context, kinds ...RecordKind) ([]string, error) {
	j.sweeps.Add(1)
	return j.KindReader.RunsWithKinds(ctx, kinds...)
}

func TestJournalKindReader(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		ctx := context.Background()
		j := f.open(t)
		records := []Record{
			{RunID: "z", Kind: RecordMessage, Timestamp: now(), Payload: []byte(`"history"`)},
			{RunID: "a", Kind: RecordOwnerCancel, Timestamp: now(), Payload: []byte(`true`)},
			{RunID: "z", Kind: RecordOwnerCancel, Timestamp: now(), Payload: []byte(`false`)},
			{RunID: "a", Kind: RecordMessage, Timestamp: now(), Payload: []byte(`"a"`)},
		}
		if _, err := j.(StepJournal).AppendStep(ctx, records[:1]); err != nil {
			t.Fatal(err)
		}
		for _, rec := range records[1:] {
			if _, err := j.Append(ctx, rec); err != nil {
				t.Fatal(err)
			}
		}
		if err := j.Checkpoint(ctx, "z", RunCompleted); err != nil {
			t.Fatal(err)
		}
		j = f.reopen(t, j)
		kr := j.(KindReader)
		for _, source := range []Journal{j, journalOnly{Journal: j}} {
			got, err := replayKinds(ctx, source, "z", RecordState, RecordOwnerCancel, RecordOwnerCancel)
			if err != nil || len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 {
				t.Fatalf("filtered: %v, %v", got, err)
			}
			got[0].Payload[0] = 'x'
			again, err := replayKinds(ctx, source, "z", RecordOwnerCancel)
			if err != nil || string(again[0].Payload) != "false" {
				t.Fatalf("payload alias: %v %v", again, err)
			}
			for _, kinds := range [][]RecordKind{nil, {RecordSubmission}} {
				got, err := replayKinds(ctx, source, "missing", kinds...)
				if err != nil || len(got) != 0 {
					t.Fatalf("missing: %v %v", got, err)
				}
			}
			got, err = recordsOfKind(ctx, source, RecordOwnerCancel)
			if err != nil || len(got) != 2 || got[0].RunID != "a" || got[1].RunID != "z" {
				t.Fatalf("global: %v %v", got, err)
			}
		}
		ids, err := kr.RunsWithKinds(ctx, RecordMessage, RecordOwnerCancel, RecordMessage)
		if err != nil || !reflect.DeepEqual(ids, []string{"a", "z"}) {
			t.Fatalf("ids: %v %v", ids, err)
		}
		ids, err = kr.RunsWithKinds(ctx)
		if err != nil || len(ids) != 0 {
			t.Fatalf("empty: %v %v", ids, err)
		}
	})
}

func seedHistory(tb testing.TB, j Journal, n int) {
	tb.Helper()
	ctx := context.Background()
	for i := range n {
		id := fmt.Sprintf("history-%04d", i)
		recs := make([]Record, 500)
		for k := range recs {
			recs[k] = Record{RunID: id, Kind: RecordMessage, Timestamp: now(), Payload: []byte(`"history"`)}
		}
		if _, err := j.(StepJournal).AppendStep(ctx, recs); err != nil {
			tb.Fatal(err)
		}
		if err := j.Checkpoint(ctx, id, RunCompleted); err != nil {
			tb.Fatal(err)
		}
	}
}

func TestSchedulerIdleDoesNotReplay(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		seedHistory(t, j, 200)
		counted := &countedKinds{Journal: j, KindReader: j.(KindReader)}
		r := NewRunner(counted, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := r.RunScheduler(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if counted.sweeps.Load() < 2 {
			t.Fatal("scheduler did not sweep")
		}
		if got := counted.replays.Load(); got != 0 {
			t.Fatalf("Replay calls: %d", got)
		}
	})
}

func TestChildrenUsesOwnershipOnly(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		seedHistory(t, j, 10)
		ctx := context.Background()
		for _, child := range []ChildRun{{ID: "b", ParentID: "p"}, {ID: "a", ParentID: "p", Background: true}, {ID: "c", ParentID: "other"}} {
			payload, err := json.Marshal(child)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := j.Append(ctx, Record{RunID: child.ID, Kind: RecordOwnership, Timestamp: now(), Payload: payload}); err != nil {
				t.Fatal(err)
			}
		}
		counted := &countedKinds{Journal: j, KindReader: j.(KindReader)}
		fast, err := NewRunner(counted, nil).Children(ctx, "p")
		if err != nil {
			t.Fatal(err)
		}
		slow, err := NewRunner(journalOnly{Journal: j}, nil).Children(ctx, "p")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fast, slow) || len(fast) != 2 || fast[0].ID != "a" {
			t.Fatalf("children: %v %v", fast, slow)
		}
		if counted.replays.Load() != 0 {
			t.Fatal("child lookup replayed history")
		}
	})
}

func TestOwnerCancellationRecoverySettles(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		for _, fallback := range []bool{false, true} {
			t.Run(fmt.Sprint(fallback), func(t *testing.T) {
				ctx := context.Background()
				j := f.open(t)
				if err := j.Checkpoint(ctx, "p", RunRunning); err != nil {
					t.Fatal(err)
				}
				source := j
				if fallback {
					source = journalOnly{Journal: j, StepJournal: j.(StepJournal)}
				}
				r := NewRunner(source, nil)
				child, err := r.SpawnChild(ctx, "p", "child", Input{Text: "child"}, false)
				if err != nil {
					t.Fatal(err)
				}
				grandchild, err := r.SpawnChild(ctx, child.ID, "grandchild", Input{Text: "grandchild"}, false)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := j.Append(ctx, Record{RunID: "p", Kind: RecordOwnerCancel, Timestamp: now(), Payload: []byte("false")}); err != nil {
					t.Fatal(err)
				}
				// The owner and child settled, but a queued grandchild survived a crash.
				for _, id := range []string{"p", child.ID} {
					if err := j.Checkpoint(ctx, id, RunCancelled); err != nil {
						t.Fatal(err)
					}
				}
				counted := &countedKinds{Journal: source, KindReader: j.(KindReader)}
				var recovery Journal = counted
				if fallback {
					recovery = journalOnly{Journal: counted}
				}
				next := NewRunner(recovery, nil)
				if err := next.recoverOwnedCancellation(ctx, "p"); err != nil {
					t.Fatal(err)
				}
				state, err := j.State(ctx, grandchild.ID)
				if err != nil || state != RunCancelled {
					t.Fatalf("grandchild: %s %v", state, err)
				}
				counted.appends.Store(0)
				for range 3 {
					if err := next.recoverOwnedCancellation(ctx, "p"); err != nil {
						t.Fatal(err)
					}
				}
				if counted.appends.Load() != 0 {
					t.Fatal("settled cancellation wrote records")
				}
				settled, err := next.cancellationSettled(ctx, "p", false)
				if err != nil || !settled {
					t.Fatalf("settled: %v %v", settled, err)
				}
			})
		}
	})
}

func BenchmarkSchedulerIdle(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			j := NewMemoryJournal()
			seedHistory(b, j, n)
			r := NewRunner(j, nil)
			ctx := context.Background()
			b.ResetTimer()
			for b.Loop() {
				if _, err := r.schedulerCandidates(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
func BenchmarkChildren(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			j := NewMemoryJournal()
			seedHistory(b, j, n)
			r := NewRunner(j, nil)
			ctx := context.Background()
			if _, err := j.Append(ctx, Record{RunID: "child", Kind: RecordOwnership, Payload: []byte(`{"id":"child","parent_id":"p"}`)}); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				if _, err := r.Children(ctx, "p"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// A new runner must deliver queued input with and without the optional index.
func TestIndexedSubmissionRecovery(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		for _, fallback := range []bool{false, true} {
			t.Run(fmt.Sprint(fallback), func(t *testing.T) {
				j := f.open(t)
				source := j
				if fallback {
					source = journalOnly{Journal: j, StepJournal: j.(StepJournal)}
				}
				factory, _ := fakeFactory()
				first := NewRunner(source, factory)
				item, err := first.Submit(context.Background(), "r", "one", Input{Text: "original"}, BusyQueue)
				if err != nil {
					t.Fatal(err)
				}
				factory, _ = fakeFactory()
				next := NewRunner(source, factory)
				startTestScheduler(t, next)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				got, err := next.WaitSubmission(ctx, "r", item.ID)
				if err != nil || got.State != SubmissionDone {
					t.Fatalf("delivery: %v %v", got, err)
				}
			})
		}
	})
}

func TestCancellationSettlementHonorsChildBackgroundIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	r := NewRunner(j, nil)
	if err := j.Checkpoint(ctx, "p", RunRunning); err != nil {
		t.Fatal(err)
	}
	child, err := r.SpawnChild(ctx, "p", "child", Input{}, false)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := r.SpawnChild(ctx, child.ID, "background", Input{}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"p", child.ID} {
		if err := j.Checkpoint(ctx, id, RunCancelled); err != nil {
			t.Fatal(err)
		}
	}
	// Clear the direct child's queued input to reach its background descendants.
	items, err := r.Submissions(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AbortSubmission(ctx, child.ID, items[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(ctx, Record{RunID: child.ID, Kind: RecordOwnerCancel, Payload: []byte("true")}); err != nil {
		t.Fatal(err)
	}
	settled, err := r.cancellationSettled(ctx, "p", false)
	if err != nil || settled {
		t.Fatalf("settled with live background grandchild %s: %v %v", grandchild.ID, settled, err)
	}
}

// Opening an older store adds the index without changing or losing records.
func TestSQLiteKindIndexMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	j, err := OpenSQLiteJournal(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(ctx, Record{RunID: "r", Kind: RecordOwnerCancel, Timestamp: now(), Payload: []byte("true")}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.db.ExecContext(ctx, "DROP INDEX records_by_kind"); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = OpenSQLiteJournal(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	var count int
	if err := j.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'records_by_kind'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("kind index was not created")
	}
	recs, err := j.ReplayKinds(ctx, "r", RecordOwnerCancel)
	if err != nil || len(recs) != 1 || string(recs[0].Payload) != "true" {
		t.Fatalf("records: %v %v", recs, err)
	}
}
