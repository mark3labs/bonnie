package runtime

import (
	"context"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// TestTurnSurvivesCallerCancellation is the durability claim at its
// narrowest: the caller that started a turn goes away, and the turn still
// reaches the journal with an answer.
//
// This is not a theoretical case. Every inbound transport has a caller whose
// lifetime is shorter than a turn's: an HTTP client that hangs up, a webhook
// handler that must answer the platform in seconds, a CLI a person
// interrupts. Before [Runner.acquire] detached the turn context, each of
// them killed a durable run by leaving, and the journal kept the run at its
// last checkpoint instead of at a response.
//
// Cancelling a turn stays possible; it is [Runner.Cancel] and nothing else.
// TestCancelStopsInFlightTurn holds that end.
func TestTurnSurvivesCallerCancellation(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()
	f, _, release, started := blockingFactory(&kit.TurnResult{Response: "finished anyway"})
	r := NewRunner(journal, f)

	ctx, cancel := context.WithCancel(context.Background())

	type result struct {
		run *Run
		err error
	}
	done := make(chan result, 1)
	go func() {
		run, err := r.Start(ctx, "detach-1", Input{Text: "long job"})
		done <- result{run, err}
	}()

	// The turn is running. The caller now goes away mid-turn.
	<-started
	cancel()

	// Nothing should have changed for the turn: it is still blocked on the
	// agent, not unwound by the cancellation.
	select {
	case got := <-done:
		t.Fatalf("the turn ended when its caller left: %+v, %v", got.run, got.err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Start: %v", got.err)
		}
		if got.run.State != RunCompleted {
			t.Fatalf("state = %q, want %q: the turn must finish without its caller", got.run.State, RunCompleted)
		}
		if got.run.Response != "finished anyway" {
			t.Fatalf("response = %q, want the model's answer", got.run.Response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never finished after the agent was released")
	}

	// The durable record is what matters: a later process must find the
	// answer, not a run stuck mid-turn.
	state, err := journal.State(context.Background(), "detach-1")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != RunCompleted {
		t.Fatalf("journalled state = %q, want %q", state, RunCompleted)
	}
}

// TestCallerDeadlineDoesNotBoundTheTurn is the same claim for a deadline
// rather than an explicit cancel. A handler with a short timeout on its
// request context must not impose that timeout on a turn that may run for
// minutes, or the shape of the transport decides how long the agent may
// think.
func TestCallerDeadlineDoesNotBoundTheTurn(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()
	f, _, release, started := blockingFactory(&kit.TurnResult{Response: "slow but done"})
	r := NewRunner(journal, f)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan *Run, 1)
	go func() {
		run, err := r.Start(ctx, "detach-2", Input{Text: "long job"})
		if err != nil {
			run = &Run{Err: err}
		}
		done <- run
	}()

	<-started
	// Let the caller's deadline pass while the turn is still working.
	time.Sleep(150 * time.Millisecond)
	close(release)

	select {
	case run := <-done:
		if run.Err != nil {
			t.Fatalf("Start: %v", run.Err)
		}
		if run.State != RunCompleted {
			t.Fatalf("state = %q, want %q: a caller's deadline must not bound a turn", run.State, RunCompleted)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never finished")
	}
}
