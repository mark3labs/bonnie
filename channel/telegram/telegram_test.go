package telegram

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// The Inbound contract is platform-independent: the adapter joins the
// conformance suite like any other transport.
//
// The webhook secret is here because [New] requires one: an adapter that
// cannot verify its callers must not exist, even in a test that never
// posts to the webhook. The bot token is absent for the opposite reason —
// delivery IS optional, and the suite drives Inbound directly.
func TestConformance(t *testing.T) {
	t.Parallel()
	channeltest.RunConformance(t, func(t *testing.T) *channeltest.Fixture {
		t.Helper()
		j := runtime.NewMemoryJournal()
		agent := channeltest.NewScriptAgent()
		runner := runtime.NewRunner(j, agent.Factory())
		return &channeltest.Fixture{
			Inbound: mustNew(t, runner, Config{Secret: "conformance"}),
			Agent:   agent,
			Journal: j,
		}
	})
}

// mustNew builds a channel or fails the test.
func mustNew(t *testing.T, r *runtime.Runner, cfg Config) *Channel {
	t.Helper()
	ch, err := New(r, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ch
}

// Telegram's only proof that a delivery is Telegram's is the secret token
// header. Without a secret to compare it against, the adapter would mint a
// [channel.Principal] from the `from` field of whatever JSON arrived.
func TestNewRefusesAnEmptySecret(t *testing.T) {
	t.Parallel()
	runner := runtime.NewRunner(runtime.NewMemoryJournal(), channeltest.NewScriptAgent().Factory())

	ch, err := New(runner, Config{Token: "t0ken"})
	if !errors.Is(err, channel.ErrUnverifiedWebhook) {
		t.Fatalf("New with no secret = %v, want channel.ErrUnverifiedWebhook", err)
	}
	if ch != nil {
		t.Fatal("New returned a channel beside the refusal")
	}
	if !strings.Contains(err.Error(), "TELEGRAM_WEBHOOK_SECRET") {
		t.Fatalf("the refusal does not name the variable to set: %v", err)
	}
}

// Defence in depth: a Channel that somehow holds no secret refuses every
// delivery rather than accepting every delivery. [New] makes the state
// unreachable; this pins which way it fails if that guard is ever lost.
func TestVerifyFailsClosedWithoutASecret(t *testing.T) {
	t.Parallel()
	bare := &Channel{}
	if bare.verify("") || bare.verify("anything") {
		t.Fatal("a channel with no secret accepted an unverified delivery")
	}
}

// fakeAPI is a stand-in for the Telegram Bot API. It records every
// sendMessage and can be made to fail.
type fakeAPI struct {
	mu     sync.Mutex
	server *httptest.Server
	sent   []string
	fail   bool
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !strings.Contains(r.URL.Path, "/sendMessage") {
			http.NotFound(w, r)
			return
		}
		if f.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body struct {
			ChatID          int64  `json:"chat_id"`
			Text            string `json:"text"`
			MessageThreadID int64  `json:"message_thread_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("fake API: undecodable body: %v", err)
		}
		f.sent = append(f.sent, body.Text)
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

// post sends an update to the channel's webhook the way Telegram would.
func (h *harness) post(t *testing.T, secret string, v any) *http.Response {
	t.Helper()
	body, _ := json.Marshal(v)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+DefaultPath, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func webhookPath() string { return DefaultPath }

// harness is the adapter under test with its webhook mounted and its
// platform faked.
type harness struct {
	ch     *Channel
	agent  *channeltest.ScriptAgent
	fake   *fakeAPI
	server *httptest.Server
	// secret is the webhook secret the channel was built with. Every
	// adapter has one now — [New] refuses otherwise — so a test that does
	// not care which value it is posts with this.
	secret string
}

// defaultTestSecret is the webhook secret [adapter] uses when a test does
// not choose one. There is no "no secret" case to test any more: [New]
// refuses that config outright, which TestNewRefusesAnEmptySecret pins.
const defaultTestSecret = "s3cret"

// adapter builds a channel over a scripted agent with the fake API wired
// in and its webhook mounted on a test server.
func adapter(t *testing.T, script []*kit.TurnResult, secret, username string) *harness {
	t.Helper()
	if secret == "" {
		secret = defaultTestSecret
	}
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	for _, s := range script {
		agent.Say(s)
	}
	runner := runtime.NewRunner(j, agent.Factory())
	fake := newFakeAPI(t)
	ch := mustNew(t, runner, Config{
		Token:    "t0ken",
		Secret:   secret,
		Username: username,
		APIURL:   fake.server.URL,
	})
	mux := http.NewServeMux()
	for _, r := range ch.Routes() {
		mux.HandleFunc(r.Method+" "+r.Path, func(w http.ResponseWriter, req *http.Request) {
			r.Handler(w, req, ch, nil)
		})
	}
	h := &harness{ch: ch, agent: agent, fake: fake, server: httptest.NewServer(mux), secret: secret}
	t.Cleanup(h.server.Close)
	return h
}

func TestWebhookRejectsAWrongSecret(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil, "s3cret", "")

	resp, err := http.Post(h.server.URL+webhookPath(), "application/json", strings.NewReader(`{"message":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := h.fake.messages(); len(got) != 0 {
		t.Fatalf("a rejected request drove a delivery: %v", got)
	}
}

// TestWebhookDeliversTheResponse is the happy path: a private message
// starts a run, the run completes, the response lands in the chat.
func TestWebhookDeliversTheResponse(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{{Response: "here is your answer"}}, "", "")

	msg := map[string]any{
		"message": map[string]any{
			"text": "what is a durable run?",
			"from": map[string]any{"id": 7, "username": "ada", "is_bot": false},
			"chat": map[string]any{"id": 42, "type": "private"},
		},
	}
	resp := h.post(t, h.secret, msg)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook = %d, want 200 (the ack must be immediate)", resp.StatusCode)
	}

	// Delivery is async: wait for the fake API to see it.
	waitFor(t, func() bool { return len(h.fake.messages()) > 0 })
	if got := h.fake.messages(); got[0] != "here is your answer" {
		t.Fatalf("delivered %q", got[0])
	}
}

// A group message without a mention or command is not for the agent: it
// must not create a run, or a busy group would drive the model all day.
func TestGroupMessageWithoutMentionIsIgnored(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{{Response: "x"}}, "s3cret", "mybot")

	base := map[string]any{
		"from": map[string]any{"id": 7, "username": "ada", "is_bot": false},
		"chat": map[string]any{"id": 99, "type": "supergroup"},
	}
	for _, text := range []string{
		"just chatting about the weather",
		"/otherbot do a thing",
	} {
		msg := map[string]any{"text": text, "from": base["from"], "chat": base["chat"]}
		if resp := h.post(t, "s3cret", map[string]any{"message": msg}); resp.StatusCode != http.StatusOK {
			t.Fatalf("webhook = %d for %q", resp.StatusCode, text)
		}
	}
	time.Sleep(100 * time.Millisecond) // anything delivered would show by now
	if got := h.fake.messages(); len(got) != 0 {
		t.Fatalf("a message that is not for the bot drove a delivery: %v", got)
	}
}

