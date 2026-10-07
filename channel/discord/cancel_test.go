package discord

import (
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// The registered cancel command reaches the shared dispatcher without a model
// prompt. The normal ask command remains available.
func TestNativeCancelText(t *testing.T) {
	t.Parallel()
	c := &Channel{cfg: Config{Command: "ask", CancelCommand: "stop"}}
	if got := c.inputText(&discordData{Name: "stop"}); got != "/cancel" {
		t.Fatalf("control: %q", got)
	}
	if got := c.inputText(&discordData{Name: "ask"}); got != "" {
		t.Fatalf("ask: %q", got)
	}
}

// A signed native control stops execution without a second model call.
func TestNativeCancelStopsTurn(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)
	release, active := h.agent.HoldOpen()
	defer release()
	done := make(chan *runtime.Run, 1)
	go func() {
		run, err := h.ch.From("C77").Send(t.Context(), "work", channel.SendOptions{})
		if err != nil {
			t.Error(err)
		}
		done <- run
	}()
	<-active
	h.post(t, `{"id":"cancel-1","type":2,"channel_id":"C77","data":{"name":"cancel"},"member":{"user":{"id":"U7","username":"ada"}}}`)
	select {
	case run := <-done:
		if run == nil || run.State != runtime.RunCancelled {
			t.Fatalf("boundary: %+v", run)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop turn")
	}
}
