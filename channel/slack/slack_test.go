package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// The Inbound contract is platform-independent: the adapter joins the
// conformance suite like any other transport.
func TestConformance(t *testing.T) {
	t.Parallel()
	channeltest.RunConformance(t, func(t *testing.T) *channeltest.Fixture {
		t.Helper()
		j := runtime.NewMemoryJournal()
		agent := channeltest.NewScriptAgent()
		runner := runtime.NewRunner(j, agent.Factory())
		return &channeltest.Fixture{
			Inbound: New(runner, Config{}),
			Agent:   agent,
			Journal: j,
		}
	})
}

// fakeAPI is a stand-in for the Slack API. It records every postMessage.
type fakeAPI struct {
	mu     sync.Mutex
	server *httptest.Server
	sent   []string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.postMessage" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Channel  string `json:"channel"`
			Text     string `json:"text"`
			ThreadTS string `json:"thread_ts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("fake API: undecodable body: %v", err)
		}
		f.mu.Lock()
		f.sent = append(f.sent, body.ThreadTS+"|"+body.Text)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// harness is the adapter under test with its webhook mounted and its
// platform faked.
type harness struct {
	ch      *Channel
	agent   *channeltest.ScriptAgent
	fake    *fakeAPI
	server  *httptest.Server
	secret  string
	created time.Time
	journal runtime.Journal
}

// adapter builds the channel with signature verification on, because
// production always runs with it on and the tests must exercise it.
func adapter(t *testing.T, script []*kit.TurnResult) *harness {
	t.Helper()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	for _, s := range script {
		agent.Say(s)
	}
	runner := runtime.NewRunner(j, agent.Factory())
	fake := newFakeAPI(t)
	const secret = "trustno1"
	ch := New(runner, Config{
		BotToken:      "xoxb-test",
		SigningSecret: secret,
		APIURL:        fake.server.URL,
	})
	mux := http.NewServeMux()
	for _, rt := range ch.Routes() {
		mux.HandleFunc(rt.Method+" "+rt.Path, func(w http.ResponseWriter, req *http.Request) {
			rt.Handler(w, req, ch)
		})
	}
	h := &harness{ch: ch, agent: agent, fake: fake, server: httptest.NewServer(mux), secret: secret, created: time.Now(), journal: j}
	t.Cleanup(h.server.Close)
	return h
}

// post sends a body the way Slack would: signed with the v0 scheme.
func (h *harness) post(t *testing.T, body string, ts string) *http.Response {
	t.Helper()
	if ts == "" {
		ts = fmt.Sprintf("%d", time.Now().Unix())
	}
	mac := hmac.New(sha256.New, []byte(h.secret))
	_, _ = fmt.Fprintf(mac, "v0:%s:", ts)
	mac.Write([]byte(body))
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+DefaultPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func mentionEvent(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":     "event_callback",
		"event_id": "Ev" + fmt.Sprint(time.Now().UnixNano()),
		"event": map[string]any{
			"type": "app_mention", "text": text, "ts": "1719000000.000100",
			"channel": "C1", "user": "U7",
		},
	})
	return string(b)
}

func TestURLVerificationHandshake(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)
	resp := h.post(t, `{"type":"url_verification","challenge":"q9x"}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Challenge string `json:"challenge"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Challenge != "q9x" {
		t.Fatalf("challenge = %q, want it echoed", out.Challenge)
	}
}

func TestTamperedBodyIsRefused(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)

	// A signature computed over one body, sent with another: the door must
	// not open.
	sigBody := `{"type":"event_callback"}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(h.secret))
	_, _ = fmt.Fprintf(mac, "v0:%s:", ts)
	mac.Write([]byte(sigBody))
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+DefaultPath, strings.NewReader(`{"type":"url_verification","challenge":"x"}`))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a tampered body got %d, want 401", resp.StatusCode)
	}
}

func TestStaleTimestampIsRefused(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)
	old := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	resp := h.post(t, mentionEvent("hi"), old)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a replayed capture got %d, want 401", resp.StatusCode)
	}
}

