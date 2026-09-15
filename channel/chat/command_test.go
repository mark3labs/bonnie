package chat_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// delivery captures what Dispatch hands an adapter, so a test can read what
// a person would have seen.
type delivery struct {
	mu   sync.Mutex
	got  chan struct{}
	run  *runtime.Run
	err  error
	addr string
}

func newDelivery() *delivery { return &delivery{got: make(chan struct{}, 8)} }

func (d *delivery) deliver(address string, run *runtime.Run, err error) {
	d.mu.Lock()
	d.addr, d.run, d.err = address, run, err
	d.mu.Unlock()
	d.got <- struct{}{}
}

// wait blocks for one delivery and returns the text the adapter would post.
func (d *delivery) wait(t *testing.T) (string, *runtime.Run, error) {
	t.Helper()
	select {
	case <-d.got:
	case <-time.After(10 * time.Second):
		t.Fatal("Dispatch never delivered")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return chat.DeliveryText(d.run, d.err, ""), d.run, d.err
}

// commandFixture is a core over a script agent, with its deliveries captured.
func commandFixture(t *testing.T) (*chat.Core, *channeltest.ScriptAgent, *delivery) {
	t.Helper()
	agent := channeltest.NewScriptAgent()
	j := runtime.NewMemoryJournal()
	r := runtime.NewRunner(j, agent.Factory())
	return chat.NewCore(r, "test", ""), agent, newDelivery()
}

// A control acts on the run instead of becoming a turn. The model must never
// see one: "/clear" is an instruction to the framework, and an agent that
// answered it would be answering a question nobody asked.
func TestControlsDoNotReachTheModel(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		chat.ResetCommand,
		chat.CancelCommand,
		chat.ClearCommand,
		chat.CompactCommand,
		chat.HelpCommand,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			core, agent, d := commandFixture(t)
			chat.Dispatch(context.Background(), core, chat.Turn{Address: "c1", Text: name}, d.deliver)

			text, _, err := d.wait(t)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if text == "" {
				t.Fatalf("%s delivered nothing: a control must tell the person what happened", name)
			}
			if calls := agent.Calls(); calls != 0 {
				t.Fatalf("%s ran %d turns: a control must not reach the model", name, calls)
			}
		})
	}
}

// The match is the whole message, so ordinary text that merely mentions a
// control is a turn for the model.
func TestControlsMatchTheWholeMessage(t *testing.T) {
	t.Parallel()
	core, agent, d := commandFixture(t)
	chat.Dispatch(context.Background(), core,
		chat.Turn{Address: "c1", Text: "should I use /new here?"}, d.deliver)

	if _, _, err := d.wait(t); err != nil {
		t.Fatal(err)
	}
	if calls := agent.Calls(); calls != 1 {
		t.Fatalf("the agent ran %d turns, want 1: a message that mentions a control is still a message", calls)
	}
}

// A phone keyboard capitalises the first word of a message. Someone who
// typed "/New" meant "/new".
func TestControlsIgnoreCase(t *testing.T) {
	t.Parallel()
	core, agent, d := commandFixture(t)
	chat.Dispatch(context.Background(), core, chat.Turn{Address: "c1", Text: "  /HELP  "}, d.deliver)

	text, _, err := d.wait(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, chat.CancelCommand) {
		t.Fatalf("help = %q, want the control list", text)
	}
	if calls := agent.Calls(); calls != 0 {
		t.Fatal("a capitalised control reached the model")
	}
}

// /cancel stops a turn that is running. This is the case the control exists
// for, and the one the turn policy would otherwise swallow: the default
// policy steers a mid-turn message into the running turn, so a control that
// went through Route would be read by the model instead of stopping it.
func TestCancelStopsATurnInFlight(t *testing.T) {
	t.Parallel()
	core, agent, d := commandFixture(t)
	ctx := context.Background()

	release, active := agent.HoldOpen()
	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: "long work"}, d.deliver)

	select {
	case <-active:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never became active")
	}

	// The control arrives while the turn is running.
	cancelDone := newDelivery()
	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: chat.CancelCommand}, cancelDone.deliver)
	text, run, err := cancelDone.wait(t)
	if err != nil {
		t.Fatalf("/cancel: %v", err)
	}
	if run.State != runtime.RunCancelled {
		t.Fatalf("state = %q, want cancelled", run.State)
	}
	if text != "(cancelled)" {
		t.Fatalf("delivered %q, want the cancellation acknowledgement", text)
	}
	release()

	// The steer never happened: the control stopped the turn rather than
	// being read by the model.
	if steered := agent.Steered(); len(steered) != 0 {
		t.Fatalf("the control was steered into the turn as %q", steered)
	}

	// The turn's own delivery reports the cancellation too.
	if _, turnRun, _ := d.wait(t); turnRun != nil && turnRun.State != runtime.RunCancelled {
		t.Fatalf("the turn ended %q, want cancelled", turnRun.State)
	}
}

