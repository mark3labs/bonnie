package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

type submissionMarkerFailure struct {
	*MemoryJournal
	fail bool
}

func (j *submissionMarkerFailure) AppendStep(ctx context.Context, records []Record) ([]int, error) {
	if j.fail {
		return nil, errors.New("marker save failed")
	}
	return j.MemoryJournal.AppendStep(ctx, records)
}

// A failed marker must release both the planning lock and acquired turn. The
// next delivery can proceed, and a turn can itself submit without deadlock.
func TestSubmissionPlanningReleasesBeforeStart(t *testing.T) {
	t.Parallel()
	j := &submissionMarkerFailure{MemoryJournal: NewMemoryJournal()}
	f, agent := fakeFactory(&kit.TurnResult{Response: "done"})
	r := NewRunner(j, f)
	if _, err := r.Submit(t.Context(), "r", "first", Input{Text: "work"}, BusyQueue); err != nil {
		t.Fatal(err)
	}
	j.fail = true
	if err := r.deliverSubmission(t.Context(), "r"); err == nil || err.Error() != "marker save failed" {
		t.Fatalf("delivery error = %v", err)
	}
	if !r.workMu.TryLock() {
		t.Fatal("planning lock remained held after marker failure")
	}
	r.workMu.Unlock()
	r.mu.Lock()
	_, active := r.active["r"]
	r.mu.Unlock()
	if active {
		t.Fatal("failed marker retained the acquired turn")
	}
	j.fail = false
	agent.onPrompt = func(*Session) error {
		_, err := r.Submit(t.Context(), "r", "second", Input{Text: "follow up"}, BusyQueue)
		return err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.deliverSubmission(ctx, "r") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn could not Submit while delivery was active")
	}
	items, err := r.Submissions(t.Context(), "r")
	if err != nil || len(items) != 2 || items[0].State != SubmissionDone || items[1].State != SubmissionQueued {
		t.Fatalf("submissions = %+v, %v", items, err)
	}
}
