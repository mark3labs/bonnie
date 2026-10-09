package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/channel"
	httpchannel "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
)

type testAgent struct{ session *runtime.Session }

func (a *testAgent) PromptResult(_ context.Context, text string) (*kit.TurnResult, error) {
	if _, err := a.session.AppendMessage(kit.NewLLMUserMessage(text)); err != nil {
		return nil, err
	}
	return &kit.TurnResult{Response: "done"}, nil
}
func (*testAgent) InjectSteer(string) {}
func (*testAgent) Close() error       { return nil }
func setup(t *testing.T, opts ...httpchannel.Option) (http.Handler, *runtime.MemoryJournal) {
	t.Helper()
	j := runtime.NewMemoryJournal()
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	runner := runtime.NewRunner(j, func(_ context.Context, s *runtime.Session) (runtime.Agent, error) { return &testAgent{s}, nil })
	return New(runner, j, httpchannel.New(runner, opts...)), j
}
func request(h http.Handler, method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func csrf(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := request(h, "GET", "/web/", nil, nil)
	if w.Code != 200 {
		t.Fatalf("page: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "bonnie_web_csrf" {
			return c
		}
	}
	t.Fatal("missing CSRF cookie")
	return nil
}

// No web route, including assets and streams, bypasses the channel verifier.
func TestAuthentication(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err    error
		status int
	}{{httpchannel.ErrUnauthenticated, 401}, {errors.New("verifier failed"), 500}} {
		h, _ := setup(t, httpchannel.WithAuthenticator(func(*http.Request) (*channel.Principal, error) { return nil, tc.err }))
		for _, path := range []string{"/web", "/web/", "/web/?run=secret", "/web/live", "/web/assets/web.css", "/web/assets/datastar.js"} {
			w := request(h, "GET", path, nil, nil)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "Conversation") {
				t.Fatalf("%s: %d", path, w.Code)
			}
		}
	}
}

