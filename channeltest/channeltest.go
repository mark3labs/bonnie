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
// # Mandatory cases and capabilities
//
// Most cases are mandatory: they state what any transport must do, and an
// adapter that fails one is broken rather than limited.
//
// A case that states an OPTIONAL behaviour is gated on a [Capability]. An
// adapter names the capabilities it does not have in
// [Fixture.Unsupported], and the suite skips exactly those cases with a
// message that names the capability. This is the same rule the suite always
// had — "a case an adapter cannot satisfy should skip, not fail" — moved out
// of each adapter's build function and into one declaration, so the set of
// skips across the adapters reads as a parity matrix instead of scattered
// t.Skip calls.
//
// Declare a capability only when a real transport can genuinely lack it. A
// capability every adapter claims is a mandatory case wearing a disguise.
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

// Factory returns a [runtime.AgentFactory] that hands out this agent bound
// to each turn's session, so the agent's messages and compactions reach
// the journal the way a real Kit's do. Fixtures should build their runner
// with it.
func (a *ScriptAgent) Factory() runtime.AgentFactory {
	return func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
		a.mu.Lock()
		a.session = s
		a.mu.Unlock()
		return a, nil
	}
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

// Compact implements [runtime.Compactor] with a summary that keeps nothing,
// which is what a summary of everything so far amounts to. It lets the
// suite prove an adapter's Compact reaches the runner and the journal.
func (a *ScriptAgent) Compact(context.Context, *kit.CompactionOptions, string) (*kit.CompactionResult, error) {
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session == nil {
		return &kit.CompactionResult{}, nil
	}
	_, err := session.AppendCompaction("scripted summary", "", 0, 0, 0, nil, nil)
	return &kit.CompactionResult{}, err
}

var _ runtime.Compactor = (*ScriptAgent)(nil)

// Fixture is what an adapter hands the conformance suite: the inbound side
// under test, the script agent driving it, and the journal beneath both.
type Fixture struct {
	Inbound channel.Inbound
	Agent   *ScriptAgent
	Journal runtime.Journal

	// Unsupported names the optional capabilities this adapter does not
	// have. The suite skips their cases and runs everything else. An empty
	// slice — the usual case — claims every capability.
	//
	// Name a capability here only when the transport or its runner truly
	// cannot do the thing. Silencing a case that fails for another reason
	// hides a defect behind a skip, which is worse than a red test.
	Unsupported []Capability
}

// Capability names an optional behaviour a channel adapter may or may not
// have. Cases for a capability are skipped for an adapter that lists it in
// [Fixture.Unsupported].
type Capability string

// CapCompaction is on-demand compaction: [channel.SessionRef.Compact]
// summarises a run's conversation without a user message.
//
// It is optional because it is not the transport's to give. Compaction needs
// an agent that implements [runtime.Compactor]; a runner built over one that
// does not answers [runtime.ErrCompactionUnsupported], and the HTTP channel
// maps that to 501 rather than pretending the work happened.
const CapCompaction Capability = "compaction"

