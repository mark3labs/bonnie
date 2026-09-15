package chat_test

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
)

// proactive runs one hand-off and waits for its delivery.
func proactive(t *testing.T, core *chat.Core, turn chat.Turn) (*runtime.Run, error) {
	t.Helper()
	type outcome struct {
		run *runtime.Run
		err error
	}
	done := make(chan outcome, 1)
	if err := core.Proactive(context.Background(), turn, func(_ string, run *runtime.Run, err error) {
		done <- outcome{run, err}
	}); err != nil {
		return nil, err
	}
	select {
	case o := <-done:
		return o.run, o.err
	case <-time.After(5 * time.Second):
		t.Fatal("the proactive turn never delivered")
		return nil, nil
	}
}

// A hand-off to an address that already carries a conversation continues
// it. Only Slack mints a fresh surface per hand-off; Telegram, Discord,
// and GitHub derive one stable address from their target, so a hand-off
// that re-keyed the address would strand the run the person is talking to
// and leave two runs delivering into one surface.
func TestProactiveContinuesAnExistingConversation(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	core := chat.NewCore(runtime.NewRunner(j, agent.Factory()), "telegram", "")
	ctx := context.Background()

	first, err := core.From("-100123").Send(ctx, "hello", channel.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}

	run, err := proactive(t, core, chat.Turn{Address: "-100123", Text: "the digest"})
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != first.ID {
		t.Fatalf("the hand-off ran as %s, want the bound run %s", run.ID, first.ID)
	}
	bound, ok, err := core.Lookup(ctx, "-100123")
	if err != nil || !ok {
		t.Fatalf("Lookup = %v %v", ok, err)
	}
	if bound != first.ID {
		t.Fatalf("the address now points at %s, want the run it already served, %s", bound, first.ID)
	}
}

// A hand-off to an address nobody has spoken on creates and binds the run
// before the turn runs, so a platform reply that arrives mid-turn
// continues it rather than starting a second conversation.
func TestProactiveBindsANewAddressBeforeTheTurn(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	core := chat.NewCore(runtime.NewRunner(j, agent.Factory()), "slack", "")

	run, err := proactive(t, core, chat.Turn{
		Address: "C1/1700.1",
		Text:    "the digest",
		Kind:    chat.KindThread,
	})
	if err != nil {
		t.Fatal(err)
	}
	bound, ok, err := core.Lookup(context.Background(), "C1/1700.1")
	if err != nil || !ok {
		t.Fatalf("the hand-off left its address unbound: %v %v", ok, err)
	}
	if bound != run.ID {
		t.Fatalf("the address points at %s, want the run that ran, %s", bound, run.ID)
	}
}

// The initiating principal is recorded on the destination run, once. The
// dispatch notes it exactly as it notes an inbound sender's; a second note
// from Proactive itself would duplicate a journal record for every
// hand-off.
func TestProactiveRecordsTheInitiatingPrincipalOnce(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	core := chat.NewCore(runtime.NewRunner(j, agent.Factory()), "slack", "")

	run, err := proactive(t, core, chat.Turn{
		Address: "C1/1700.1",
		Text:    "the digest",
		Auth:    &channel.Principal{Authenticator: "test", Kind: "user", ID: "initiator"},
	})
	if err != nil {
		t.Fatal(err)
	}

	recs, err := j.Replay(context.Background(), chat.AddressRun)
	if err != nil {
		t.Fatal(err)
	}
	var notes int
	for _, rec := range recs {
		if rec.ExtType == chat.PrincipalExtType && rec.Text == run.ID {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("the hand-off wrote %d principal records for %s, want 1", notes, run.ID)
	}
}

// A turn with no address has nowhere to go, and says so before it writes
// anything.
func TestProactiveRefusesATurnWithNoAddress(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	core := chat.NewCore(runtime.NewRunner(j, agent.Factory()), "slack", "")

	err := core.Proactive(context.Background(), chat.Turn{Text: "the digest"},
		func(string, *runtime.Run, error) { t.Error("an addressless turn dispatched") })
	if err == nil {
		t.Fatal("Proactive accepted a turn with no address")
	}
}