func TestGroupMentionAndCommandAreAnswered(t *testing.T) {
	t.Parallel()
	say := []*kit.TurnResult{{Response: "group answer"}, {Response: "group answer"}, {Response: "group answer"}}
	h := adapter(t, say, "s3cret", "mybot")

	base := map[string]any{
		"from": map[string]any{"id": 7, "username": "ada", "is_bot": false},
		"chat": map[string]any{"id": 99, "type": "supergroup"},
	}
	for _, tc := range []struct {
		text     string
		entities []map[string]any
	}{
		{"/ask@mybot what time is it", nil},
		{"@mybot what time is it", []map[string]any{{"type": "mention", "offset": 0, "length": 6}}},
		{"/ask what time is it", nil},
	} {
		msg := map[string]any{"text": tc.text, "from": base["from"], "chat": base["chat"]}
		if tc.entities != nil {
			msg["entities"] = tc.entities
		}
		h.post(t, "s3cret", map[string]any{"message": msg})
		waitFor(t, func() bool { return len(h.fake.messages()) > 0 })
		if got := h.fake.messages(); !strings.Contains(got[0], "group answer") {
			t.Fatalf("delivered %q for %q", got[0], tc.text)
		}
		h.fake.mu.Lock()
		h.fake.sent = nil
		h.fake.mu.Unlock()
	}
}

// The HITL shape: a run parks, the prompt is delivered, the next text in
// the chat is the answer — routed to Respond, not to a fresh turn.
func TestReplyToAParkedRunResumesIt(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{
		{Response: "Which region?", FinalValue: runtime.SuspendRequest{Kind: "ask", Prompt: "Which region?"}},
		{Response: "Deployed to eu-west-1."},
	}, "", "")

	h.post(t, h.secret, map[string]any{"message": map[string]any{
		"text": "deploy please",
		"from": map[string]any{"id": 7, "username": "ada", "is_bot": false},
		"chat": map[string]any{"id": 42, "type": "private"},
	}})
	waitFor(t, func() bool {
		for _, m := range h.fake.messages() {
			if strings.Contains(m, "Which region?") {
				return true
			}
		}
		return false
	})

	// The answer: a plain reply in the same chat.
	h.post(t, h.secret, map[string]any{"message": map[string]any{
		"text": "eu-west-1",
		"from": map[string]any{"id": 7, "username": "ada", "is_bot": false},
		"chat": map[string]any{"id": 42, "type": "private"},
	}})
	waitFor(t, func() bool {
		for _, m := range h.fake.messages() {
			if strings.Contains(m, "eu-west-1") {
				return true
			}
		}
		return false
	})

	// Exactly two turns ran: the park, then the resume. A fresh turn for
	// the answer would be the dispatch rule broken.
	if calls := h.agent.Calls(); calls != 2 {
		t.Fatalf("agent ran %d turns, want 2", calls)
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
