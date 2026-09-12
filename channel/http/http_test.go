package http

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// stubAgent stands in for a live model. The HTTP layer is under test here, not
// the agent loop.
type stubAgent struct {
	mu      sync.Mutex
	turns   []*kit.TurnResult
	call    int
	steers  []string
	block   chan struct{}
	started chan struct{}
	session *runtime.Session
}

func (a *stubAgent) PromptResult(ctx context.Context, msg string) (*kit.TurnResult, error) {
	a.mu.Lock()
	if a.started != nil {
		close(a.started)
		a.started = nil
	}
	block := a.block
	a.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// The real agent journals its turns — the assistant message is what a
	// response event anchors to, and a stream test that skips it would test
	// a shape production never produces. Unscripted calls answer "done",
	// and that answer is journalled like any other.
	res := &kit.TurnResult{Response: "done"}
	if a.call < len(a.turns) {
		res = a.turns[a.call]
	}
	if a.session != nil {
		if _, err := a.session.AppendMessage(kit.NewLLMUserMessage(msg)); err != nil {
			return nil, err
		}
		if res.Response != "" {
			assistant := kit.LLMMessage{
				Role:    kit.LLMMessageRole("assistant"),
				Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: res.Response}},
			}
			if _, err := a.session.AppendMessage(assistant); err != nil {
				return nil, err
			}
		}
	}
	a.call++
	return res, nil
}

func (a *stubAgent) InjectSteer(msg string) {
	a.mu.Lock()
	a.steers = append(a.steers, msg)
	a.mu.Unlock()
}

func (a *stubAgent) steered() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.steers...)
}

func (a *stubAgent) Close() error { return nil }

// testServer wires a runner, a channel, and an httptest server together.
type testServer struct {
	*httptest.Server
	runner  *runtime.Runner
	channel *Channel
	agent   *stubAgent
}

func newTestServer(t *testing.T, agent *stubAgent, opts ...Option) *testServer {
	t.Helper()
	return newTestServerOn(t, runtime.NewMemoryJournal(), agent, opts...)
}

func newTestServerOn(t *testing.T, j runtime.Journal, agent *stubAgent, opts ...Option) *testServer {
	t.Helper()
	r := runtime.NewRunner(j, func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
		agent.mu.Lock()
		agent.session = s
		agent.mu.Unlock()
		return agent, nil
	})
	c := New(r, opts...)
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return &testServer{Server: srv, runner: r, channel: c, agent: agent}
}