// /cancel with nothing running says so, rather than failing. Someone who
// types it a second after a turn ended asked for a state they already have.
func TestCancelOnAnIdleRunSaysSo(t *testing.T) {
	t.Parallel()
	core, _, d := commandFixture(t)
	ctx := context.Background()

	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: "hello"}, d.deliver)
	if _, _, err := d.wait(t); err != nil {
		t.Fatal(err)
	}

	idle := newDelivery()
	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: chat.CancelCommand}, idle.deliver)
	text, _, err := idle.wait(t)
	if err != nil {
		t.Fatalf("/cancel on an idle run: %v", err)
	}
	if text != "Nothing is running." {
		t.Fatalf("delivered %q, want the idle note", text)
	}
}

// /clear keeps the run and forgets the conversation: same run ID, empty
// context. It is the one control whose whole point is that the run survives.
func TestClearKeepsTheRun(t *testing.T) {
	t.Parallel()
	core, _, d := commandFixture(t)
	ctx := context.Background()

	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: "remember this"}, d.deliver)
	_, first, err := d.wait(t)
	if err != nil {
		t.Fatal(err)
	}

	cleared := newDelivery()
	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: chat.ClearCommand}, cleared.deliver)
	text, run, err := cleared.wait(t)
	if err != nil {
		t.Fatalf("/clear: %v", err)
	}
	if run.ID != first.ID {
		t.Fatalf("/clear moved the conversation from %q to %q", first.ID, run.ID)
	}
	if !strings.Contains(text, "Cleared") {
		t.Fatalf("delivered %q, want the clear note", text)
	}

	sess, err := runtime.Restore(ctx, first.ID, core.Runner().Journal())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(sess.GetMessages()); n != 0 {
		t.Fatalf("%d messages after /clear, want none", n)
	}
}

// /compact summarises on demand, and says plainly when the agent cannot.
// An agent with no compactor is a fact about the deployment, not a failed
// run, and the person should read it as one.
func TestCompactReportsAnAgentThatCannot(t *testing.T) {
	t.Parallel()
	core, _, d := commandFixture(t)
	ctx := context.Background()

	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: "a long story"}, d.deliver)
	if _, _, err := d.wait(t); err != nil {
		t.Fatal(err)
	}

	// The script agent implements runtime.Compactor, so this one succeeds.
	done := newDelivery()
	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: chat.CompactCommand}, done.deliver)
	text, _, err := done.wait(t)
	if err != nil {
		t.Fatalf("/compact: %v", err)
	}
	if !strings.Contains(text, "Compacted") {
		t.Fatalf("delivered %q, want the compaction note", text)
	}
}

// A control on a surface that carries no conversation says so instead of
// creating one. A stray "/cancel" in a channel the agent never joined must
// not mint a run.
func TestControlsOnAnEmptySurfaceCreateNothing(t *testing.T) {
	t.Parallel()
	for _, name := range []string{chat.CancelCommand, chat.ClearCommand, chat.CompactCommand} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			core, _, d := commandFixture(t)
			chat.Dispatch(context.Background(), core,
				chat.Turn{Address: "untouched", Text: name}, d.deliver)

			text, _, err := d.wait(t)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if text != "There is no conversation here yet." {
				t.Fatalf("%s delivered %q, want the empty-surface note", name, text)
			}
			if _, bound, _ := core.Lookup(context.Background(), "untouched"); bound {
				t.Fatalf("%s bound a run on a surface that had none", name)
			}
		})
	}
}

// /new retires the run and frees the address, so the next message starts a
// fresh conversation in the same place. This is the one control that
// predates the others; the test pins that generalising the table kept it.
func TestResetStillRetiresAndFrees(t *testing.T) {
	t.Parallel()
	core, agent, d := commandFixture(t)
	ctx := context.Background()

	agent.Say(&kit.TurnResult{Response: "first"})
	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: "hello"}, d.deliver)
	_, first, err := d.wait(t)
	if err != nil {
		t.Fatal(err)
	}

	reset := newDelivery()
	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: chat.ResetCommand}, reset.deliver)
	text, _, err := reset.wait(t)
	if err != nil {
		t.Fatalf("/new: %v", err)
	}
	if !strings.Contains(text, "Started a new conversation") {
		t.Fatalf("delivered %q, want the reset note", text)
	}
	if state, _ := core.Runner().Journal().State(ctx, first.ID); state != runtime.RunRetired {
		t.Fatalf("old run state = %q, want retired", state)
	}

	next := newDelivery()
	chat.Dispatch(ctx, core, chat.Turn{Address: "c1", Text: "again"}, next.deliver)
	_, second, err := next.wait(t)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("the address still resolves to the retired run")
	}
}

// /help lists every control, so the vocabulary is discoverable from the
// surface it applies to.
func TestHelpListsEveryControl(t *testing.T) {
	t.Parallel()
	core, _, d := commandFixture(t)
	chat.Dispatch(context.Background(), core, chat.Turn{Address: "c1", Text: chat.HelpCommand}, d.deliver)

	text, _, err := d.wait(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		chat.ResetCommand,
		chat.CancelCommand,
		chat.ClearCommand,
		chat.CompactCommand,
		chat.HelpCommand,
	} {
		if !strings.Contains(text, name) {
			t.Fatalf("help omits %q:\n%s", name, text)
		}
	}
}