// supports reports whether the fixture claims a capability, and skips the
// case with a message naming it when it does not.
func supports(t *testing.T, f *Fixture, cap Capability) {
	t.Helper()
	for _, missing := range f.Unsupported {
		if missing == cap {
			t.Skipf("adapter declares it does not support %q", cap)
		}
	}
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
	t.Run("a turn survives the caller going away", func(t *testing.T) {
		f := build(t)
		// The context that STARTS the turn is cancelled mid-turn, the way an
		// HTTP client that hangs up cancels a request context and a webhook
		// handler's context ends when it answers the platform. Neither is a
		// decision to stop the agent: the run is durable, and stopping it is
		// Cancel and nothing else.
		//
		// Before the runner detached the turn context, every transport lost
		// runs this way and the journal kept them at the last checkpoint
		// instead of at an answer.
		callerCtx, hangUp := context.WithCancel(context.Background())
		defer hangUp()

		release, active := f.Agent.HoldOpen()
		done := make(chan *runtime.Run, 1)
		go func() {
			run, err := f.Inbound.From("detach-addr").Send(callerCtx, "long work", channel.SendOptions{})
			if err != nil {
				run = &runtime.Run{Err: err}
			}
			done <- run
		}()

		select {
		case <-active:
		case <-time.After(10 * time.Second):
			t.Fatal("the turn never became active")
		}
		hangUp()

		// The turn is still working: the caller's exit unwound nothing.
		select {
		case run := <-done:
			t.Fatalf("the turn ended when its caller left: %+v", run)
		case <-time.After(200 * time.Millisecond):
		}
		release()

		select {
		case run := <-done:
			if run.Err != nil {
				t.Fatalf("Send: %v", run.Err)
			}
			if run.State != runtime.RunCompleted {
				t.Fatalf("state = %q, want %q", run.State, runtime.RunCompleted)
			}
			// The durable record is the claim: a later process must find
			// the finished run, not one stuck mid-turn.
			state, err := f.Journal.State(context.Background(), run.ID)
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if state != runtime.RunCompleted {
				t.Fatalf("journalled state = %q, want %q", state, runtime.RunCompleted)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the turn never finished after the agent was released")
		}
	})

	t.Run("send records title, origin, and context", func(t *testing.T) {
		f := build(t)
		ctx := context.Background()

		ref := f.Inbound.From("norm-addr")
		run, err := ref.Send(ctx, "what changed?", channel.SendOptions{
			Title:   "PR #42",
			Kind:    "pull_request",
			Context: []string{"event: pull_request.opened"},
		})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		// A second turn with different metadata and no context.
		if _, err := ref.Send(ctx, "and now?", channel.SendOptions{Title: "ignored", Kind: "dm"}); err != nil {
			t.Fatalf("second Send: %v", err)
		}

		sess, err := runtime.Restore(ctx, run.ID, f.Journal)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if sess.Title() != "PR #42" {
			t.Fatalf("title = %q, want the first turn's", sess.Title())
		}
		o := sess.Origin()
		if o.Channel == "" || o.Kind != "pull_request" {
			t.Fatalf("origin = %+v, want the channel's name and the first turn's kind", o)
		}

		recs, err := f.Journal.Replay(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		var contexts, users []string
		for _, rec := range recs {
			switch {
			case rec.Kind == runtime.RecordContext:
				contexts = append(contexts, rec.Text)
			case rec.Kind == runtime.RecordMessage && rec.Role == "user":
				users = append(users, rec.Text)
			}
		}
		if len(contexts) != 1 || contexts[0] != "event: pull_request.opened" {
			t.Fatalf("context records = %q, want the first turn's only", contexts)
		}
		for _, u := range users {
			if u != "what changed?" && u != "and now?" {
				t.Fatalf("user message %q: context leaked into the conversation", u)
			}
		}
	})

	t.Run("reset retires the run and frees the address", func(t *testing.T) {
		f := build(t)
		ctx := context.Background()

		ref := f.Inbound.From("reset-addr")
		first, err := ref.Send(ctx, "hello", channel.SendOptions{})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := ref.Reset(ctx, "start over"); err != nil {
			t.Fatalf("Reset: %v", err)
		}
		if state, _ := f.Journal.State(ctx, first.ID); state != runtime.RunRetired {
			t.Fatalf("old run state = %q, want retired", state)
		}

		// The address is free: the next Send creates a new run.
		second, err := f.Inbound.From("reset-addr").Send(ctx, "again", channel.SendOptions{})
		if err != nil {
			t.Fatalf("Send after reset: %v", err)
		}
		if second.ID == first.ID {
			t.Fatal("the address still resolves to the retired run")
		}
		// A fixed reference stays pinned to the retired run and is refused.
		if _, err := f.Inbound.Attach(first.ID).Send(ctx, "more", channel.SendOptions{}); !errors.Is(err, runtime.ErrRunRetired) {
			t.Fatalf("Send to the retired run = %v, want ErrRunRetired", err)
		}
		// Reset on an address that owns nothing is a no-op, not a create.
		if err := f.Inbound.From("never-bound").Reset(ctx, ""); err != nil {
			t.Fatalf("Reset on an unbound address: %v", err)
		}
		if _, err := f.Inbound.From("never-bound").RunID(ctx); err != nil {
			t.Fatalf("RunID: %v", err)
		}
		if err := f.Inbound.Attach("no-such-run").Reset(ctx, ""); !errors.Is(err, runtime.ErrRunNotFound) {
			t.Fatalf("Reset on an unknown run = %v, want ErrRunNotFound", err)
		}
	})

	t.Run("clear forgets the conversation and keeps the run", func(t *testing.T) {
		f := build(t)
		ctx := context.Background()

		ref := f.Inbound.From("clear-addr")
		run, err := ref.Send(ctx, "remember this", channel.SendOptions{})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := ref.Clear(ctx); err != nil {
			t.Fatalf("Clear: %v", err)
		}
		sess, err := runtime.Restore(ctx, run.ID, f.Journal)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if n := len(sess.GetMessages()); n != 0 {
			t.Fatalf("%d messages after clear, want 0", n)
		}
		again, err := ref.Send(ctx, "fresh start", channel.SendOptions{})
		if err != nil {
			t.Fatalf("Send after clear: %v", err)
		}
		if again.ID != run.ID {
			t.Fatalf("clear changed the run: %s -> %s", run.ID, again.ID)
		}
	})

	t.Run("compact summarises on demand", func(t *testing.T) {
		f := build(t)
		supports(t, f, CapCompaction)
		ctx := context.Background()

		ref := f.Inbound.From("compact-addr")
		run, err := ref.Send(ctx, "a long story", channel.SendOptions{})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := ref.Compact(ctx); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		sess, err := runtime.Restore(ctx, run.ID, f.Journal)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if c := sess.GetLastCompaction(); c == nil || c.Summary != "scripted summary" {
			t.Fatalf("last compaction = %+v, want the scripted summary", c)
		}
	})
}

// refSend is Send with an explicit policy, so the table cases stay flat.
func refSend(ctx context.Context, ref channel.SessionRef, policy channel.TurnPolicy) (*runtime.Run, error) {
	return ref.Send(ctx, "message", channel.SendOptions{TurnPolicy: policy})
}
