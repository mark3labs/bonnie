package telegram

import (
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// Native controls keep bot targeting intact and do not need a group mention.
func TestNativeCancelTargeting(t *testing.T) {
	t.Parallel()
	c := &Channel{cfg: Config{Username: "bonnie", Command: "ask"}}
	for _, chatType := range []string{"private", "group", "supergroup"} {
		for _, text := range []string{"/cancel", "/cancel@bonnie", "/cancel@other", "/cancel later"} {
			m := &tgMessage{Text: text}
			m.Chat.Type = chatType
			got, ok := c.forUs(m)
			want := text == "/cancel" || text == "/cancel@bonnie"
			if ok != want || (ok && got != "/cancel") {
				t.Fatalf("%s %s: %q %v", chatType, text, got, ok)
			}
		}
	}
}

// A group control targets the bot and stops a turn through the verified webhook.
func TestNativeCancelStopsGroupTurn(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil, "secret", "bonnie")
	release, active := h.agent.HoldOpen()
	defer release()
	done := make(chan *runtime.Run, 1)
	go func() {
		run, err := h.ch.From("42").Send(t.Context(), "work", channel.SendOptions{})
		if err != nil {
			t.Error(err)
		}
		done <- run
	}()
	<-active
	h.post(t, "secret", map[string]any{"update_id": 99, "message": map[string]any{"text": "/cancel@bonnie", "from": map[string]any{"id": 7}, "chat": map[string]any{"id": 42, "type": "group"}}})
	select {
	case run := <-done:
		if run == nil || run.State != runtime.RunCancelled {
			t.Fatalf("boundary: %+v", run)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop turn")
	}
}