// Same-site cookies alone cannot authorize an action. Cross-origin requests
// remain forbidden even when they carry a valid token.
func TestCSRF(t *testing.T) {
	t.Parallel()
	h, j := setup(t)
	c := csrf(t, h)
	for _, tc := range []struct{ token, origin, fetch string }{{"", "", ""}, {"wrong", "", ""}, {c.Value, "https://attacker.example", "cross-site"}} {
		r := httptest.NewRequest("POST", "http://example.com/web/actions/start", strings.NewReader(url.Values{"csrf": {tc.token}, "text": {"hello"}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(c)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.fetch)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("CSRF: %d %s", w.Code, w.Body.String())
		}
	}
	ids, err := j.Runs(t.Context(), "")
	if err != nil || len(ids) != 0 {
		t.Fatalf("unexpected runs %v %v", ids, err)
	}
}

// Actions go through the channel, and snapshots escape all untrusted content.
func TestActionsAndViews(t *testing.T) {
	t.Parallel()
	h, j := setup(t, httpchannel.WithIDGenerator(func() string { return "chat" }))
	c := csrf(t, h)
	text := "<script>alert('x')</script>"
	w := request(h, "POST", "/web/actions/start", url.Values{"csrf": {c.Value}, "text": {text}}, c)
	if w.Code != 200 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "POST", "/web/actions/send", url.Values{"csrf": {c.Value}, "run": {"chat"}, "text": {"second"}}, c)
	if w.Code != 200 {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}

	payload := json.RawMessage(`{"nested":{"html":"<img src=x onerror=alert(1)>"},"values":[1,2]}`)
	if _, err := j.Append(t.Context(), runtime.Record{RunID: "chat", Kind: runtime.RecordContext, Text: text, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := j.Checkpoint(t.Context(), "bonnie.hidden", runtime.RunPending); err != nil {
		t.Fatal(err)
	}
	w = request(h, "GET", "/web/?run=chat", nil, c)
	html := w.Body.String()
	for _, want := range []string{"Message content", "&lt;script&gt;", "cn-button", "send-text"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"<script>alert", "<img src=x", "bonnie.hidden"} {
		if strings.Contains(html, bad) {
			t.Errorf("unsafe or reserved: %s", bad)
		}
	}
	w = request(h, "GET", "/web/?view=trace&run=chat&filter=nested", nil, c)
	if strings.Count(w.Body.String(), `class="record"`) != 1 {
		t.Fatal("filter did not select one record")
	}
	for _, action := range []string{"clear", "reset"} {
		w = request(h, "POST", "/web/actions/"+action, url.Values{"csrf": {c.Value}, "run": {"chat"}}, c)
		if w.Code < 200 || w.Code >= 300 {
			t.Fatalf("%s: %d %s", action, w.Code, w.Body.String())
		}
	}
	w = request(h, "GET", "/web/?view=trace&run=chat", nil, c)
	if !strings.Contains(w.Body.String(), "retired") || !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatal("reset lost readable history")
	}
	if request(h, "GET", "/web/?run=missing", nil, c).Code != 404 {
		t.Fatal("missing run should be 404")
	}
	if request(h, "POST", "/web/actions/unknown", url.Values{"csrf": {c.Value}}, c).Code != 404 {
		t.Fatal("unknown action should be 404")
	}
}

type observedJournal struct {
	runtime.Journal
	reads atomic.Int64
}

func (j *observedJournal) Runs(ctx context.Context, state runtime.RunState) ([]string, error) {
	j.reads.Add(1)
	return j.Journal.Runs(ctx, state)
}

// SSE sends Datastar patches on runtime events. An idle connection does not
// read the journal, and a disconnect releases its subscription.
func TestLive(t *testing.T) {
	t.Parallel()
	memory := runtime.NewMemoryJournal()
	defer func() {
		if err := memory.Close(); err != nil {
			t.Error(err)
		}
	}()
	journal := &observedJournal{Journal: memory}
	runner := runtime.NewRunner(journal, nil)
	h := New(runner, journal, httpchannel.New(runner))
	c := csrf(t, h)
	server := httptest.NewServer(h)
	defer server.Close()
	req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/web/live?view=runs", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(c)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: datastar-patch-elements\n" {
		t.Fatalf("SSE %q %v", line, err)
	}
	if err := journal.Checkpoint(t.Context(), "live-run", runtime.RunPending); err != nil {
		t.Fatal(err)
	}
	runner.Events().Publish(runtime.Event{RunID: "live-run", Type: runtime.EventState, State: runtime.RunPending})
	found := make(chan bool, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				found <- false
				return
			}
			if strings.Contains(line, "live-run") {
				found <- true
				return
			}
		}
	}()
	select {
	case ok := <-found:
		if !ok {
			t.Fatal("stream ended")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no live update")
	}
	beforeIdle := journal.reads.Load()
	time.Sleep(1100 * time.Millisecond)
	if journal.reads.Load() != beforeIdle {
		t.Fatal("idle stream read the journal")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	before := journal.reads.Load()
	time.Sleep(1100 * time.Millisecond)
	if journal.reads.Load() != before {
		t.Fatal("polling continued after disconnect")
	}
}

func TestLocalAssets(t *testing.T) {
	t.Parallel()
	h, _ := setup(t)
	for _, path := range []string{"datastar.js", "web.js", "web.css"} {
		w := request(h, "GET", "/web/assets/"+path, nil, nil)
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("asset %s: %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "https://cdn") {
			t.Fatal("runtime CDN dependency")
		}
	}
	if got := pretty(json.RawMessage("bad <script>")); got != "bad <script>" {
		t.Fatal("invalid payload was lost")
	}
	// The logo is embedded at two sizes: 64px for the favicon and the 32px
	// header mark, 320px for the 160px welcome image. Both at 2x density.
	for _, path := range []string{"logo-64.png", "logo-320.png"} {
		w := request(h, "GET", "/web/assets/"+path, nil, nil)
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("asset %s: %d %s", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
}

// Suspension controls retain the durable turn ID and show explicit decisions.
// The channel, not the form adapter, rejects stale answers.
func TestSuspensionView(t *testing.T) {
	t.Parallel()
	h, j := setup(t)
	if _, err := j.Append(t.Context(), runtime.Record{RunID: "waiting", Kind: runtime.RecordTurn, Text: "turn-current"}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(runtime.SuspendRequest{TurnID: "turn-current", Kind: runtime.SuspendApproval, Prompt: "<script>approve?</script>"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(t.Context(), runtime.Record{RunID: "waiting", Kind: runtime.RecordSuspend, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := j.Checkpoint(t.Context(), "waiting", runtime.RunWaiting); err != nil {
		t.Fatal(err)
	}
	c := csrf(t, h)
	w := request(h, "GET", "/web/?run=waiting", nil, c)
	for _, want := range []string{"Waiting for input", "&lt;script&gt;approve?", "turn-current", `value="approve"`, `value="reject"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
	w = request(h, "POST", "/web/actions/respond", url.Values{"csrf": {c.Value}, "run": {"waiting"}, "turn": {"stale"}, "approval": {"approve"}}, c)
	if w.Code < 400 {
		t.Fatalf("stale approval was accepted: %d", w.Code)
	}
}

// The original browser credentials reach the channel's action verifier.
func TestActionAuthentication(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	h, _ := setup(t, httpchannel.WithAuthenticator(func(r *http.Request) (*channel.Principal, error) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer operator" {
			return nil, httpchannel.ErrUnauthenticated
		}
		return &channel.Principal{ID: "operator"}, nil
	}))
	pageRequest := httptest.NewRequest("GET", "/web/", nil)
	pageRequest.Header.Set("Authorization", "Bearer operator")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, pageRequest)
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no cookie")
	}
	c := cookies[0]
	r := httptest.NewRequest("POST", "/web/actions/start", strings.NewReader(url.Values{"csrf": {c.Value}, "text": {"hello"}}.Encode()))
	r.Header.Set("Authorization", "Bearer operator")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(c)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || calls.Load() < 3 {
		t.Fatalf("action auth: %d, calls %d", w.Code, calls.Load())
	}
}

// Chat is the default. Management and trace require an explicit view, and SSE
// keeps that view and its filter instead of switching back to chat.
func TestViewNavigation(t *testing.T) {
	t.Parallel()
	h, _ := setup(t, httpchannel.WithIDGenerator(func() string { return "chat" }))
	c := csrf(t, h)
	start := request(h, "GET", "/web/", nil, c).Body.String()
	for _, want := range []string{"How can I help?", `id="start-text"`, `/web/actions/start`, `id="new-chat"`} {
		if !strings.Contains(start, want) {
			t.Errorf("entry missing %s", want)
		}
	}
	for _, bad := range []string{`id="start-title"`, `class="runs"`, `class="record"`, "Run controls"} {
		if strings.Contains(start, bad) {
			t.Errorf("entry contains %s", bad)
		}
	}
	w := request(h, "POST", "/web/actions/start", url.Values{"csrf": {c.Value}, "text": {"first message"}}, c)
	if w.Code != 200 {
		t.Fatalf("first message: %d %s", w.Code, w.Body.String())
	}
	for _, name := range []string{"chat", "runs", "trace"} {
		w := request(h, "GET", viewURL(name, "chat"), nil, c)
		if w.Code != 200 {
			t.Fatalf("%s: %d", name, w.Code)
		}
		html := w.Body.String()
		for marker, present := range map[string]bool{`id="send-text"`: name == "chat", `class="runs"`: name == "runs", `class="record"`: name == "trace", "Run controls": name == "runs"} {
			if strings.Contains(html, marker) != present {
				t.Errorf("%s: %s presence", name, marker)
			}
		}
	}
	for _, path := range []string{"/web/?view=unknown", "/web/live?view=unknown"} {
		if got := request(h, "GET", path, nil, c).Code; got != 400 {
			t.Errorf("%s: %d", path, got)
		}
	}
	u, err := url.Parse(liveURL(view{View: "trace", ID: "chat", Filter: "tool & state"}))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("view") != "trace" || u.Query().Get("run") != "chat" || u.Query().Get("filter") != "tool & state" {
		t.Fatalf("live URL: %s", u)
	}
}
