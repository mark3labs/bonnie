package wire

import (
	"encoding/json"
	"testing"
)

func TestRunStateIsTerminal(t *testing.T) {
	t.Parallel()
	for _, state := range []RunState{RunPending, RunRunning, RunWaiting, RunInterrupted, "unknown"} {
		if state.IsTerminal() {
			t.Errorf("%q must not be terminal", state)
		}
	}
	for _, state := range []RunState{RunCompleted, RunFailed, RunCancelled, RunRetired} {
		if !state.IsTerminal() {
			t.Errorf("%q must be terminal", state)
		}
	}
}

// Approval answers must retain all three states on the wire.
func TestInputResponseJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		response InputResponse
		want     string
	}{
		{InputResponse{Text: "note"}, `{"text":"note"}`},
		{Approve("note"), `{"text":"note","approved":true}`},
		{Reject("note"), `{"text":"note","approved":false}`},
	} {
		raw, err := json.Marshal(tc.response)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != tc.want {
			t.Errorf("got %s, want %s", raw, tc.want)
		}
	}
}

func TestEventJSON(t *testing.T) {
	t.Parallel()
	const raw = `{"run_id":"run-1","turn_id":"turn-1","seq":4,"type":"run_suspend","time":"2026-10-21T00:00:00Z","text":"approve?","state":"waiting","data":{"kind":"approval"}}`
	var event Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatal(err)
	}
	if event.State != RunWaiting || event.Type != EventSuspend {
		t.Fatalf("unexpected event: %+v", event)
	}
	got, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != raw {
		t.Fatalf("got %s, want %s", got, raw)
	}
}
