package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestBoundedObserverOverflowAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		events, bytes int
		payload       string
	}{{1, 1024, "x"}, {100, 100, strings.Repeat("x", 200)}} {
		bus := NewEventBus(4)
		out, fail, stop := bus.SubscribeBounded("run", 0, tc.events, tc.bytes)
		for range 10 {
			bus.Publish(Event{RunID: "run", Text: tc.payload})
		}
		select {
		case err := <-fail:
			if !errors.Is(err, ErrEventOverflow) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("no overflow")
		}
		stop()
		stop()
		select {
		case _, ok := <-out:
			if ok {
				t.Fatal("closed observer delivered")
			}
		case <-time.After(time.Second):
			t.Fatal("observer did not close")
		}
		bus.mu.Lock()
		n := len(bus.subs)
		bus.mu.Unlock()
		if n != 0 {
			t.Fatal("subscription leaked")
		}
	}
}
func TestBoundedStreamSameAnchorAndReplay(t *testing.T) {
	factory, _ := fakeFactory(&kit.TurnResult{Response: "done"})
	r := NewRunner(NewMemoryJournal(), factory, WithEventBuffer(4))
	if _, err := r.Start(t.Context(), "run", Input{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	out, fail, stop := r.StreamEventsBounded(t.Context(), "run", 0, 256, 4<<20)
	defer stop()
	select {
	case ev := <-out:
		if ev.RunID != "run" {
			t.Fatal(ev)
		}
	case err := <-fail:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("no replay")
	}
	stop()
	// A live-only stream preserves all events with a shared journal anchor.
	out, fail, stop = r.StreamEventsBounded(t.Context(), "live", 0, 256, 4<<20)
	defer stop()
	for range 3 {
		r.Events().Publish(Event{RunID: "live", Seq: 42, Type: "tool"})
	}
	for range 3 {
		select {
		case ev := <-out:
			if ev.Seq != 42 {
				t.Fatal(ev)
			}
		case err := <-fail:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("same-anchor event lost")
		}
	}
}
func TestBoundedStopReleasesReplay(t *testing.T) {
	factory, _ := fakeFactory(&kit.TurnResult{Response: "done"})
	r := NewRunner(NewMemoryJournal(), factory, WithEventBuffer(1))
	if _, err := r.Start(context.Background(), "run", Input{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		out, _, stop := r.StreamEventsBounded(t.Context(), "run", 0, 256, 4<<20)
		select {
		case <-out:
		case <-time.After(time.Second):
			t.Fatal("no replay")
		}
		stop()
		stop()
	}
}

// A live-only event anchored at the replay watermark follows the replay and
// must not be suppressed as a duplicate; replayed types at that anchor are.
func TestReplayHandoffKeepsLiveOnlyEventsAtWatermark(t *testing.T) {
	t.Parallel()
	factory, _ := fakeFactory(&kit.TurnResult{Response: "done"})
	r := NewRunner(NewMemoryJournal(), factory, WithEventBuffer(1))
	if _, err := r.Start(t.Context(), "run", Input{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	watermark := r.durableSeq("run")
	check := func(t *testing.T, out <-chan Event, fail <-chan error) {
		t.Helper()
		// Publish a duplicate of the replayed closing state and a live-only
		// event at the same anchor; only the live-only one may come through.
		r.Events().Publish(Event{RunID: "run", Seq: watermark, Type: EventState, State: RunCompleted})
		r.Events().Publish(Event{RunID: "run", Seq: watermark, Type: "tool"})
		states := 0
		for {
			select {
			case ev := <-out:
				if ev.Type == EventState && ev.Seq == watermark {
					states++
				}
				if ev.Type == "tool" {
					if states != 1 {
						t.Fatalf("saw %d closing states before the live event, want 1", states)
					}
					return
				}
			case err := <-fail:
				t.Fatal(err)
			case <-time.After(time.Second):
				t.Fatal("live-only event at the watermark was dropped")
			}
		}
	}
	t.Run("bounded", func(t *testing.T) {
		out, fail, stop := r.StreamEventsBounded(t.Context(), "run", 0, 256, 4<<20)
		defer stop()
		check(t, out, fail)
	})
	t.Run("unbounded", func(t *testing.T) {
		out, stop := r.StreamEvents("run", 0)
		defer stop()
		check(t, out, nil)
	})
}