func (s *testServer) post(t *testing.T, path string, body any) (*http.Response, RunResponse) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	} else {
		buf.WriteString("{}")
	}
	resp, err := http.Post(s.URL+path, "application/json", &buf)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out RunResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (s *testServer) get(t *testing.T, path string) (*http.Response, RunResponse) {
	t.Helper()
	resp, err := http.Get(s.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out RunResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// TestStartRunAndSend covers the happy path of the two write routes.
func TestStartRunAndSend(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{
		{Response: "hello"},
		{Response: "again"},
	}})

	resp, run := s.post(t, "/runs", StartRequest{Text: "hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if run.State != runtime.RunCompleted || run.Response != "hello" {
		t.Fatalf("run = %+v", run)
	}
	if run.RunID == "" {
		t.Fatal("no run ID in the response")
	}

	resp, second := s.post(t, "/runs/"+run.RunID, SendRequest{Text: "more"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if second.RunID != run.RunID {
		t.Fatalf("second turn ran as %q, want %q", second.RunID, run.RunID)
	}
	if second.Response != "again" {
		t.Fatalf("response = %q", second.Response)
	}
}

// TestAttachNeverCreates is the sharp edge of the From/Attach distinction. A
// typo in a run ID must be a 404, not a new conversation.
func TestAttachNeverCreates(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{})

	resp, _ := s.post(t, "/runs/does-not-exist", SendRequest{Text: "hi"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	runs, err := s.runner.Journal().Runs(context.Background(), "")
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	for _, id := range runs {
		if id == "does-not-exist" {
			t.Fatal("Attach created a run")
		}
	}
}

// TestFromResolvesAddress is the other half: a known address keeps its run, a
// new one gets a new run.
func TestFromResolvesAddress(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{
		{Response: "one"}, {Response: "two"}, {Response: "three"},
	}})

	_, first := s.post(t, "/runs", StartRequest{Address: "slack:C1/T1", Text: "hi"})
	_, again := s.post(t, "/runs", StartRequest{Address: "slack:C1/T1", Text: "still here?"})
	if first.RunID != again.RunID {
		t.Fatalf("address resolved to %q then %q", first.RunID, again.RunID)
	}

	_, other := s.post(t, "/runs", StartRequest{Address: "slack:C1/T2", Text: "hi"})
	if other.RunID == first.RunID {
		t.Fatal("two addresses share one run")
	}
}

// TestAddressMapSurvivesJournalReopen is why the map lives in the journal. A
// restart that loses it would orphan every thread the agent was in.
func TestAddressMapSurvivesJournalReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	j, err := runtime.OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("OpenFileJournal: %v", err)
	}
	first := newTestServerOn(t, j, &stubAgent{turns: []*kit.TurnResult{{Response: "one"}}})
	_, run := first.post(t, "/runs", StartRequest{Address: "slack:C1/T1", Text: "hi"})
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A second process: new journal, new runner, new channel, same directory.
	next, err := runtime.OpenFileJournal(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = next.Close() })
	second := newTestServerOn(t, next, &stubAgent{turns: []*kit.TurnResult{{Response: "two"}}})

	_, resumed := second.post(t, "/runs", StartRequest{Address: "slack:C1/T1", Text: "still here?"})
	if resumed.RunID != run.RunID {
		t.Fatalf("after restart the address resolved to %q, want %q", resumed.RunID, run.RunID)
	}
}

