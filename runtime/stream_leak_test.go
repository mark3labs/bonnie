package runtime

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// waitForGoroutines waits until the count drops to want, or fails. Goroutine
// teardown is asynchronous, so a bare count straight after the stop is a
// flake; a count that never drops is the leak.
func waitForGoroutines(t *testing.T, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := runtime.NumGoroutine()
		if got <= want {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("%s: %d goroutines still running, want at most %d\n%s", what, got, want, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStreamEventsStopReleasesAParkedSender is the leak this seam exists to
// prevent. An HTTP client disconnects between two events: the handler stops
// reading and calls stop. The forwarding goroutine is parked on its send and
// the subscriber's pump is parked behind it, so before the fix a public
// server leaked two goroutines and their queued events on every disconnect
// that raced an event — for the life of the process.
func TestStreamEventsStopReleasesAParkedSender(t *testing.T) {
	// Not parallel: it counts goroutines.
	ctx := context.Background()

	fa, _ := fakeFactory(&kit.TurnResult{Response: "done"})
	r := NewRunner(NewMemoryJournal(), fa)
	if _, err := r.Start(ctx, "leak-live", Input{Text: "hi"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	base := runtime.NumGoroutine()

	for range 20 {
		// Cursor 0 on a run with a backlog: the events are queued into the
		// subscriber at once, and nothing reads them. Both goroutines park.
		_, stop := r.StreamEvents("leak-live", 0)
		time.Sleep(time.Millisecond)
		stop()
	}

	waitForGoroutines(t, base, "after 20 streams were stopped without being read")
}

// TestStreamEventsStopReleasesAParkedReplay covers the wider window: a
// client that goes away during journal catch-up. Replay pushes one event per
// record, so a long history parks the forwarder on the first send and the
// whole catch-up is stuck behind it.
func TestStreamEventsStopReleasesAParkedReplay(t *testing.T) {
	// Not parallel: it counts goroutines.
	ctx := context.Background()

	turns := make([]*kit.TurnResult, 0, 12)
	for i := range 12 {
		turns = append(turns, &kit.TurnResult{Response: fmt.Sprintf("turn %d", i+1)})
	}
	fa, _ := fakeFactory(turns...)
	r := NewRunner(NewMemoryJournal(), fa, WithEventBuffer(4))
	for i := range 12 {
		if _, err := r.Start(ctx, "leak-replay", Input{Text: fmt.Sprintf("msg %d", i+1)}); err != nil {
			t.Fatalf("Start %d: %v", i+1, err)
		}
	}
	if r.bus.Oldest("leak-replay") <= 1 {
		t.Fatal("the backlog did not move past the start, so this test does not reach the replay path")
	}

	base := runtime.NumGoroutine()

	for range 20 {
		events, stop := r.StreamEvents("leak-replay", 0)
		// Read one event, so the stream is mid-replay, then leave.
		select {
		case <-events:
		case <-time.After(2 * time.Second):
			stop()
			t.Fatal("no event arrived: the replay never started")
		}
		stop()
	}

	waitForGoroutines(t, base, "after 20 streams were abandoned mid-replay")
}

// TestStreamEventsStopIsSafeTwice pins that the returned stop stays callable
// more than once, which is what a deferred stop plus an explicit one does.
func TestStreamEventsStopIsSafeTwice(t *testing.T) {
	t.Parallel()
	fa, _ := fakeFactory(&kit.TurnResult{Response: "done"})
	r := NewRunner(NewMemoryJournal(), fa)
	if _, err := r.Start(context.Background(), "stop-twice", Input{Text: "hi"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, stop := r.StreamEvents("stop-twice", 0)
	stop()
	stop()
}
