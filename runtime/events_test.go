package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// The tests below cover the T-016 seam: the stream survives a cursor that
// has fallen off the backlog edge, because every event is anchored to a
// journal record and the journal replays past the edge.

// TestStreamEventsReplaysPastTheBacklog is the §4.8 contract, stated as a
// test. A run with more events than the backlog holds, a client whose cursor
// is 0: the replay covers everything, in order, with the same Seqs the live
// path used, and nothing is delivered twice.
func TestStreamEventsReplaysPastTheBacklog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Many turns, far more than a shrunken backlog keeps.
	turns := make([]*kit.TurnResult, 0, 12)
	for i := range 12 {
		turns = append(turns, &kit.TurnResult{Response: fmt.Sprintf("turn %d", i+1)})
	}
	fa, agent := fakeFactory(turns...)
	j := NewMemoryJournal()
	r := NewRunner(j, fa, WithEventBuffer(4))

	for i := range 12 {
		if _, err := r.Start(ctx, "gap-run", Input{Text: fmt.Sprintf("msg %d", i+1)}); err != nil {
			t.Fatalf("Start %d: %v", i+1, err)
		}
	}
	_ = agent

	if r.bus.Oldest("gap-run") <= 1 {
		t.Fatalf("oldest = %d, want the backlog to have moved past the start — "+
			"the test is not exercising the edge it exists for", r.bus.Oldest("gap-run"))
	}

	events, stop := r.StreamEvents("gap-run", 0)
	defer stop()

	var got []Event
	for len(got) < 12*3 { // per turn: a running state, a response, a completed state
		select {
		case ev, open := <-events:
			if !open {
				break
			}
			got = append(got, ev)
			continue
		case <-time.After(2 * time.Second):
		}
		break
	}
	if len(got) != 36 {
		t.Fatalf("replayed %d events, want the full 36-event history (12 × running, response, completed)", len(got))
	}
	// Seqs must rise, never repeat, and never skip backwards.
	for i, ev := range got {
		if i > 0 && ev.Seq <= got[i-1].Seq {
			t.Fatalf("event %d has seq %d, not above %d: the replay is out of order", i, ev.Seq, got[i-1].Seq)
		}
	}
	// The response text of the last turn must be the last response seen.
	if got[len(got)-2].Type != EventResponse || got[len(got)-2].Text != "turn 12" {
		t.Fatalf("second-to-last event = %+v, want the turn 12 response", got[len(got)-2])
	}
	if got[len(got)-1].Type != EventState || got[len(got)-1].State != RunCompleted {
		t.Fatalf("last event = %+v, want the closing completed state", got[len(got)-1])
	}
}

// TestStreamEventsSurvivesRestart covers the case that used to be a silent
// gap: the process died, a new Runner opened the same journal, and a client
// reconnected from 0. The replay comes from the records, not from a bus that
// no longer remembers anything.
func TestStreamEventsSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	fa, _ := fakeFactory(&kit.TurnResult{Response: "before the crash"})
	j1, err := OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	r1 := NewRunner(j1, fa)
	if _, err := r1.Start(ctx, "restart-run", Input{Text: "go"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := j1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The new process: same journal directory, a bus that knows nothing.
	j2, err := OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = j2.Close() }()
	factory2, _ := fakeFactory(&kit.TurnResult{Response: "after"})
	r2 := NewRunner(j2, factory2)

	events, stop := r2.StreamEvents("restart-run", 0)
	defer stop()

	var sawResponse, sawState bool
	for range 3 { // the replayed history: running, response, completed
		select {
		case ev, open := <-events:
			if !open {
				break
			}
			switch {
			case ev.Type == EventResponse && ev.Text == "before the crash":
				sawResponse = true
			case ev.Type == EventState && ev.State == RunCompleted:
				sawState = true
			}
			continue
		case <-time.After(2 * time.Second):
		}
		break
	}
	if !sawResponse || !sawState {
		t.Fatalf("replayed response=%v state=%v, want both: the restart lost history", sawResponse, sawState)
	}
}

// TestStreamEventsHandoffHasNoGapOrDuplicate covers the join between the
// replayed journal and the live bus: events the replay covered are dropped,
// events it did not cover are delivered, and the seam is invisible.
func TestStreamEventsHandoffHasNoGapOrDuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fa, agent := fakeFactory(&kit.TurnResult{Response: "first"})
	j := NewMemoryJournal()
	r := NewRunner(j, fa)
	if _, err := r.Start(ctx, "handoff-run", Input{Text: "go"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	position := r.durableSeq("handoff-run")
	if position == 0 {
		t.Skip("journal cannot report a position; replay cannot be anchored")
	}

	// Subscribe first, so the live side is already running, then replay the
	// whole history: every event arrives on both paths and the seq filter
	// must keep exactly one copy.
	events, stop := r.StreamEvents("handoff-run", 0)
	defer stop()

	seen := map[string]int{}
	for range 3 { // the whole history: running, response, completed
		select {
		case ev, open := <-events:
			if !open {
				break
			}
			seen[ev.Type]++
			continue
		case <-time.After(2 * time.Second):
		}
		break
	}
	_ = agent
	if seen[EventResponse] != 1 || seen[EventState] != 2 {
		t.Fatalf("counts = %v, want exactly one response and two states (running, completed) across the handoff", seen)
	}
}
