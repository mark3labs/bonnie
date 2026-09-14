package github_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/channel/github"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// testKey is generated once per test binary. The fake API only checks that
// a Bearer token arrives, so the key never leaves the process and its size
// is a speed choice, not a security one.
var testKey = sync.OnceValue(func() string {
	k, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
})

// fakeAPI is a stand-in for GitHub: it mints installation tokens, accepts
// comment posts, records what was posted, and — when asked — serves the
// pull-request detail and file endpoints the PR context reads.
type fakeAPI struct {
	mu        sync.Mutex
	tokens    int
	posted    []string
	reactions []string
	server    *httptest.Server
}

func newFakeAPI(t *testing.T, withPulls bool, title, patch string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "ghs_test-token"})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/reactions") {
			f.reactions = append(f.reactions, r.URL.Path)
		} else {
			f.posted = append(f.posted, r.URL.Path+"|"+body.Body)
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})
	if withPulls {
		mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{n}", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"title": title, "changed_files": 2,
				"base": map[string]string{"ref": "main"},
				"head": map[string]string{"ref": "fix"},
			})
		})
		mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{n}/files", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"filename": "main.go", "patch": patch},
				{"filename": "go.sum", "patch": "+generated"},
			})
		})
	}
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) posts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posted...)
}

// harness wires a channel over a fake GitHub and serves its webhook.
type harness struct {
	ch      *github.Channel
	agent   *channeltest.ScriptAgent
	fake    *fakeAPI
	server  *httptest.Server
	journal runtime.Journal
}

func newHarness(t *testing.T, cfg github.Config) *harness {
	t.Helper()
	fake := newFakeAPI(t, false, "", "")
	return newHarnessOn(t, cfg, fake)
}

