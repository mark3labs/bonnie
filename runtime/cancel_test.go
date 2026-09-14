package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// blockingFactory returns a factory whose agent hangs until the test releases
// it, so an in-flight turn is observable.
func blockingFactory(turns ...*kit.TurnResult) (AgentFactory, *fakeAgent, chan struct{}, chan struct{}) {
	release := make(chan struct{})
	started := make(chan struct{})
	agent := &fakeAgent{turns: turns, block: release, started: started}
	return func(_ context.Context, s *Session) (Agent, error) {
		agent.session = s
		return agent, nil
	}, agent, release, started
}

// TestCancelStopsInFlightTurn is the point of T-005: an operator can stop a
// turn that would otherwise run to the model's own limits.
func TestCancelStopsInFlightTurn(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()
	f, _, release, started := blockingFactory(&kit.TurnResult{Response: "too late"})
	defer close(release)

	r := NewRunner(journal, f)
	ctx := context.Background()

	type result struct {
		run *Run
		err error
	}
	done := make(chan result, 1)
	go func() {
		run, err := r.Start(ctx, "cancel-1", Input{Text: "long job"})
		done <- result{run, err}
	}()

	<-started
	if err := r.Cancel("cancel-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Start after Cancel: %v", got.err)
		}
		if got.run.State != RunCancelled {
			t.Fatalf("state = %q, want %q", got.run.State, RunCancelled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not stop the turn")
	}

	// The journal must agree, or the run is stuck in "running" for ever.
	state, err := journal.State(ctx, "cancel-1")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != RunCancelled {
		t.Fatalf("journalled state = %q, want %q", state, RunCancelled)
	}
}

// TestCancelKeepsCompletedSteps states the persistence half of the claim. Kit
// journals a step's messages before it checks the context (docs/SPEC.md §3.3),
// so cancelling never throws away finished work.
func TestCancelKeepsCompletedSteps(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()
	release := make(chan struct{})
	started := make(chan struct{})
	agent := &fakeAgent{block: release, started: started}

	factory := func(_ context.Context, s *Session) (Agent, error) {
		agent.session = s
		// A step that finished before the cancel arrived.
		if _, err := s.AppendMessage(user("long job")); err != nil {
			return nil, err
		}
		if _, err := s.AppendMessage(assistant("step one done")); err != nil {
			return nil, err
		}
		return agent, nil
	}

	r := NewRunner(journal, factory)
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.Start(ctx, "cancel-2", Input{Text: "long job"}); err != nil {
			t.Errorf("Start: %v", err)
		}
	}()

	<-started
	if err := r.Cancel("cancel-2"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	<-done
	close(release)

	// A cancelled run restores to a conversation a provider accepts, so it can
	// be continued rather than abandoned.
	s, err := Restore(ctx, "cancel-2", journal)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	msgs := s.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("kept %d messages after cancel, want the finished step", len(msgs))
	}
	assertNoOrphan(t, msgs)
}

func TestCancelUnknownRun(t *testing.T) {
	t.Parallel()
	r := NewRunner(NewMemoryJournal(), nil)
	if err := r.Cancel("nobody"); !errors.Is(err, ErrRunNotActive) {
		t.Fatalf("err = %v, want ErrRunNotActive", err)
	}
}

// TestCancelledRunContinuesInASecondRunner is the claim [Runner.Cancel]
// makes, tested the way every durability claim must be: a second Runner that
// shares only the journal.
//
// A cancelled run is not a failed one. Kit persists a step's messages before
// it looks at the context (docs/SPEC.md §3.3), so the finished work is on
// disk and the conversation a resume rebuilds is provider-valid. Until this
// test, the claim rested on a Restore in the first process — which proves
// the records survive, not that another process can carry the run on.
func TestCancelledRunContinuesInASecondRunner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	journalA, err := OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}

	// Process A: a turn that is cancelled while it works.
	fa, _, release, started := blockingFactory(&kit.TurnResult{Response: "too late"})
	runA := NewRunner(journalA, fa)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := runA.Start(ctx, "cancel-resume", Input{Text: "long job"}); err != nil {
			t.Errorf("Start: %v", err)
		}
	}()

	<-started
	if err := runA.Cancel("cancel-resume"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	<-done
	close(release)

	// Process A dies. Closing the journal releases the run's flock, which is
	// what a dead process looks like to the next owner.
	if err := journalA.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Process B: a new Runner over the same directory, nothing else shared.
	journalB, err := OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = journalB.Close() }()

	if state, err := journalB.State(ctx, "cancel-resume"); err != nil {
		t.Fatalf("State: %v", err)
	} else if state != RunCancelled {
		t.Fatalf("state = %q, want %q: a cancelled run must not look failed", state, RunCancelled)
	}

	fb, _ := fakeFactory(&kit.TurnResult{Response: "finished after the cancel"})
	runB := NewRunner(journalB, fb)
	run, err := runB.Start(ctx, "cancel-resume", Input{Text: "carry on"})
	if err != nil {
		t.Fatalf("Start in the second Runner: %v", err)
	}
	if run.State != RunCompleted {
		t.Fatalf("state = %q, want %q", run.State, RunCompleted)
	}
	if run.Response != "finished after the cancel" {
		t.Fatalf("response = %q", run.Response)
	}

	// The conversation the second Runner built must be one a provider
	// accepts, and it must still hold what the cancelled turn finished.
	s, err := Restore(ctx, "cancel-resume", journalB)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	assertNoOrphan(t, s.GetMessages())
}

