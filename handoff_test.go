package bonnie

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/slack"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// fakeSlackAPI records what the adapter posts and answers a thread root's
// timestamp, which is what a hand-off needs to bind an address to.
type fakeSlackAPI struct {
	mu     sync.Mutex
	sent   []string
	token  string
	server *httptest.Server
}

func newFakeSlackAPI(t *testing.T) *fakeSlackAPI {
	t.Helper()
	f := &fakeSlackAPI{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.token = r.Header.Get("Authorization")
		var body struct {
			Channel  string `json:"channel"`
			ThreadTS string `json:"thread_ts"`
			Text     string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		ts := "1700000000.000900"
		if body.ThreadTS != "" {
			ts = ""
		}
		f.sent = append(f.sent, body.ThreadTS+"|"+body.Text)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"ts":"` + ts + `"}`))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSlackAPI) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// digestAgent answers every turn the same way.
type digestAgent struct{ session *runtime.Session }

func (a *digestAgent) PromptResult(ctx context.Context, msg string) (*kit.TurnResult, error) {
	if a.session != nil {
		if _, err := a.session.AppendMessage(kit.LLMMessage{
			Role:    kit.LLMMessageRole("assistant"),
			Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "here is the digest"}},
		}); err != nil {
			return nil, err
		}
	}
	return &kit.TurnResult{Response: "here is the digest"}, nil
}
func (a *digestAgent) InjectSteer(string) {}
func (a *digestAgent) Close() error       { return nil }

// The hand-off: a route on one channel starts a run on Slack, the reply
// lands in the thread the Slack adapter opened, and the run is bound so a
// later platform reply continues it.
func TestCrossChannelHandOff(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_SIGNING_SECRET", "s3cret")
	fake := newFakeSlackAPI(t)
	journalDir := t.TempDir()

	// The calling channel: a route that hands work to Slack.
	caller := WithChannel(func(*runtime.Runner) (Channel, error) {
		return handoffChannel{}, nil
	})

	base, _ := serveForTest(t,
		caller,
		WithSlack(slack.Config{APIURL: fake.server.URL}),
		WithJournal(journalDir),
		WithAgentFactory(func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
			return &digestAgent{session: s}, nil
		}),
	)

	// Drive the hand-off.
	resp, err := http.Get(base + "/handoff?channel=C123")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /handoff = %d", resp.StatusCode)
	}

	// The instruction was posted as a thread root, and the reply landed
	// inside that thread.
	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs := fake.messages()
		if len(msgs) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Slack adapter posted %v, want a root and a reply", msgs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	msgs := fake.messages()
	if msgs[0] != "|the digest request" {
		t.Fatalf("the root message = %q, want the instruction", msgs[0])
	}
	if !strings.HasPrefix(msgs[1], "1700000000.000900|") {
		t.Fatalf("the reply = %q, want it threaded under the root", msgs[1])
	}

	// The run exists on the journal and names Slack as its origin.
	journal, err := runtime.OpenSQLiteJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.Close() }()
	ids, err := journal.Runs(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, id := range ids {
		sess, err := runtime.Restore(context.Background(), id, journal)
		if err != nil {
			continue
		}
		o := sess.Origin()
		if o.Channel != "slack" {
			continue
		}
		found = true
		if o.Kind != "thread" {
			t.Fatalf("origin kind = %q, want thread", o.Kind)
		}
	}
	if !found {
		t.Fatal("no run on the journal names Slack as its origin")
	}
}

// handoffChannel is the calling side of the test: one route that asks
// Slack for a digest.
type handoffChannel struct{}

func (handoffChannel) Name() string { return "caller" }

func (handoffChannel) Routes() []channel.Route {
	return []channel.Route{{
		Method: http.MethodGet,
		Path:   "/handoff",
		Handler: func(w http.ResponseWriter, r *http.Request, _ channel.Inbound, out channel.Outbound) {
			to, ok := out.To("slack")
			if !ok {
				http.Error(w, "no slack mounted", http.StatusInternalServerError)
				return
			}
			err := to.Receive(r.Context(), r.URL.Query().Get("channel"), "the digest request", channel.SendOptions{
				Auth: &channel.Principal{Authenticator: "test", Kind: "user", ID: "initiator"},
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		},
	}}
}

func (handoffChannel) From(string) channel.SessionRef   { return nil }
func (handoffChannel) Attach(string) channel.SessionRef { return nil }