func newHarnessOn(t *testing.T, cfg github.Config, fake *fakeAPI) *harness {
	t.Helper()
	if cfg.BotName == "" {
		cfg.BotName = "my-agent"
	}
	if cfg.AppID == "" {
		cfg.AppID = "1"
	}
	cfg.PrivateKey = testKey()
	cfg.WebhookSecret = "s3cret"
	cfg.APIURL = fake.server.URL

	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	runner := runtime.NewRunner(j, agent.Factory())
	ch, err := github.New(runner, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	for _, rt := range ch.Routes() {
		mux.HandleFunc(rt.Method+" "+rt.Path, func(w http.ResponseWriter, req *http.Request) {
			rt.Handler(w, req, ch, nil)
		})
	}
	h := &harness{ch: ch, agent: agent, fake: fake, server: httptest.NewServer(mux), journal: j}
	t.Cleanup(h.server.Close)
	return h
}

// deliver posts a webhook the way GitHub would: signed, with a delivery ID.
func (h *harness) deliver(t *testing.T, deliveryID, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+github.DefaultPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	sum := hmac.New(sha256.New, []byte("s3cret"))
	sum.Write([]byte(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(sum.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// refRunID resolves a From reference without tripping vet's shadow rules.
func refRunID(ch *github.Channel, address string) (string, bool, error) {
	ref, ok := ch.From(address).(interface {
		RunID(ctx context.Context) (string, error)
	})
	if !ok {
		return "", false, fmt.Errorf("the reference does not expose RunID")
	}
	id, err := ref.RunID(context.Background())
	return id, err == nil, err
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// The fixture the conformance suite drives. The suite never posts to
// GitHub, so the fake API URL is a dead address and the controls — which
// are channel-agnostic — still run.
func TestGitHubChannelJoinsConformance(t *testing.T) {
	t.Parallel()
	channeltest.RunConformance(t, func(t *testing.T) *channeltest.Fixture {
		j := runtime.NewMemoryJournal()
		agent := channeltest.NewScriptAgent()
		runner := runtime.NewRunner(j, agent.Factory())
		ch, err := github.New(runner, github.Config{
			BotName:       "bot",
			AppID:         "1",
			PrivateKey:    testKey(),
			WebhookSecret: "s3cret",
			APIURL:        "http://127.0.0.1:1",
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return &channeltest.Fixture{Inbound: ch, Agent: agent, Journal: j}
	})
}

func TestMentionStartsAConversationAndContinuesIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{})

	h.agent.Say(&kit.TurnResult{Response: "the answer"})
	resp := h.deliver(t, "d1", issueComment("U1", "@my-agent what is a durable run?"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack = %d, want 200 immediately", resp.StatusCode)
	}
	waitFor(t, func() bool { return len(h.fake.posts()) > 0 })
	if got := h.fake.posts()[0]; !strings.Contains(got, "issues/42/comments|the answer") {
		t.Fatalf("delivered %q", got)
	}

	// A second comment continues the same run, with no mention.
	h.agent.Say(&kit.TurnResult{Response: "the follow-up"})
	h.deliver(t, "d2", issueComment("U1", "and in one sentence?"))
	waitFor(t, func() bool { return len(h.fake.posts()) > 1 })
	if calls := h.agent.Calls(); calls != 2 {
		t.Fatalf("agent ran %d turns, want 2", calls)
	}

	// The run is titled by the issue, its origin is this channel and kind
	// issue, the sender reached the model as context, and the comment
	// carries no mention.
	id, bound, _ := refRunID(h.ch, github.AddressIssue("octo", "repo", 42))
	if !bound {
		t.Fatal("the issue is not bound")
	}
	sess, err := runtime.Restore(context.Background(), id, h.journal)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Title() != "Issue: fix the login flow" {
		t.Fatalf("title = %q", sess.Title())
	}
	if o := sess.Origin(); o.Channel != "github" || o.Kind != chat.KindIssue {
		t.Fatalf("origin = %+v", o)
	}
	recs, _ := h.journal.Replay(context.Background(), id)
	var contexts []string
	for _, rec := range recs {
		if rec.Kind == runtime.RecordMessage {
			// The invocation token and the sender must not have entered
			// the conversation as anyone's words.
			for _, noise := range []string{"@my-agent", "U1"} {
				if strings.Contains(rec.Text, noise) {
					t.Fatalf("message record %q carries platform noise", rec.Text)
				}
			}
		}
		if rec.Kind == runtime.RecordContext {
			contexts = append(contexts, rec.Text)
		}
	}
	if len(contexts) == 0 || !strings.Contains(contexts[0], "U1") || !strings.Contains(contexts[0], "mentioned") {
		t.Fatalf("context = %q, want the event and the sender", contexts)
	}
}

// A reply in one review thread and a PR-timeline comment on the same PR
// are two conversations.
func TestReviewThreadAndTimelineAreSeparateRuns(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{})
	h.agent.Say(&kit.TurnResult{Response: "review answer"})
	h.agent.Say(&kit.TurnResult{Response: "timeline answer"})

	h.deliver(t, "r1", reviewComment("U1", "@my-agent why is this a mutex?"))
	waitFor(t, func() bool { return len(h.fake.posts()) > 0 })
	h.deliver(t, "r2", prTimelineComment("U1", "@my-agent summarise this"))
	waitFor(t, func() bool { return len(h.fake.posts()) > 1 })

	reviewID, ok, _ := refRunID(h.ch, github.AddressReview("octo", "repo", 7, 9001))
	if !ok {
		t.Fatal("the review thread is not bound")
	}
	timelineID, ok, _ := refRunID(h.ch, github.AddressPullRequest("octo", "repo", 7))
	if !ok {
		t.Fatal("the PR timeline is not bound")
	}
	if reviewID == timelineID {
		t.Fatal("a review thread and its PR share one run")
	}
	posts := h.fake.posts()
	if !strings.Contains(posts[0], "/comments/9001/replies|review answer") {
		t.Fatalf("review reply delivered as %q", posts[0])
	}
	if !strings.Contains(posts[1], "/issues/7/comments|timeline answer") {
		t.Fatalf("timeline reply delivered as %q", posts[1])
	}
}

// The PR diff reaches the model as context, not as user text.
func TestPullRequestContextCarriesTheDiff(t *testing.T) {
	t.Parallel()
	fake := newFakeAPI(t, true, "fix things", "+1 -1 main.go")
	h := newHarnessOn(t, github.Config{}, fake)
	h.agent.Say(&kit.TurnResult{Response: "looks good"})

	h.deliver(t, "p1", prTimelineComment("U1", "@my-agent review this"))
	waitFor(t, func() bool { return h.agent.Calls() == 1 })

	id, _, _ := refRunID(h.ch, github.AddressPullRequest("octo", "repo", 7))
	recs, _ := h.journal.Replay(context.Background(), id)
	var contexts []string
	for _, rec := range recs {
		if rec.Kind == runtime.RecordMessage && strings.Contains(rec.Text, "@my-agent") {
			t.Fatalf("the invocation token entered the conversation: %q", rec.Text)
		}
		if rec.Kind == runtime.RecordContext {
			contexts = append(contexts, rec.Text)
		}
	}
	if len(contexts) != 1 || !strings.Contains(contexts[0], "fix things") || !strings.Contains(contexts[0], "main.go") {
		t.Fatalf("PR context = %q", contexts)
	}
	// No message record may carry the diff: it was context for one turn,
	// not something anyone said.
	for _, rec := range recs {
		if rec.Kind == runtime.RecordMessage && (strings.Contains(rec.Text, "fix things") || strings.Contains(rec.Text, "main.go")) {
			t.Fatalf("the diff entered the conversation as %q", rec.Text)
		}
	}
	if strings.Contains(contexts[0], "+generated") {
		t.Fatalf("go.sum's patch reached the context: %q", contexts[0])
	}
}

// The installation token never reaches the journal.
func TestTokenNeverReachesTheJournal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{})
	h.agent.Say(&kit.TurnResult{Response: "done"})
	h.deliver(t, "t1", issueComment("U1", "@my-agent hi"))
	waitFor(t, func() bool { return len(h.fake.posts()) > 0 })

	id, _, _ := refRunID(h.ch, github.AddressIssue("octo", "repo", 42))
	recs, err := h.journal.Replay(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		for _, part := range []string{rec.Text, string(rec.Payload)} {
			if strings.Contains(part, "ghs_") || strings.Contains(part, "test-token") {
				t.Fatalf("the token reached the journal: %q", part)
			}
		}
	}
}

// An unsigned delivery is refused; a replayed delivery is dropped.
func TestSignatureAndReplay(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{})
	body := issueComment("U1", "@my-agent hi")

	req, _ := http.NewRequest(http.MethodPost, h.server.URL+github.DefaultPath, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned delivery = %d, want 401", resp.StatusCode)
	}

	h.agent.Say(&kit.TurnResult{Response: "once"})
	h.deliver(t, "same-id", body)
	waitFor(t, func() bool { return h.agent.Calls() == 1 })
	h.deliver(t, "same-id", body)
	time.Sleep(200 * time.Millisecond)
	if calls := h.agent.Calls(); calls != 1 {
		t.Fatalf("agent ran %d turns after a replayed delivery, want 1", calls)
	}
}

// A bot's own comment never triggers the agent, and a comment with no
// mention in an unbound thread is ignored.
func TestBotsAndStrangersAreIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{})
	h.deliver(t, "b1", issueComment("my-agent[bot]", "the answer was 42"))
	h.deliver(t, "b2", issueComment("U1", "a stranger comments"))
	time.Sleep(200 * time.Millisecond)
	if calls := h.agent.Calls(); calls != 0 {
		t.Fatalf("agent ran %d turns, want 0", calls)
	}
}

// The opt-in hooks dispatch on the events they accept and ignore the rest.
func TestOptInHooks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, github.Config{
		OnIssue: func(ctx github.IssueCtx) *chat.Turn {
			if ctx.Action != "opened" {
				return nil
			}
			return &chat.Turn{Text: "Summarise issue #" + fmt.Sprint(ctx.Number) + ": " + ctx.Title}
		},
		OnCheckSuite: func(ctx github.CheckSuiteCtx) *chat.Turn {
			if ctx.Conclusion != "failure" || len(ctx.PullRequests) == 0 {
				return nil
			}
			return &chat.Turn{
				Text:    fmt.Sprintf("Triage the failed check suite %d on %s.", ctx.SuiteID, ctx.HeadSHA),
				Context: []string{fmt.Sprintf("check suite failed for PR #%d", ctx.PullRequests[0])},
			}
		},
	})
	h.agent.Say(&kit.TurnResult{Response: "summarised"})
	h.agent.Say(&kit.TurnResult{Response: "triaged"})

	h.deliver(t, "h1", issueOpened("the thing is broken"))
	h.deliver(t, "h2", checkSuiteFailed(9901, "abc123", 5))
	// A reply is posted only after the run reaches its boundary, so two
	// posts mean both turns are fully journalled.
	waitFor(t, func() bool { return len(h.fake.posts()) >= 2 })

	// The suite anchored to PR #5: a different run from the issue's, with
	// both its own turn.
	id, ok, _ := refRunID(h.ch, github.AddressPullRequest("octo", "repo", 5))
	if !ok {
		t.Fatal("the pull request is not bound")
	}
	sess, err := runtime.Restore(context.Background(), id, h.journal)
	if err != nil {
		t.Fatal(err)
	}
	// The scripted agent journalls one assistant message per turn.
	if msgs := sess.GetMessages(); len(msgs) == 0 {
		t.Fatalf("no messages on the suite run: %v", sess.GetMessages())
	}
}

