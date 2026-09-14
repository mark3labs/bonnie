package chat_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel/chat"
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
