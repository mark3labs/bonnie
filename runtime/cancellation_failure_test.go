package runtime

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

type cancelFailJournal struct{ Journal }

func (j cancelFailJournal) Append(ctx context.Context, rec Record) (int, error) {
	if rec.Kind == RecordCancel {
		return 0, errors.New("disk full")
	}
	return j.Journal.Append(ctx, rec)
}

// A refused journal write must not acknowledge or signal cancellation.
func TestCancellationRequiresDurableWrite(t *testing.T) {
	t.Parallel()
	factory, _, release, started := blockingFactory(&kit.TurnResult{Response: "done"})
	r := NewRunner(cancelFailJournal{NewMemoryJournal()}, factory)
	done := make(chan *Run, 1)
	go func() {
		run, err := r.Start(t.Context(), "r", Input{Text: "work"})
		if err != nil {
			t.Error(err)
		}
		done <- run
	}()
	<-started
	if _, err := r.RequestCancel(t.Context(), "r", ""); err == nil {
		t.Fatal("accepted a refused write")
	}
	close(release)
	if run := <-done; run == nil || run.State != RunCompleted {
		t.Fatalf("boundary: %+v", run)
	}
}