// ---------------------------------------------------------------------------
// Payloads
// ---------------------------------------------------------------------------

func issueComment(user, body string) string {
	return fmt.Sprintf(`{
		"action":"created",
		"sender":{"login":%q,"type":"User"},
		"repository":{"owner":{"login":"octo"},"name":"repo"},
		"installation":{"id":77},
		"issue":{"number":42,"title":"fix the login flow"},
		"comment":{"id":9000,"body":%q}
	}`, user, body)
}

func prTimelineComment(user, body string) string {
	return fmt.Sprintf(`{
		"action":"created",
		"sender":{"login":%q,"type":"User"},
		"repository":{"owner":{"login":"octo"},"name":"repo"},
		"installation":{"id":77},
		"issue":{"number":7,"title":"fix the login flow","pull_request":{}},
		"comment":{"id":9002,"body":%q}
	}`, user, body)
}

func reviewComment(user, body string) string {
	return fmt.Sprintf(`{
		"action":"created",
		"sender":{"login":%q,"type":"User"},
		"repository":{"owner":{"login":"octo"},"name":"repo"},
		"installation":{"id":77},
		"pull_request":{"number":7,"title":"fix the login flow"},
		"comment":{"id":9003,"in_reply_to_id":9001,"pull_request_review_id":8001,"body":%q}
	}`, user, body)
}

func issueOpened(title string) string {
	return fmt.Sprintf(`{
		"action":"opened",
		"sender":{"login":"U1","type":"User"},
		"repository":{"owner":{"login":"octo"},"name":"repo"},
		"installation":{"id":77},
		"issue":{"number":5,"title":%q}
	}`, title)
}

func checkSuiteFailed(suiteID int64, sha string, prNumber int) string {
	return fmt.Sprintf(`{
		"action":"completed",
		"sender":{"login":"U1","type":"User"},
		"repository":{"owner":{"login":"octo"},"name":"repo"},
		"installation":{"id":77},
		"check_suite":{"id":%d,"conclusion":"failure","head_sha":%q,
			"app":{"login":"github-actions"},
			"pull_requests":[{"number":%d}]}
	}`, suiteID, sha, prNumber)
}
