package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// AppendMessage publishes an entry into the tree, then writes the journal
// sequence onto it once the record lands. LastMessageSeq reads that field
// under the read lock, so the write has to take the write lock — it used to
// happen outside, which is a data race on a field two goroutines touch.
//
// Run with -race: without the lock this reports
// "WARNING: DATA RACE ... Previous write at ... AppendMessage".
func TestAppendMessageSeqIsRaceFree(t *testing.T) {
	t.Parallel()
	s := NewSession("run-race", NewMemoryJournal())

	// The window is between the entry entering the tree and the sequence
	// landing on it, which is one journal write wide. A reader that runs a
	// fixed number of iterations finishes long before the writer and never
	// overlaps it, so the readers spin for exactly as long as the writer
	// runs.
	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					_ = s.LastMessageSeq()
				}
			}
		})
	}

	const n = 500
	for i := range n {
		if _, err := s.AppendMessage(kit.NewLLMUserMessage("m")); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
	}
	close(done)
	readers.Wait()

	// The anchor still has to be right, not merely race-free: the last
	// message's sequence is what a response event carries.
	if got := s.LastMessageSeq(); got != n {
		t.Fatalf("LastMessageSeq after %d appends = %d, want %d", n, got, n)
	}
}

// AppendStep does the same bookkeeping for a whole step and always took the
// lock for it. Pin that the two paths agree, so a later edit cannot fix one
// and leave the other.
func TestAppendStepSeqIsRaceFree(t *testing.T) {
	t.Parallel()
	s := NewSession("run-race-step", NewMemoryJournal())

	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					_ = s.LastMessageSeq()
				}
			}
		})
	}

	const steps = 250
	for range steps {
		if _, err := s.AppendStep(context.Background(), []kit.LLMMessage{
			kit.NewLLMUserMessage("a"),
			kit.NewLLMUserMessage("b"),
		}); err != nil {
			t.Fatalf("AppendStep: %v", err)
		}
	}
	close(done)
	readers.Wait()

	if got := s.LastMessageSeq(); got != steps*2 {
		t.Fatalf("LastMessageSeq after %d two-message steps = %d, want %d", steps, got, steps*2)
	}
}

// The event bus resolves an event's journal anchor BEFORE it takes its own
// lock, because the anchor reaches the journal and a SQLite read under the
// bus lock serialised every publisher and every subscribe against a disk
// read.
//
// An anchor that touches the bus is the sharp end of that: with the anchor
// called under the lock this deadlocks outright rather than merely running
// slowly, so the test states the property as liveness. A Runner's own
// anchor does not call back into the bus, but nothing in the exported API
// says it may not, and a deadlock is what the old shape would have cost.
func TestPublishDoesNotHoldTheLockAcrossTheAnchor(t *testing.T) {
	t.Parallel()
	bus := NewEventBus(8)

	var reentered bool
	bus.Anchor(func(runID string) int {
		// Any bus method that takes b.mu will do; Oldest is the cheapest.
		_ = bus.Oldest(runID)
		reentered = true
		return 7
	})

	done := make(chan Event, 1)
	go func() { done <- bus.Publish(Event{RunID: "r", Type: EventState, State: RunRunning}) }()

	select {
	case ev := <-done:
		if !reentered {
			t.Fatal("the anchor never ran")
		}
		if ev.Seq != 7 {
			t.Fatalf("Seq = %d, want the anchor's 7", ev.Seq)
		}
	case <-time.After(5 * time.Second):
		// Bounded here rather than left to the package timeout: a
		// deadlock should fail this test in seconds and name the cause,
		// not hang the whole binary until `go test` kills it.
		t.Fatal("Publish deadlocked: the anchor was called while the bus lock was held")
	}
}

// Moving the anchor out of the lock must not change what an event carries:
// an unstamped event still gets the journal position, and a caller that set
// Seq itself still wins.
func TestPublishStampsFromTheAnchorUnlessTheCallerSetSeq(t *testing.T) {
	t.Parallel()
	bus := NewEventBus(8)
	bus.Anchor(func(string) int { return 42 })

	if ev := bus.Publish(Event{RunID: "r", Type: EventState}); ev.Seq != 42 {
		t.Fatalf("an unstamped event got Seq %d, want the anchor's 42", ev.Seq)
	}
	if ev := bus.Publish(Event{RunID: "r", Type: EventState, Seq: 9}); ev.Seq != 9 {
		t.Fatalf("a caller-stamped event got Seq %d, want its own 9", ev.Seq)
	}

	// A bus with no anchor leaves events unstamped, which is the
	// live-only behaviour a standalone bus has always had.
	plain := NewEventBus(8)
	if ev := plain.Publish(Event{RunID: "r", Type: EventState}); ev.Seq != 0 {
		t.Fatalf("a bus with no anchor stamped Seq %d, want 0", ev.Seq)
	}
}

// Publish and Anchor may run at the same time: the anchor is read under the
// lock and then called outside it, so neither racing nor a nil dereference
// is possible.
func TestAnchorIsRaceFreeAgainstPublish(t *testing.T) {
	t.Parallel()
	bus := NewEventBus(64)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			bus.Publish(Event{RunID: "r", Type: EventState, State: RunRunning})
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 200 {
			bus.Anchor(func(string) int { return i })
		}
	}()
	wg.Wait()
}