// TestSuspendAndRespond is the human-in-the-loop claim over HTTP.
func TestSuspendAndRespond(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{
		{
			Response:     "Which region?",
			HaltedByTool: "ask_human",
			FinalValue: runtime.SuspendRequest{
				Kind: runtime.SuspendQuestion, Prompt: "Which region?",
			},
		},
		{Response: "Deployed to eu-west-1."},
	}})

	_, run := s.post(t, "/runs", StartRequest{Text: "deploy"})
	if run.State != runtime.RunWaiting {
		t.Fatalf("state = %q, want waiting", run.State)
	}
	if run.Suspend == nil || run.Suspend.Prompt != "Which region?" {
		t.Fatalf("suspend = %+v", run.Suspend)
	}

	// A run that is waiting reports its question to anyone who asks.
	_, snapshot := s.get(t, "/runs/"+run.RunID)
	if snapshot.State != runtime.RunWaiting || snapshot.Suspend == nil {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	resp, done := s.post(t, "/runs/"+run.RunID+"/respond", RespondRequest{
		Responses: []runtime.InputResponse{{Text: "eu-west-1"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if done.State != runtime.RunCompleted || done.Response != "Deployed to eu-west-1." {
		t.Fatalf("run = %+v", done)
	}
}

func TestRespondToRunningRunConflicts(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "ok"}}})

	_, run := s.post(t, "/runs", StartRequest{Text: "hi"})
	resp, _ := s.post(t, "/runs/"+run.RunID+"/respond", RespondRequest{
		Responses: []runtime.InputResponse{{Text: "nobody asked"}},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

// TestCancelRoute stops a turn that is still running.
func TestCancelRoute(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{})
	agent := &stubAgent{block: release, started: started}
	defer close(release)

	s := newTestServer(t, agent, WithIDGenerator(func() string { return "cancel-me" }))

	done := make(chan RunResponse, 1)
	go func() {
		_, run := s.post(t, "/runs", StartRequest{Text: "long job"})
		done <- run
	}()

	<-started
	resp, _ := s.post(t, "/runs/cancel-me/cancel", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	select {
	case run := <-done:
		if run.State != runtime.RunCancelled {
			t.Fatalf("state = %q, want cancelled", run.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop the turn")
	}
}

func TestCancelIdleRunConflicts(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "ok"}}})

	_, run := s.post(t, "/runs", StartRequest{Text: "hi"})
	resp, _ := s.post(t, "/runs/"+run.RunID+"/cancel", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

// TestStreamEmitsNDJSON checks the wire contract: one JSON object per line,
// flushed as it happens.
func TestStreamEmitsNDJSON(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{})
	agent := &stubAgent{block: release, started: started, turns: []*kit.TurnResult{{Response: "hello"}}}
	s := newTestServer(t, agent, WithIDGenerator(func() string { return "stream-me" }))

	turnDone := make(chan struct{})
	go func() {
		defer close(turnDone)
		s.post(t, "/runs", StartRequest{Text: "hi"}) //nolint:errcheck // asserted through the stream
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/runs/stream-me/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("content type = %q", got)
	}

	close(release)
	<-turnDone

	// Read until the run completes, which proves the stream flushes rather
	// than buffering to the end of the request.
	scanner := bufio.NewScanner(resp.Body)
	var seen []runtime.Event
	var complete bool
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev runtime.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("line is not one JSON object: %q", line)
		}
		seen = append(seen, ev)
		if ev.Type == runtime.EventState && ev.State == runtime.RunCompleted {
			complete = true
			break
		}
	}
	// The loop ends either on the completion event or because the stream
	// stopped. A stream that stopped early looks exactly like one that ended
	// cleanly, so ask why before trusting what was read.
	if !complete {
		if err := scanner.Err(); err != nil {
			t.Fatalf("stream failed after %d events: %v", len(seen), err)
		}
		t.Fatalf("stream closed after %d events without reaching the completed state", len(seen))
	}
	// Seqs are journal anchors, not dense counters: a turn journals its
	// messages between the state records, so the response's anchor sits
	// between the running and completed states. What the reconnect
	// contract needs is that they rise and never repeat.
	for i, ev := range seen {
		if i > 0 && ev.Seq <= seen[i-1].Seq {
			t.Fatalf("event %d has seq %d, not above %d", i, ev.Seq, seen[i-1].Seq)
		}
		if ev.RunID != "stream-me" {
			t.Fatalf("event %d belongs to %q", ev.Seq, ev.RunID)
		}
	}
}

// TestStreamResumesFromCursor is the reconnect contract: no gap, no duplicate.
// The seqs are journal anchors, so they are not dense — the response sits
// between the running and completed state records.
func TestStreamResumesFromCursor(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "hello"}}},
		WithIDGenerator(func() string { return "cursor-me" }))

	s.post(t, "/runs", StartRequest{Text: "hi"}) //nolint:errcheck // state asserted below

	first := readStream(t, s, "/runs/cursor-me/stream?cursor=0", 2)
	if first[0].Seq != 1 || first[1].Seq != 3 {
		t.Fatalf("first read = %v, want [1 3] — the response anchors to the assistant message record", seqs(first))
	}

	// A client that dropped after event 3 comes back with its cursor: the
	// closing state of the turn is the next durable event.
	second := readStream(t, s, "/runs/cursor-me/stream?cursor=3", 1)
	if second[0].Seq != 4 || second[0].State != runtime.RunCompleted {
		t.Fatalf("reconnect delivered %v, want the completed state at seq 4 — a gap or a duplicate", seqs(second))
	}
}

