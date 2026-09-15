package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
)

// refusingAPI answers the way Slack reports an application failure: HTTP
// 200 with "ok": false. An unknown channel, a revoked token, and a rate
// limit all arrive like this, so a channel that reads the status line only
// calls every one of them a success.
func refusingAPI(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

// A hand-off whose root message Slack refuses is a hand-off that failed.
// It must not bind an address, must not run a turn, and must say so: the
// caller is a program, and it has no other way to learn.
func TestReceiveReportsARefusedRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"the API refuses", `{"ok":false,"error":"channel_not_found"}`},
		{"the answer carries no timestamp", `{"ok":true}`},
		{"the answer is not JSON", `<html>gateway error</html>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			j := runtime.NewMemoryJournal()
			agent := channeltest.NewScriptAgent()
			ch := New(runtime.NewRunner(j, agent.Factory()), Config{
				BotToken:      "xoxb-test",
				SigningSecret: "s3cret",
				APIURL:        refusingAPI(t, c.body).URL,
			})

			err := ch.Receive(context.Background(), "C404", "the digest request", channel.SendOptions{})
			if err == nil {
				t.Fatal("the hand-off reported success though Slack never opened a thread")
			}
			if !strings.Contains(err.Error(), "thread could not be opened") {
				t.Fatalf("error = %v, want one that says the thread did not open", err)
			}
			if calls := agent.Calls(); calls != 0 {
				t.Fatalf("the agent ran %d turns for a hand-off that failed", calls)
			}
			if ids, _ := j.Runs(context.Background(), ""); len(ids) != 0 {
				t.Fatalf("a failed hand-off left runs on the journal: %v", ids)
			}
		})
	}
}

// The happy path: the root opens a thread, the address binds to it, and
// the run carries this channel's origin. The reply threading itself is
// covered end to end in the framework's handoff test.
func TestReceiveBindsTheThreadItOpened(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	fake := newFakeAPI(t)
	ch := New(runtime.NewRunner(j, agent.Factory()), Config{
		BotToken:      "xoxb-test",
		SigningSecret: "s3cret",
		APIURL:        fake.server.URL,
	})

	if err := ch.Receive(context.Background(), "C123", "the digest request", channel.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	id, ok, err := ch.core.Lookup(context.Background(), "C123/"+fakeRootTS)
	if err != nil || !ok {
		t.Fatalf("the thread the hand-off opened is not bound: %v %v", ok, err)
	}
	waitFor(t, func() bool { return len(fake.messages()) > 1 })
	sess, err := runtime.Restore(context.Background(), id, j)
	if err != nil {
		t.Fatal(err)
	}
	if o := sess.Origin(); o.Channel != "slack" || o.Kind != chat.KindThread {
		t.Fatalf("origin = %+v, want slack/thread", o)
	}
}

// A target that is not a channel ID is refused before anything is posted.
func TestReceiveRefusesATargetThatIsNotAChannelID(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	ch := New(runtime.NewRunner(j, agent.Factory()), Config{BotToken: "xoxb-test", SigningSecret: "s3cret"})
	for _, target := range []any{42, "", nil} {
		if err := ch.Receive(context.Background(), target, "hi", channel.SendOptions{}); err == nil {
			t.Fatalf("Receive accepted %#v as a target", target)
		}
	}
}