func TestCancelIdleRun(t *testing.T) {
	t.Parallel()
	f, _ := fakeFactory(&kit.TurnResult{Response: "ok"})
	r := NewRunner(NewMemoryJournal(), f)

	if _, err := r.Start(context.Background(), "idle", Input{Text: "hi"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The run exists and is finished — cancelling it is not a cancellation.
	if err := r.Cancel("idle"); !errors.Is(err, ErrRunNotActive) {
		t.Fatalf("err = %v, want ErrRunNotActive", err)
	}
}

// TestSteerReachesActiveTurn covers the cheap half of a steer policy: a message
// that arrives mid-turn joins the turn instead of restarting it.
func TestSteerReachesActiveTurn(t *testing.T) {
	t.Parallel()
	f, agent, release, started := blockingFactory(&kit.TurnResult{Response: "ok"})
	r := NewRunner(NewMemoryJournal(), f)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.Start(context.Background(), "steer-1", Input{Text: "go"}); err != nil {
			t.Errorf("Start: %v", err)
		}
	}()

	<-started
	if err := r.Steer("steer-1", "actually, use eu-west-1"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	close(release)
	<-done

	steers := agent.steered()
	if len(steers) != 1 || steers[0] != "actually, use eu-west-1" {
		t.Fatalf("steers = %v", steers)
	}
	if err := r.Steer("steer-1", "too late"); !errors.Is(err, ErrRunNotActive) {
		t.Fatalf("err = %v, want ErrRunNotActive", err)
	}
}

// TestConcurrentStartAndCancelIsRaceClean hammers the lock discipline that
// Cancel adds to the runner.
func TestConcurrentStartAndCancelIsRaceClean(t *testing.T) {
	t.Parallel()
	f, _, release, _ := blockingFactory()
	close(release) // never block: the turn finishes as fast as it can
	r := NewRunner(NewMemoryJournal(), f)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			runID := "race"
			if i%2 == 0 {
				_, _ = r.Start(context.Background(), runID, Input{Text: "hi"})
				return
			}
			_ = r.Cancel(runID)
		})
	}
	wg.Wait()
}

// TestRunnerPublishesLifecycleEvents is what the HTTP stream is built on. The
// bus is part of L1 so a transport never has to poll the journal.
func TestRunnerPublishesLifecycleEvents(t *testing.T) {
	t.Parallel()
	f, _ := fakeFactory(&kit.TurnResult{Response: "hello"})
	r := NewRunner(NewMemoryJournal(), f)

	events, unsubscribe := r.Events().Subscribe("ev-1", 0)
	defer unsubscribe()

	if _, err := r.Start(context.Background(), "ev-1", Input{Text: "hi"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var states []RunState
	var response string
	deadline := time.After(5 * time.Second)
	for len(states) < 2 {
		select {
		case ev := <-events:
			switch ev.Type {
			case EventState:
				states = append(states, ev.State)
			case EventResponse:
				response = ev.Text
			}
		case <-deadline:
			t.Fatalf("timed out after states %v", states)
		}
	}

	if states[0] != RunRunning || states[1] != RunCompleted {
		t.Fatalf("states = %v, want [running completed]", states)
	}
	if response != "hello" {
		t.Fatalf("response event = %q", response)
	}
}

// TestEventBusReplaysFromCursor is the reconnect contract: a client that drops
// and comes back with its cursor sees no gap and no duplicate. The events
// carry their journal anchors, which is what the cursor counts.
func TestEventBusReplaysFromCursor(t *testing.T) {
	t.Parallel()
	bus := NewEventBus(16)

	for i, text := range []string{"one", "two", "three"} {
		bus.Publish(Event{RunID: "r", Type: EventResponse, Seq: i + 1, Text: text})
	}

	events, unsubscribe := bus.Subscribe("r", 1)
	defer unsubscribe()

	var got []string
	for range 2 {
		select {
		case ev := <-events:
			got = append(got, ev.Text)
		case <-time.After(time.Second):
			t.Fatalf("timed out after %v", got)
		}
	}
	if len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Fatalf("replayed %v, want [two three]", got)
	}
}

// TestEventBusUnsubscribeClosesChannel keeps a dropped client from leaking a
// goroutine per connection.
func TestEventBusUnsubscribeClosesChannel(t *testing.T) {
	t.Parallel()
	bus := NewEventBus(4)

	events, unsubscribe := bus.Subscribe("r", 0)
	unsubscribe()

	select {
	case _, open := <-events:
		if open {
			// A buffered event may still arrive before the close.
			if _, open = <-events; open {
				t.Fatal("channel stayed open after unsubscribe")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("unsubscribe did not close the channel")
	}
	// Publishing after the last subscriber left must not panic.
	bus.PublishData("r", EventResponse, "after", nil)
}

// TestEventBusSlowSubscriberKeepsEvents states the queueing choice: a slow
// reader costs memory, never a lost event, because the cursor contract cannot
// survive a silent drop.
func TestEventBusSlowSubscriberKeepsEvents(t *testing.T) {
	t.Parallel()
	bus := NewEventBus(4096)

	events, unsubscribe := bus.Subscribe("r", 0)
	defer unsubscribe()

	const n = 500
	for i := range n {
		bus.Publish(Event{RunID: "r", Type: EventResponse, Seq: i + 1, Text: itoa(i + 1)})
	}
	for i := range n {
		select {
		case ev := <-events:
			if ev.Seq != i+1 {
				t.Fatalf("event %d has seq %d", i, ev.Seq)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("lost events after %d", i)
		}
	}
}