func TestStreamRejectsBadCursor(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "hello"}}},
		WithIDGenerator(func() string { return "bad-cursor" }))
	s.post(t, "/runs", StartRequest{Text: "hi"}) //nolint:errcheck // status asserted below

	resp, err := http.Get(s.URL + "/runs/bad-cursor/stream?cursor=abc")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestStreamUnknownRunIs404(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{})

	resp, err := http.Get(s.URL + "/runs/nope/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestSteerPolicyReachesActiveTurn proves a mid-turn message joins the turn
// instead of queueing behind it.
func TestSteerPolicyReachesActiveTurn(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{})
	agent := &stubAgent{block: release, started: started, turns: []*kit.TurnResult{{Response: "ok"}}}
	s := newTestServer(t, agent, WithIDGenerator(func() string { return "steer-me" }))

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.post(t, "/runs", StartRequest{Text: "long job"}) //nolint:errcheck // steering asserted below
	}()
	<-started

	resp, run := s.post(t, "/runs/steer-me", SendRequest{
		Text: "actually, eu-west-1", TurnPolicy: channel.PolicySteer,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if run.State != runtime.RunRunning {
		t.Fatalf("state = %q, want running — the steer should join the turn", run.State)
	}

	close(release)
	<-done

	if steers := agent.steered(); len(steers) != 1 || steers[0] != "actually, eu-west-1" {
		t.Fatalf("steers = %v", steers)
	}
}

// TestQueuePolicySerialises proves the other policy: a queued message waits
// for the active turn and then runs as its own turn.
func TestQueuePolicySerialises(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{})
	agent := &stubAgent{
		block: release, started: started,
		turns: []*kit.TurnResult{{Response: "first"}, {Response: "second"}},
	}
	s := newTestServer(t, agent, WithIDGenerator(func() string { return "queue-me" }))

	firstDone := make(chan RunResponse, 1)
	go func() {
		_, run := s.post(t, "/runs", StartRequest{Text: "first"})
		firstDone <- run
	}()
	<-started

	secondDone := make(chan RunResponse, 1)
	go func() {
		_, run := s.post(t, "/runs/queue-me", SendRequest{
			Text: "second", TurnPolicy: channel.PolicyQueue,
		})
		secondDone <- run
	}()

	// Give the queued request time to reach the lock, then let the first turn
	// go. Both must complete, in order, as separate turns.
	time.Sleep(50 * time.Millisecond)
	close(release)

	first := <-firstDone
	second := <-secondDone
	if first.Response != "first" {
		t.Fatalf("first turn = %+v", first)
	}
	if second.Response != "second" {
		t.Fatalf("queued turn = %+v", second)
	}
	if len(agent.steered()) != 0 {
		t.Fatalf("queue policy steered: %v", agent.steered())
	}
}

// TestConcurrentRunsAreRaceClean runs many independent conversations through
// one server, which is how a channel is actually used.
func TestConcurrentRunsAreRaceClean(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{})

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			address := fmt.Sprintf("slack:C1/T%d", i)
			resp, run := s.post(t, "/runs", StartRequest{Address: address, Text: "hi"})
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d", resp.StatusCode)
				return
			}
			if run.State != runtime.RunCompleted {
				t.Errorf("run %s state = %q", address, run.State)
			}
		})
	}
	wg.Wait()
}

