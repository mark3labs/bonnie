package runtime_test

import (
	"context"
	"testing"

	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
)

// Schedule provenance survives restore, but must not become the identity or
// trigger of a later human turn on the same conversation.
func TestCurrentTriggerClearsOnHumanTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	runner := runtime.NewRunner(journal, agent.Factory())
	if _, err := runner.Start(ctx, "trigger-run", runtime.Input{Text: "scheduled", Trigger: &runtime.Trigger{ScheduleName: "daily", DispatchID: "dispatch"}}); err != nil {
		t.Fatal(err)
	}
	session, err := runtime.Restore(ctx, "trigger-run", journal)
	if err != nil {
		t.Fatal(err)
	}
	trigger := session.CurrentTrigger()
	if trigger == nil || trigger.ScheduleName != "daily" {
		t.Fatalf("trigger=%+v", trigger)
	}
	trigger.ScheduleName = "changed"
	if session.CurrentTrigger().ScheduleName != "daily" {
		t.Fatal("caller changed session provenance")
	}
	if _, err := runner.Start(ctx, "trigger-run", runtime.Input{Text: "human"}); err != nil {
		t.Fatal(err)
	}
	session, err = runtime.Restore(ctx, "trigger-run", journal)
	if err != nil {
		t.Fatal(err)
	}
	if session.CurrentTrigger() != nil {
		t.Fatal("human turn inherited schedule trigger")
	}
}
