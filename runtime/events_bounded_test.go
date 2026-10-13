package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBoundedEventOverflowUnsubscribes(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(map[bool]string{false: "count", true: "bytes"}[large], func(t *testing.T) {
			b := NewEventBus(8)
			events, terminal, stop := b.SubscribeBounded("run", 0, 2, 1024)
			defer stop()
			if large {
				b.Publish(Event{RunID: "run", Text: strings.Repeat("x", 2048)})
			} else {
				for range 10 {
					b.Publish(Event{RunID: "run", Text: "event"})
				}
			}
			select {
			case err := <-terminal:
				if !errors.Is(err, ErrEventOverflow) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("no overflow")
			}
			select {
			case _, ok := <-events:
				if ok {
					t.Fatal("overflow still delivered")
				}
			case <-time.After(time.Second):
				t.Fatal("pump leak")
			}
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				b.mu.Lock()
				n := len(b.subs["run"])
				b.mu.Unlock()
				if n == 0 {
					stop()
					stop()
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("subscription leaked")
		})
	}
}
func TestBoundedPreservesSameAnchor(t *testing.T) {
	b := NewEventBus(8)
	events, terminal, stop := b.SubscribeBounded("run", 1, 8, 4096)
	defer stop()
	for _, text := range []string{"one", "two", "three"} {
		b.Publish(Event{RunID: "run", Seq: 2, Text: text})
	}
	for _, text := range []string{"one", "two", "three"} {
		select {
		case ev := <-events:
			if ev.Text != text {
				t.Fatalf("got %q want %q", ev.Text, text)
			}
		case err := <-terminal:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("missing event")
		}
	}
}

// The second runner has only the journal, not the first runner's live bus.
func TestBoundedReplayAcrossRunners(t *testing.T) {
	journal := NewMemoryJournal()
	first := NewRunner(journal, nil)
	first.bus.Publish(Event{RunID: "replay", Seq: 1, Type: EventState, State: RunCompleted})
	if _, err := journal.Append(t.Context(), Record{RunID: "replay", Kind: RecordState, State: RunCompleted}); err != nil {
		t.Fatal(err)
	}
	second := NewRunner(journal, nil)
	events, terminal, stop := second.StreamEventsBounded(t.Context(), "replay", 0, 8, 4096)
	defer stop()
	select {
	case ev := <-events:
		if ev.Type != EventState || ev.State != RunCompleted {
			t.Fatalf("replay %+v", ev)
		}
	case err := <-terminal:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("no replay")
	}
}
