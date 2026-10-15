package chat_test

import (
	"context"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
)

func TestTrackedDispatchRecoversSavedCompletion(t *testing.T) {
	t.Parallel()
	for _, state := range []runtime.RunState{runtime.RunRunning, runtime.RunInterrupted, runtime.RunCancelled} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			j := runtime.NewMemoryJournal()
			for _, rec := range []runtime.Record{
				{Kind: runtime.RecordCompletion, Payload: []byte(`{"phase":"accepted","candidate":{"response":"saved response","continuations_used":1}}`)},
				{Kind: runtime.RecordState, State: state},
			} {
				rec.RunID = "dispatch-run"
				if _, err := j.Append(ctx, rec); err != nil {
					t.Fatal(err)
				}
			}
			agent := channeltest.NewScriptAgent()
			core := chat.NewCore(runtime.NewRunner(j, agent.Factory()), "chat", "")
			run, err := core.RunDispatch(ctx, channel.DispatchReceipt{RunID: "dispatch-run"}, "original", channel.SendOptions{Trigger: &runtime.Trigger{DispatchID: "dispatch"}})
			if err != nil || run == nil {
				t.Fatalf("RunDispatch = %+v, %v", run, err)
			}
			if state == runtime.RunCancelled {
				if run.State != runtime.RunCancelled {
					t.Fatalf("run = %+v", run)
				}
			} else if run.State != runtime.RunCompleted || run.Response != "saved response" {
				t.Fatalf("run = %+v", run)
			}
			if agent.Calls() != 0 {
				t.Fatalf("model calls = %d", agent.Calls())
			}
		})
	}
}
