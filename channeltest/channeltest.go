// Package channeltest is the conformance suite for channel adapters, in the
// same spirit as the journal and sandbox suites.
//
// An adapter proves itself by joining, not by writing its own tests:
//
//	func TestMyAdapter(t *testing.T) {
//		channeltest.RunConformance(t, func(t *testing.T) *channeltest.Fixture {
//			// build the adapter over a fresh journal and script agent
//		})
//	}
//
// The suite drives the [channel.Inbound] contract only — address resolution,
// attach versus from, turn policies, suspension, cancellation — with a
// scripted agent standing in for the model. It never opens a port and never
// needs credentials.
//
// A case an adapter cannot satisfy on some platform should skip inside the
// build function, not fail: the contract is what a transport must do, and a
// platform that cannot do a thing must say so loudly and specifically.
package channeltest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// ScriptAgent is a deterministic [runtime.Agent] for conformance cases. It
// runs scripted turns in order, can hold a turn open so a test can make a
// turn active, records steered messages, and parks a run when a scripted
// turn carries a [runtime.SuspendRequest].
type ScriptAgent struct {
	mu    sync.Mutex
	turns []*kit.TurnResult
	call  int

	// hold, when non-nil, makes PromptResult wait until released, exactly
	// once, after signalling active. This is how a test makes a turn be
	// "already running".
	hold   chan struct{}
	active chan struct{}

	session *runtime.Session
	steers  []string
}

var _ runtime.Agent = (*ScriptAgent)(nil)

// NewScriptAgent returns an agent that answers every turn with "done" until
// turns are scripted.
func NewScriptAgent() *ScriptAgent {
	return &ScriptAgent{}
}

// Say scripts the next turn response. A turn whose TurnResult carries a
// FinalValue parks the run.
func (a *ScriptAgent) Say(res *kit.TurnResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turns = append(a.turns, res)
}

// HoldOpen makes the next turn block until Release, and returns the channel
// that closes once that turn is running — the moment [runtime.Runner.IsActive]
// is true. Use it to hold a turn open across a concurrent Send.
//
// The returned closure captures its own channel, so calling it after the turn
// already consumed the hold — a test that releases late — stays safe.
func (a *ScriptAgent) HoldOpen() (release func(), active <-chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	hold, activeCh := make(chan struct{}), make(chan struct{})
	a.hold, a.active = hold, activeCh
	return func() { close(hold) }, activeCh
}

// Steered returns the messages InjectSteer received, in order.
func (a *ScriptAgent) Steered() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.steers...)
}

// Calls reports how many turns ran. A steered message must not advance it;
// a queued message must.
func (a *ScriptAgent) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.call
}

// PromptResult implements [runtime.Agent].
func (a *ScriptAgent) PromptResult(ctx context.Context, msg string) (*kit.TurnResult, error) {
	a.mu.Lock()
	if a.hold != nil {
		hold, active := a.hold, a.active
		a.hold, a.active = nil, nil
		a.mu.Unlock()

		close(active)
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		a.mu.Unlock()
	}

	a.mu.Lock()
	call := a.call
	a.call++
	turns := a.turns
	session := a.session
	a.mu.Unlock()

	if session != nil {
		assistant := kit.LLMMessage{
			Role:    kit.LLMMessageRole("assistant"),
			Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: msg}},
		}
		if _, err := session.AppendMessage(assistant); err != nil {
			return nil, err
		}
	}
	if call < len(turns) {
		return turns[call], nil
	}
	return &kit.TurnResult{Response: "done"}, nil
}

// InjectSteer implements [runtime.Agent].
func (a *ScriptAgent) InjectSteer(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steers = append(a.steers, msg)
}

// Close implements [runtime.Agent].
func (a *ScriptAgent) Close() error { return nil }

// Fixture is what an adapter hands the conformance suite: the inbound side
// under test, the script agent driving it, and the journal beneath both.
type Fixture struct {
	Inbound channel.Inbound
	Agent   *ScriptAgent
	Journal runtime.Journal
}

