package discord

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// The public key is here because [New] requires one: an adapter that cannot
// verify its callers must not exist, even in a test that never posts to the
// webhook. The bot token is absent for the opposite reason — delivery IS
// optional, and the suite drives Inbound directly.
func TestConformance(t *testing.T) {
	t.Parallel()
	channeltest.RunConformance(t, func(t *testing.T) *channeltest.Fixture {
		t.Helper()
		j := runtime.NewMemoryJournal()
		agent := channeltest.NewScriptAgent()
		runner := runtime.NewRunner(j, agent.Factory())
		return &channeltest.Fixture{
			Inbound: mustNew(t, runner, Config{PublicKey: testPublicKey(t)}),
			Agent:   agent,
			Journal: j,
		}
	})
}

// testPublicKey returns a well-formed hex Ed25519 public key, for a test
// that needs [New] to accept a config but never verifies a signature.
func testPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return hex.EncodeToString(pub)
}

// Discord's only proof that an interaction is Discord's is the Ed25519
// signature. Without the application's public key the adapter would mint a
// [channel.Principal] from whatever member or user the body claimed.
func TestNewRefusesAnEmptyPublicKey(t *testing.T) {
	t.Parallel()
	runner := runtime.NewRunner(runtime.NewMemoryJournal(), channeltest.NewScriptAgent().Factory())

	ch, err := New(runner, Config{BotToken: "dt0ken"})
	if !errors.Is(err, channel.ErrUnverifiedWebhook) {
		t.Fatalf("New with no public key = %v, want channel.ErrUnverifiedWebhook", err)
	}
	if ch != nil {
		t.Fatal("New returned a channel beside the refusal")
	}
	if !strings.Contains(err.Error(), "DISCORD_PUBLIC_KEY") {
		t.Fatalf("the refusal does not name the variable to set: %v", err)
	}
}

// Defence in depth: a Channel that somehow holds no key refuses every
// interaction rather than accepting every interaction. [New] makes the
// state unreachable; this pins which way it fails if that guard is lost.
func TestVerifyFailsClosedWithoutAKey(t *testing.T) {
	t.Parallel()
	bare := &Channel{}
	if bare.verify("", "", []byte("{}")) {
		t.Fatal("a channel with no public key accepted an unsigned interaction")
	}
}

func mustNew(t *testing.T, r *runtime.Runner, cfg Config) *Channel {
	t.Helper()
	ch, err := New(r, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ch
}

// fakeAPI is a stand-in for the Discord API. It records every channel post.
type fakeAPI struct {
	mu     sync.Mutex
	server *httptest.Server
	sent   []string
	// raw keeps each post's whole body, so a test can read what the
	// content field alone does not carry — the controls under a message.
	raw []string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("fake API: unreadable body: %v", err)
		}
		var decoded struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("fake API: undecodable body: %v", err)
		}
		f.mu.Lock()
		f.sent = append(f.sent, decoded.Content)
		f.raw = append(f.raw, string(body))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"m"}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// bodies returns each post's whole payload, in order.
func (f *fakeAPI) bodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.raw...)
}

// harness is the adapter under test with its webhook mounted, its platform
// faked, and its key pair in hand.
type harness struct {
	ch      *Channel
	agent   *channeltest.ScriptAgent
	fake    *fakeAPI
	server  *httptest.Server
	privkey ed25519.PrivateKey
}

func adapter(t *testing.T, script []*kit.TurnResult) *harness {
	t.Helper()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	for _, s := range script {
		agent.Say(s)
	}
	runner := runtime.NewRunner(j, agent.Factory())
	fake := newFakeAPI(t)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ch := mustNew(t, runner, Config{
		BotToken:  "dt0ken",
		PublicKey: hex.EncodeToString(pub),
		APIURL:    fake.server.URL,
	})
	mux := http.NewServeMux()
	for _, rt := range ch.Routes() {
		mux.HandleFunc(rt.Method+" "+rt.Path, func(w http.ResponseWriter, req *http.Request) {
			rt.Handler(w, req, ch, nil)
		})
	}
	h := &harness{ch: ch, agent: agent, fake: fake, server: httptest.NewServer(mux), privkey: priv}
	t.Cleanup(h.server.Close)
	return h
}

