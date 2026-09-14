package chat_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
)

// TestDeliveryTextRendersEveryBoundary pins the rule the chat adapters share.
// It lived three times, byte for byte, in slack, discord, and telegram: a
// change to what a person sees at the end of a turn could reach two
// transports out of three and nobody would notice.
func TestDeliveryTextRendersEveryBoundary(t *testing.T) {
	t.Parallel()

	const hint = "(Reply in this chat to answer.)"
	cases := []struct {
		name string
		run  *runtime.Run
		err  error
		want string
	}{
		{
			name: "an answer is the answer",
			run:  &runtime.Run{State: runtime.RunCompleted, Response: "eu-west-1"},
			want: "eu-west-1",
		},
		{
			name: "a parked run asks its question and says how to answer",
			run: &runtime.Run{
				State:   runtime.RunWaiting,
				Suspend: &runtime.SuspendRequest{Prompt: "Which region?"},
			},
			want: "Which region?\n\n" + hint,
		},
		{
			name: "a cancelled run says so rather than falling silent",
			run:  &runtime.Run{State: runtime.RunCancelled},
			want: "(cancelled)",
		},
		{
			name: "a failure says why, in one line",
			err:  errors.New("bonnie: boom\nand a stack of context nobody reads in a chat window"),
			want: "the run failed: bonnie: boom",
		},
		{
			name: "nothing to say stays quiet",
			run:  &runtime.Run{State: runtime.RunCompleted},
			want: "",
		},
		{
			name: "no run and no error stays quiet",
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := chat.DeliveryText(c.run, c.err, hint); got != c.want {
				t.Fatalf("DeliveryText = %q, want %q", got, c.want)
			}
		})
	}
}

// TestDeliveryTextWithoutAHintJustAsks covers an adapter that has no way to
// tell a person how to answer: the question must not grow a dangling blank
// line.
func TestDeliveryTextWithoutAHintJustAsks(t *testing.T) {
	t.Parallel()
	run := &runtime.Run{
		State:   runtime.RunWaiting,
		Suspend: &runtime.SuspendRequest{Prompt: "Which region?"},
	}
	if got := chat.DeliveryText(run, nil, ""); got != "Which region?" {
		t.Fatalf("DeliveryText = %q", got)
	}
}

// TestFirstLineCapsAnError keeps a structural error out of a chat window.
func TestFirstLineCapsAnError(t *testing.T) {
	t.Parallel()
	if got := chat.FirstLine("one\ntwo"); got != "one" {
		t.Fatalf("FirstLine = %q, want the first line only", got)
	}
	long := strings.Repeat("x", 500)
	if got := chat.FirstLine(long); len(got) != 200 {
		t.Fatalf("FirstLine kept %d bytes, want 200", len(got))
	}
}

// Two channels over one journal cannot resolve the same bare key to one
// run: the core applies its own name as the prefix, so an adapter that
// writes "C1/dm" on Slack and one that writes "C1/dm" on a custom channel
// get two runs. The prefix is the core's, not the adapter's — an adapter
// that spelled "slack/" itself would be prefixed again and still not
// collide.
func TestCorePrefixesAddressesWithItsName(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	r := runtime.NewRunner(j, agent.Factory())
	ctx := context.Background()

	slack := chat.NewCore(r, "slack", "")
	other := chat.NewCore(r, "other", "")

	a, err := slack.From("C1/dm").Send(ctx, "hi", channel.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := other.From("C1/dm").Send(ctx, "hi", channel.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two channels resolved one bare key to the same run")
	}
	if got := slack.Address("C1/dm"); got != "slack/C1/dm" {
		t.Fatalf("Address = %q, want slack/C1/dm", got)
	}
	// The raw map holds the prefixed keys, and each channel sees only its
	// own binding through Lookup.
	if id, ok, _ := slack.Lookup(ctx, "C1/dm"); !ok || id != a.ID {
		t.Fatalf("slack Lookup = %q %v, want %q", id, ok, a.ID)
	}
	if _, ok, _ := slack.Lookup(ctx, "other/C1/dm"); ok {
		t.Fatal("a channel can reach another channel's binding by spelling its prefix")
	}
	// An adapter that borrows another channel's prefix is prefixed again.
	if _, ok, _ := other.Lookup(ctx, "slack/C1/dm"); ok {
		t.Fatal("spelling another channel's prefix reached its binding")
	}
}

// TitleFrom derives a listing title from the first message.
func TestTitleFrom(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"deploy the app":              "deploy the app",
		"  first line\nsecond line  ": "first line",
		strings.Repeat("word ", 20):   strings.TrimSpace(strings.Repeat("word ", 12)) + "…",
		strings.Repeat("x", 100):      strings.Repeat("x", 60) + "…",
		"":                            "",
	}
	for in, want := range cases {
		if got := chat.TitleFrom(in); got != want {
			t.Errorf("TitleFrom(%q) = %q, want %q", in, got, want)
		}
	}
}