// RunConformance runs every conformance case against the adapter that build
// returns. Call it from the adapter's own test with a build function that
// wires a fresh journal, a fresh [ScriptAgent], and the adapter.
func RunConformance(t *testing.T, build func(t *testing.T) *Fixture) {
	t.Helper()
	t.Run("from creates and resolves one run per address", func(t *testing.T) {
		f := build(t)
		ctx := context.Background()

		first := f.Inbound.From("addr-1")
		run, err := first.Send(ctx, "hello", channel.SendOptions{})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		id, err := first.RunID(ctx)
		if err != nil {
			t.Fatalf("RunID: %v", err)
		}
		if id != run.ID {
			t.Fatalf("RunID = %q, want the run the address created (%q)", id, run.ID)
		}

		// The same address resolves to the same run; a different address
		// must not share it.
		again, err := f.Inbound.From("addr-1").RunID(ctx)
		if err != nil || again != id {
			t.Fatalf("second resolve = %q, %v, want %q", again, err, id)
		}
		other, err := f.Inbound.From("addr-2").Send(ctx, "hi", channel.SendOptions{})
		if err != nil {
			t.Fatalf("Send on a second address: %v", err)
		}
		if other.ID == run.ID {
			t.Fatal("two addresses resolved to one run")
		}
	})

	t.Run("attach never creates", func(t *testing.T) {
		f := build(t)
		ctx := context.Background()

		if _, err := f.Inbound.Attach("no-such-run").Send(ctx, "hi", channel.SendOptions{}); !errors.Is(err, runtime.ErrRunNotFound) {
			t.Fatalf("err = %v, want runtime.ErrRunNotFound: attach must never create", err)
		}
		// And the failure is the one the contract names, so an adapter can
		// map it to its own not-found.
		if _, err := f.Inbound.Attach("no-such-run").RunID(ctx); err == nil {
			t.Fatal("Attach RunID to an unknown run must fail")
		}
	})

	t.Run("steer injects into the running turn", func(t *testing.T) {
		f := build(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		release, active := f.Agent.HoldOpen()
		started := make(chan *runtime.Run, 1)
		go func() {
			run, err := f.Inbound.From("steer-addr").Send(ctx, "long work", channel.SendOptions{})
			if err != nil {
				run = &runtime.Run{Err: err}
			}
			started <- run
		}()

		select {
		case <-active:
		case <-time.After(10 * time.Second):
			t.Fatal("the turn never became active")
		}

		ref := f.Inbound.From("steer-addr")
		if _, err := ref.Send(ctx, "change of plan", channel.SendOptions{
			TurnPolicy: channel.PolicySteer,
		}); err != nil {
			t.Fatalf("steered Send: %v", err)
		}
		release()

		run := <-started
		if run.Err != nil {
			t.Fatalf("turn failed: %v", run.Err)
		}
		if steered := f.Agent.Steered(); len(steered) != 1 || steered[0] != "change of plan" {
			t.Fatalf("steered = %v, want the message injected once", steered)
		}
		// Steering must not have started a second turn.
		if calls := f.Agent.Calls(); calls != 1 {
			t.Fatalf("agent ran %d turns, want 1: steering must not queue", calls)
		}
	})

	t.Run("queue waits for the turn to finish", func(t *testing.T) {
		f := build(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		release, active := f.Agent.HoldOpen()
		first := f.Inbound.From("queue-addr")
		go func() {
			_, _ = first.Send(ctx, "long work", channel.SendOptions{})
			// The turn ends; the lock releases.
		}()

		select {
		case <-active:
		case <-time.After(10 * time.Second):
			t.Fatal("the turn never became active")
		}

		// Queue: this Send must wait for the first turn to end, then run
		// as a new turn.
		done := make(chan *runtime.Run, 1)
		go func() {
			run, err := refSend(ctx, f.Inbound.From("queue-addr"), channel.PolicyQueue)
			if err != nil {
				run = &runtime.Run{Err: err}
			}
			done <- run
		}()

		// The queued send must NOT complete while the first turn runs.
		select {
		case run := <-done:
			t.Fatalf("the queued message ran while the turn was active: %+v", run)
		case <-time.After(300 * time.Millisecond):
		}

		release()
		run := <-done
		if run.Err != nil {
			t.Fatalf("queued turn failed: %v", run.Err)
		}
		if calls := f.Agent.Calls(); calls != 2 {
			t.Fatalf("agent ran %d turns, want 2: the queued message must run as its own turn", calls)
		}
	})

	t.Run("unknown turn policy is refused", func(t *testing.T) {
		f := build(t)
		ctx := context.Background()

		if _, err := refSend(ctx, f.Inbound.From("policy-addr"), "nope"); err == nil {
			t.Fatal("an unknown policy must be refused, never guessed")
		}
	})

	t.Run("respond answers a suspended run", func(t *testing.T) {
		f := build(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ref := f.Inbound.From("sus-addr")
		f.Agent.Say(&kit.TurnResult{
			Response:   "Which region?",
			FinalValue: runtime.SuspendRequest{Kind: "ask", Prompt: "Which region?"},
		})
		parked, err := refSend(ctx, ref, channel.PolicyQueue)
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if parked.State != runtime.RunWaiting {
			t.Fatalf("state = %q, want waiting", parked.State)
		}

		f.Agent.Say(&kit.TurnResult{Response: "Deployed to eu-west-1."})
		resumed, err := ref.Respond(ctx, []runtime.InputResponse{{Text: "eu-west-1"}})
		if err != nil {
			t.Fatalf("Respond: %v", err)
		}
		if resumed.State != runtime.RunCompleted {
			t.Fatalf("state after respond = %q, want completed", resumed.State)
		}
	})

	t.Run("cancel stops the turn", func(t *testing.T) {
		f := build(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		release, active := f.Agent.HoldOpen()
		ref := f.Inbound.From("cancel-addr")
		startRef := f.Inbound.From("cancel-addr")
		started := make(chan error, 1)
		go func() {
			_, err := refSend(ctx, startRef, channel.PolicyQueue)
			started <- err
		}()
		select {
		case <-active:
		case <-time.After(10 * time.Second):
			t.Fatal("the turn never became active")
		}

		if err := ref.Cancel(ctx); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		release()

		select {
		case err := <-started:
			// A cancelled turn is not a failed one: the runner reports
			// RunCancelled and Send returns without an error.
			if err != nil {
				t.Fatalf("Send after cancel: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the turn never returned after cancel")
		}
	})
}

// refSend is Send with an explicit policy, so the table cases stay flat.
func refSend(ctx context.Context, ref channel.SessionRef, policy channel.TurnPolicy) (*runtime.Run, error) {
	return ref.Send(ctx, "message", channel.SendOptions{TurnPolicy: policy})
}