// post sends an interaction the way Discord would: signed with Ed25519 over
// the timestamp and the body.
func (h *harness) post(t *testing.T, body string) (*http.Response, string) {
	t.Helper()
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := ed25519.Sign(h.privkey, []byte(ts+body))
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+DefaultPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(sig))
	req.Header.Set("X-Signature-Timestamp", ts)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

// ask is the `/ask` interaction.
func ask(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":       2,
		"channel_id": "C77",
		"data": map[string]any{
			"name":    "ask",
			"options": []map[string]any{{"name": "message", "type": 3, "value": text}},
		},
		"member": map[string]any{"user": map[string]any{"id": "U7", "username": "ada"}},
	})
	return string(b)
}

// responseType reads the type out of an interaction response.
func responseType(t *testing.T, raw string) int {
	t.Helper()
	var out struct {
		Type int `json:"type"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("response is not an interaction reply: %v", err)
	}
	return out.Type
}

func TestPingHandshake(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)
	resp, raw := h.post(t, `{"type":1}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if responseType(t, raw) != 1 {
		t.Fatalf("the ping was not echoed: %s", raw)
	}
}

func TestTamperedBodyIsRefused(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)

	// Sign one body, send another: the door must not open.
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := ed25519.Sign(h.privkey, []byte(ts+"other body"))
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+DefaultPath, strings.NewReader(ask("hello")))
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(sig))
	req.Header.Set("X-Signature-Timestamp", ts)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a tampered body got %d, want 401", resp.StatusCode)
	}
}

func TestUnparsableKeyIsRefused(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	runner := runtime.NewRunner(j, runtime.AgentFactory(func(context.Context, *runtime.Session) (runtime.Agent, error) {
		return channeltest.NewScriptAgent(), nil
	}))
	if _, err := New(runner, Config{PublicKey: "not hex"}); err == nil {
		t.Fatal("an unparsable public key must be refused, not run wide open")
	}
}

// The happy path: the command is acknowledged deferred (inside the
// three-second deadline), the turn runs, the reply lands in the channel.
func TestAskIsDeferredThenDelivered(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{{Response: "deployed"}})

	resp, raw := h.post(t, ask("deploy to eu-west-1"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if responseType(t, raw) != typeDeferredWithSource {
		t.Fatalf("ack = %s, want the deferred type", raw)
	}
	waitFor(t, func() bool { return len(h.fake.messages()) > 0 })
	if got := h.fake.messages(); got[0] != "deployed" {
		t.Fatalf("delivered %q", got[0])
	}
}

func TestOtherCommandGetsANote(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)
	body := `{"type":2,"channel_id":"C77","data":{"name":"frobnicate"},"member":{"user":{"id":"U7"}}}`
	resp, raw := h.post(t, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if responseType(t, raw) != typeMessageWithSource {
		t.Fatalf("a stranger command got %s, want a plain reply", raw)
	}
	if got := h.fake.messages(); len(got) != 0 {
		t.Fatalf("a stranger command drove a delivery: %v", got)
	}
}

func TestEmptyMessageGetsAUsageNote(t *testing.T) {
	t.Parallel()
	h := adapter(t, nil)
	resp, raw := h.post(t, ask(""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if responseType(t, raw) != typeMessageWithSource {
		t.Fatalf("an empty message got %s, want a usage reply", raw)
	}
}

// The HITL shape: the run parks, the prompt is delivered with the answer
// gesture, and the next /ask is the answer — routed to the resume.
func TestAskAnswersAParkedRun(t *testing.T) {
	t.Parallel()
	h := adapter(t, []*kit.TurnResult{
		{Response: "Which region?", FinalValue: runtime.SuspendRequest{Kind: "ask", Prompt: "Which region?"}},
		{Response: "Deployed to eu-west-1."},
	})

	h.post(t, ask("deploy please"))
	waitFor(t, func() bool {
		for _, m := range h.fake.messages() {
			if strings.Contains(m, "Which region?") {
				return true
			}
		}
		return false
	})
	if got := h.fake.messages()[0]; !strings.Contains(got, "/ask") {
		t.Fatalf("the prompt does not say how to answer: %q", got)
	}

	h.post(t, ask("eu-west-1"))
	waitFor(t, func() bool {
		for _, m := range h.fake.messages() {
			if strings.Contains(m, "Deployed to eu-west-1.") {
				return true
			}
		}
		return false
	})
	if calls := h.agent.Calls(); calls != 2 {
		t.Fatalf("agent ran %d turns, want 2: the answer must resume, not start over", calls)
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