func TestMentionStartsAConversationAndThreadsTheReply(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{
		{Response: "the answer"},
		{Response: "the follow-up"},
	})

	resp := h.post(t, mentionEvent("<@U123> what is a durable run?"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack = %d, want 200 immediately", resp.StatusCode)
	}
	waitFor(t, func() bool { return len(h.fake.messages()) > 0 })
	if got := h.fake.messages(); !strings.HasSuffix(got[0], "|the answer") {
		t.Fatalf("delivered %q", got[0])
	}
	if !strings.HasPrefix(h.fake.messages()[0], "1719000000.000100|") {
		t.Fatalf("the reply did not thread to the mention: %q", h.fake.messages()[0])
	}

	// A reply in that thread continues the conversation, without a mention.
	h.post(t, `{
		"type":"event_callback","event_id":"Ev-reply",
		"event":{"type":"message","text":"and in one sentence?","ts":"1719000000.000200",
			"thread_ts":"1719000000.000100","channel":"C1","user":"U7"}}`, "")
	waitFor(t, func() bool { return len(h.fake.messages()) > 1 })
	if got := h.fake.messages(); !strings.HasSuffix(got[1], "|the follow-up") {
		t.Fatalf("delivered %q", got[1])
	}
	if calls := h.agent.Calls(); calls != 2 {
		t.Fatalf("agent ran %d turns, want 2", calls)
	}

	// The event was normalised: the run is titled by what was asked, its
	// origin names this channel and the thread kind, and the sender
	// reached the model as context, not as part of the message.
	runID, ok, _ := h.ch.core.Lookup(context.Background(), "C1/1719000000.000100")
	if !ok {
		t.Fatal("the thread is not bound")
	}
	sess, err := runtime.Restore(context.Background(), runID, h.journal)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Title() != "what is a durable run?" {
		t.Fatalf("title = %q, want the first message", sess.Title())
	}
	if o := sess.Origin(); o != (runtime.Origin{Channel: "slack", Kind: chat.KindThread}) {
		t.Fatalf("origin = %+v", o)
	}
	recs, _ := h.journal.Replay(context.Background(), runID)
	var contexts, users []string
	for _, rec := range recs {
		switch {
		case rec.Kind == runtime.RecordContext:
			contexts = append(contexts, rec.Text)
		case rec.Kind == runtime.RecordMessage && rec.Role == "user":
			users = append(users, rec.Text)
		}
	}
	if len(contexts) != 2 || !strings.Contains(contexts[0], "U7") {
		t.Fatalf("context records = %q, want one per turn naming the sender", contexts)
	}
	for _, u := range users {
		if strings.Contains(u, "U7") || strings.Contains(u, "<@") {
			t.Fatalf("user message %q carries platform noise", u)
		}
	}
}

// A reply in a thread this channel never bound is not the agent's
// business: a busy channel must not drive the model.
func TestThreadReplyToAnUnboundThreadIsDropped(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)
	h.post(t, `{
		"type":"event_callback","event_id":"Ev-other",
		"event":{"type":"message","text":"unsolicited","ts":"1719000000.000300",
			"thread_ts":"1719000000.000300","channel":"C1","user":"U7"}}`, "")
	time.Sleep(100 * time.Millisecond)
	if got := h.fake.messages(); len(got) != 0 {
		t.Fatalf("a thread the channel never started drove a delivery: %v", got)
	}
}

// A direct message is one conversation: every message continues the same
// run, without threading.
func TestDirectMessageIsOneConversation(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{
		{Response: "hello there"},
		{Response: "still here"},
	})
	ev := func(text, ts string) string {
		b, _ := json.Marshal(map[string]any{
			"type": "event_callback", "event_id": "Ev" + ts,
			"event": map[string]any{
				"type": "message", "text": text, "ts": ts, "channel": "D1",
				"channel_type": "im", "user": "U7",
			},
		})
		return string(b)
	}
	h.post(t, ev("hi", "1719000000.000001"), "")
	waitFor(t, func() bool { return len(h.fake.messages()) > 0 })
	h.post(t, ev("hello again", "1719000000.000002"), "")
	waitFor(t, func() bool { return len(h.fake.messages()) > 1 })
	if calls := h.agent.Calls(); calls != 2 {
		t.Fatalf("agent ran %d turns, want 2", calls)
	}
	// A DM reply does not thread: the conversation is the channel itself.
	if strings.Contains(h.fake.messages()[0], "1719000000.000001|") {
		t.Fatalf("the DM reply threaded when it should not: %q", h.fake.messages()[0])
	}
}

// Slack retries on a slow ack; the retried event must not send the same
// message into the turn again.
func TestRedeliveredEventIsDropped(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	agent.Say(&kit.TurnResult{Response: "once"})
	runner := runtime.NewRunner(j, agent.Factory())
	fake := newFakeAPI(t)
	ch := New(runner, Config{BotToken: "xoxb", APIURL: fake.server.URL}) // no secret: signature off
	mux := http.NewServeMux()
	for _, rt := range ch.Routes() {
		mux.HandleFunc(rt.Method+" "+rt.Path, func(w http.ResponseWriter, req *http.Request) {
			rt.Handler(w, req, ch)
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	const body = `{"type":"event_callback","event_id":"Ev-dup","event":{"type":"app_mention","text":"<@U1> hello","ts":"1.1","channel":"C1","user":"U7"}}`
	for range 3 {
		resp, err := http.Post(server.URL+DefaultPath, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	waitFor(t, func() bool { return len(fake.messages()) > 0 })
	time.Sleep(100 * time.Millisecond) // a duplicate would land by now
	if got := fake.messages(); len(got) != 1 || !strings.HasSuffix(got[0], "|once") {
		t.Fatalf("three deliveries of one event gave %q", got)
	}
	if calls := agent.Calls(); calls != 1 {
		t.Fatalf("agent ran %d turns, want 1: the retry must be dropped", calls)
	}
}

// waitFor polls until check is true, with a timeout.
func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the condition")
}