// TestPrincipalIsCarried covers the auth half of the MVP: the identity is
// recorded, not verified.
func TestPrincipalIsCarried(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "ok"}}})

	_, run := s.post(t, "/runs", StartRequest{
		Text: "hi",
		Auth: &channel.Principal{Authenticator: "test", Kind: "user", ID: "u-1"},
	})
	if run.State != runtime.RunCompleted {
		t.Fatalf("run = %+v", run)
	}

	recs, err := s.runner.Journal().Replay(context.Background(), addressRun)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	var found bool
	for _, rec := range recs {
		if rec.ExtType == principalExtType && strings.Contains(string(rec.Payload), `"u-1"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("the principal was not recorded")
	}
}

// TestRebindStartsAFreshConversation covers re-keying: the same address, a new
// run. eve calls this the same thing, and the old run must stay reachable.
func TestRebindStartsAFreshConversation(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{
		{Response: "one"}, {Response: "two"}, {Response: "three"},
	}})
	ctx := context.Background()

	_, first := s.post(t, "/runs", StartRequest{Address: "slack:C1/T1", Text: "hi"})

	if err := s.channel.Rebind(ctx, "slack:C1/T1", "fresh-run"); err != nil {
		t.Fatalf("Rebind: %v", err)
	}

	_, after := s.post(t, "/runs", StartRequest{Address: "slack:C1/T1", Text: "start over"})
	if after.RunID != "fresh-run" {
		t.Fatalf("after Rebind the address resolved to %q, want fresh-run", after.RunID)
	}

	// The old run keeps its history and is still reachable by ID.
	resp, old := s.get(t, "/runs/"+first.RunID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — Rebind destroyed the old run", resp.StatusCode)
	}
	if old.RunID != first.RunID {
		t.Fatalf("old run = %+v", old)
	}
}

func TestMalformedBodyIs400(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{})

	resp, err := http.Post(s.URL+"/runs", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// readStream reads exactly n events from a stream and then disconnects.
func readStream(t *testing.T, s *testServer, path string, n int) []runtime.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out []runtime.Event
	scanner := bufio.NewScanner(resp.Body)
	for len(out) < n && scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev runtime.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		out = append(out, ev)
	}
	if len(out) < n {
		// Short read. Say whether the stream broke or simply ended, because a
		// read that stopped on an error is a different fault from one that ran
		// out of events.
		if err := scanner.Err(); err != nil {
			t.Fatalf("read %d events, want %d: stream failed: %v", len(out), n, err)
		}
		t.Fatalf("read %d events, want %d: stream closed early", len(out), n)
	}
	return out
}

func seqs(events []runtime.Event) []int {
	out := make([]int, len(events))
	for i, ev := range events {
		out[i] = ev.Seq
	}
	return out
}

// TestUnknownTurnPolicyIs400 pins the refusal at the wire. The conformance
// suite holds every adapter to the same contract; this is the HTTP shape of
// it — a misspelled turn_policy is a bad request, not a silently queued one.
func TestUnknownTurnPolicyIs400(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t, &stubAgent{})

	resp, _ := ts.post(t, "/runs", StartRequest{
		Address:    "addr-policy",
		Text:       "hello",
		TurnPolicy: channel.TurnPolicy("steer "),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: an unknown policy must be refused, not guessed", resp.StatusCode)
	}
}

// TestStreamCatchUpPastTheBacklog is the §4.8 regression, at the wire. The
// run produces more events than the bus keeps, and a client reconnects from
// the very start: the catch-up must serve the whole history from the journal
// — no gap, no duplicate, in order — instead of a stream that starts in the
// middle.
func TestStreamCatchUpPastTheBacklog(t *testing.T) {
	t.Parallel()

	j := runtime.NewMemoryJournal()
	turns := make([]*kit.TurnResult, 0, 6)
	for i := range 6 {
		turns = append(turns, &kit.TurnResult{Response: fmt.Sprintf("turn %d", i+1)})
	}
	agent := &stubAgent{turns: turns}
	r := runtime.NewRunner(j, func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
		agent.mu.Lock()
		agent.session = s
		agent.mu.Unlock()
		return agent, nil
	}, runtime.WithEventBuffer(3))
	ch := New(r, WithIDGenerator(func() string { return "catch-up" }))
	srv := httptest.NewServer(ch.Handler())
	t.Cleanup(srv.Close)
	ts := &testServer{Server: srv, runner: r, channel: ch, agent: agent}

	for i := range 6 {
		if _, err := r.Start(context.Background(), "catch-up", runtime.Input{
			Text: fmt.Sprintf("turn %d", i+1),
		}); err != nil {
			t.Fatalf("Start %d: %v", i+1, err)
		}
	}

	events := readStream(t, ts, "/runs/catch-up/stream?cursor=0", 18)
	if len(events) != 18 {
		t.Fatalf("read %d events, want the full 18-event history (6 turns × running, response, completed)", len(events))
	}
	for i, ev := range events {
		if i > 0 && ev.Seq <= events[i-1].Seq {
			t.Fatalf("event %d has seq %d, not above %d", i, ev.Seq, events[i-1].Seq)
		}
	}
	if events[16].Type != runtime.EventResponse || events[16].Text != "turn 6" {
		t.Fatalf("second-to-last event = %+v, want the turn 6 response", events[16])
	}
	if last := events[17]; last.State != runtime.RunCompleted {
		t.Fatalf("last event = %+v, want the closing completed state", last)
	}
}
