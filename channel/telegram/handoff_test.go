package telegram

import (
	"context"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

// The origin a hand-off records has to be the one an inbound message on
// the same surface writes, or one conversation is listed two ways. The
// target carries all there is to go on: a topic is a thread, and Telegram
// numbers users positive and groups negative.
func TestReceiveRecordsTheKindTheTargetNames(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"a private chat", "4242", chat.KindDM},
		{"a group", "-100123", chat.KindChannel},
		{"a forum topic", "-100123/77", chat.KindThread},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := adapter(t, nil, "s3cret", "mybot")
			if err := h.ch.Receive(context.Background(), c.target, "the digest", channel.SendOptions{}); err != nil {
				t.Fatal(err)
			}
			id, ok, err := h.ch.core.Lookup(context.Background(), c.target)
			if err != nil || !ok {
				t.Fatalf("the hand-off did not bind %q: %v %v", c.target, ok, err)
			}
			waitFor(t, func() bool {
				sess, err := runtime.Restore(context.Background(), id, h.ch.core.Runner().Journal())
				return err == nil && sess.Origin().Kind != ""
			})
			sess, err := runtime.Restore(context.Background(), id, h.ch.core.Runner().Journal())
			if err != nil {
				t.Fatal(err)
			}
			if o := sess.Origin(); o.Channel != "telegram" || o.Kind != c.want {
				t.Fatalf("origin = %+v, want telegram/%s", o, c.want)
			}
		})
	}
}

// A target that is not a chat ID would deliver to chat 0 in silence: the
// address is parsed again on the way out, with the error dropped, because
// by then no one is left to tell. Refuse it while the caller is still
// listening.
func TestReceiveRefusesATargetThatIsNotAChatID(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil, "s3cret", "mybot")
	for _, target := range []any{"", "general", "@mychannel", "-100123/general", 42, nil} {
		if err := h.ch.Receive(context.Background(), target, "hi", channel.SendOptions{}); err == nil {
			t.Fatalf("Receive accepted %#v as a target", target)
		}
	}
}
