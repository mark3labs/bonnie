package slack

import (
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// renderer builds a channel whose Slack API is faked, for the indicator
// alone: the statuses come from the caller rather than from a turn.
func renderer(t *testing.T, mode ActivityMode) (*Channel, *fakeAPI) {
	t.Helper()
	fake := newFakeAPI(t)
	runner := runtime.NewRunner(runtime.NewMemoryJournal(), channeltest.NewScriptAgent().Factory())
	ch := New(runner, Config{BotToken: "xoxb-test", APIURL: fake.server.URL, Activity: mode})
	return ch, fake
}

// One turn is one message: posted once, edited in place as the work moves,
// and deleted when the reply is ready. A thread must not fill with the
// agent's progress.
func TestActivityEditsOneMessageInPlace(t *testing.T) {
	t.Parallel()
	ch, fake := renderer(t, ActivityDefault)

	ch.renderActivity("C1/1719000000.000100", chat.StatusThinking)
	ch.renderActivity("C1/1719000000.000100", chat.StatusWorking)
	ch.renderActivity("C1/1719000000.000100", "read_file runner.go")
	ch.renderActivity("C1/1719000000.000100", "")

	want := []string{
		"post|_… Thinking…_",
		"update|_… Working…_",
		"update|_… read_file runner.go_",
		"delete",
	}
	got := fake.activity()
	if len(got) != len(want) {
		t.Fatalf("activity = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("activity %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The status already on screen is not written again: a transport pays an
// API call per line, and Slack rate-limits edits.
func TestActivitySkipsARepeatedStatus(t *testing.T) {
	t.Parallel()
	ch, fake := renderer(t, ActivityDefault)

	ch.renderActivity("C1/1.1", chat.StatusWorking)
	ch.renderActivity("C1/1.1", chat.StatusWorking)
	if got := fake.activity(); len(got) != 1 {
		t.Fatalf("activity = %q, want one write", got)
	}
}

// Clearing an indicator that was never posted writes nothing: a turn that
// ended before its first status must not delete a message that does not
// exist.
func TestActivityClearWithoutAPlaceholderIsSilent(t *testing.T) {
	t.Parallel()
	ch, fake := renderer(t, ActivityDefault)

	ch.renderActivity("C1/1.1", "")
	if got := fake.activity(); len(got) != 0 {
		t.Fatalf("activity = %q, want nothing", got)
	}
}

// Slack's own assistant indicator posts no message at all: the status is a
// property of the thread, and the empty status clears it.
func TestActivityStatusModeUsesTheAssistantIndicator(t *testing.T) {
	t.Parallel()
	ch, fake := renderer(t, ActivityStatus)

	ch.renderActivity("C1/1719000000.000100", chat.StatusWorking)
	ch.renderActivity("C1/1719000000.000100", "")

	want := []string{"status|Working…", "status|"}
	got := fake.activity()
	if len(got) != len(want) {
		t.Fatalf("activity = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("activity %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A channel that shows nothing wires no renderer at all, so the option
// cannot be switched on by an event arriving.
func TestActivityOffWritesNothing(t *testing.T) {
	t.Parallel()
	ch, _ := renderer(t, ActivityOff)
	if opt := ch.activityOption(); opt != nil {
		t.Fatal("ActivityOff wired an indicator")
	}
}

// A misspelled mode shows nothing rather than quietly meaning the default:
// a host that asked for one surface and got another has no way to tell.
func TestActivityRefusesAnUnknownMode(t *testing.T) {
	t.Parallel()
	ch, _ := renderer(t, ActivityMode("typing"))
	if opt := ch.activityOption(); opt != nil {
		t.Fatal("an unknown mode was honoured as the default")
	}
}

// A channel with no bot token cannot write to Slack at all. The conformance
// suite drives one, and it must not try.
func TestActivityNeedsABotToken(t *testing.T) {
	t.Parallel()
	runner := runtime.NewRunner(runtime.NewMemoryJournal(), channeltest.NewScriptAgent().Factory())
	ch := New(runner, Config{})
	if opt := ch.activityOption(); opt != nil {
		t.Fatal("a channel with no token wired an indicator")
	}
}

// End to end: a mention shows that the agent received the message, and the
// indicator is gone by the time the answer lands. The thread a person reads
// afterwards holds the conversation and nothing else.
func TestMentionShowsActivityThenClearsIt(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{{Response: "done"}})

	h.post(t, mentionEvent("<@U1> deploy"), "")
	waitFor(t, func() bool { return len(h.fake.messages()) > 0 })

	acts := h.fake.activity()
	if len(acts) == 0 || !strings.HasPrefix(acts[0], "post|") {
		t.Fatalf("activity = %q, want it to open with a placeholder", acts)
	}
	if !strings.Contains(acts[0], chat.StatusThinking) {
		t.Errorf("first status = %q, want %q", acts[0], chat.StatusThinking)
	}
	waitFor(t, func() bool {
		a := h.fake.activity()
		return len(a) > 0 && a[len(a)-1] == "delete"
	})
}
