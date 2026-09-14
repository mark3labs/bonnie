package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestReadsOfUnknownRunsDoNotGrowTheJournal states the cost of a read that
// finds nothing: none.
//
// The journal keeps one handle per run, and the handle used to be created by
// the mere act of asking about an ID. A BONNIE server reachable from outside
// answers 404 to `GET /runs/<invented-id>`, and paid for each 404 with a map
// entry that lived as long as the process. A scan of invented IDs was
// therefore an unbounded memory cost with no run behind it.
func TestReadsOfUnknownRunsDoNotGrowTheJournal(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	// One real run, so the map is not trivially empty.
	if _, err := j.Append(ctx, Record{
		RunID: "real-run", Kind: RecordMessage, Text: "hi", Timestamp: now(),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	j.mu.Lock()
	before := len(j.runs)
	j.mu.Unlock()

	for i := range 1000 {
		id := fmt.Sprintf("no-such-run-%d", i)
		if _, err := j.State(ctx, id); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("State(%s) = %v, want ErrRunNotFound", id, err)
		}
		if _, err := j.Replay(ctx, id); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("Replay(%s) = %v, want ErrRunNotFound", id, err)
		}
		if _, err := j.Position(ctx, id); err != nil {
			t.Fatalf("Position(%s): %v", id, err)
		}
	}

	j.mu.Lock()
	after := len(j.runs)
	j.mu.Unlock()
	if after != before {
		t.Fatalf("3000 reads of unknown runs added %d entries to the journal's map; it must add none", after-before)
	}
}

// TestReadOfAKnownRunIsCached is the other half: an existing run keeps
// exactly one handle, because its mutex, its sequence counter, and its flock
// all live there. A read that found a run must therefore adopt it, not hand
// out a private copy a later write would not share.
func TestReadOfAKnownRunIsCached(t *testing.T) {
	t.Parallel()
	j := openTempJournal(t)
	ctx := context.Background()

	if _, err := j.Append(ctx, Record{
		RunID: "cached-run", Kind: RecordMessage, Text: "hi", Timestamp: now(),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// A fresh journal over the same directory has never seen the run.
	j2, err := OpenFileJournal(j.Root())
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	defer func() { _ = j2.Close() }()

	if _, err := j2.State(ctx, "cached-run"); err != nil {
		t.Fatalf("State: %v", err)
	}
	j2.mu.Lock()
	adopted, ok := j2.runs["cached-run"]
	j2.mu.Unlock()
	if !ok {
		t.Fatal("a read of an existing run left no handle behind; a later write would not share its lock")
	}

	again, err := j2.readRun("cached-run")
	if err != nil {
		t.Fatalf("readRun: %v", err)
	}
	if again != adopted {
		t.Fatal("a second read built a second handle for one run")
	}
	writer, err := j2.run("cached-run")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if writer != adopted {
		t.Fatal("the write path got a different handle from the read path")
	}
}
