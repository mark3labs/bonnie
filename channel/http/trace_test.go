package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
	"go.opentelemetry.io/otel/trace"

	"github.com/mark3labs/bonnie/runtime"
)

const (
	testTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	testTraceID     = "0af7651916cd43dd8448eb211c80319c"
)

// parentObserver records the span context each turn started in.
type parentObserver struct {
	mu      sync.Mutex
	parents []trace.SpanContext
}

func (o *parentObserver) StartTurn(ctx context.Context, _ runtime.TurnStart) (context.Context, runtime.TurnObservation) {
	o.mu.Lock()
	o.parents = append(o.parents, trace.SpanContextFromContext(ctx))
	o.mu.Unlock()
	return ctx, nil
}

func (o *parentObserver) first() trace.SpanContext {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.parents) == 0 {
		return trace.SpanContext{}
	}
	return o.parents[0]
}

func postTraced(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", testTraceparent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// A turn a request starts runs in the request's trace context, so a tracer
// makes the turn a child of the caller's span.
func TestTraceparentReachesTheTurn(t *testing.T) {
	t.Parallel()
	obs := &parentObserver{}
	agent := &stubAgent{turns: []*kit.TurnResult{{Response: "hello"}}}
	r := runtime.NewRunner(runtime.NewMemoryJournal(), func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
		agent.mu.Lock()
		agent.session = s
		agent.mu.Unlock()
		return agent, nil
	}, runtime.WithObserver(obs))
	srv := httptest.NewServer(New(r).Handler())
	t.Cleanup(srv.Close)

	if resp := postTraced(t, srv.URL+"/bonnie/v1/runs", StartRequest{Text: "hi"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	sc := obs.first()
	if !sc.IsRemote() || sc.TraceID().String() != testTraceID {
		t.Fatalf("turn started in %v, want the caller's remote span", sc)
	}
}

// A submission runs later, maybe in another process, so its trace context
// must be journalled with it rather than kept in a request context.
func TestTraceparentIsJournalledWithASubmission(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, &stubAgent{turns: []*kit.TurnResult{{Response: "hello"}, {Response: "again"}}})
	resp, run := s.post(t, "/bonnie/v1/runs", StartRequest{Text: "hi"})
	if resp.StatusCode != http.StatusOK || run.RunID == "" {
		t.Fatalf("start status = %d, run %+v", resp.StatusCode, run)
	}
	resp = postTraced(t, s.URL+"/bonnie/v1/runs/"+run.RunID+"/submissions", SubmissionRequest{RequestID: "r1", Text: "later"})
	if resp.StatusCode/100 != 2 {
		t.Fatalf("submit status = %d", resp.StatusCode)
	}
	items, err := s.runner.Submissions(context.Background(), run.RunID)
	if err != nil || len(items) != 1 {
		t.Fatalf("submissions = %+v, %v", items, err)
	}
	if got := items[0].Input.Trace["traceparent"]; got != testTraceparent {
		t.Fatalf("journalled traceparent = %q, want %q", got, testTraceparent)
	}
}

// A span the host put around the request wins over the caller's header, for
// a deferred turn as for a direct one. Before, a submission journalled the
// header and its turn got a different parent than a direct turn would.
func TestHostSpanWinsForASubmission(t *testing.T) {
	t.Parallel()
	agent := &stubAgent{turns: []*kit.TurnResult{{Response: "hello"}}}
	r := runtime.NewRunner(runtime.NewMemoryJournal(), func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
		agent.mu.Lock()
		agent.session = s
		agent.mu.Unlock()
		return agent, nil
	})
	host := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{7}, SpanID: trace.SpanID{7}, TraceFlags: trace.FlagsSampled,
	})
	h := New(r).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.ServeHTTP(w, req.WithContext(trace.ContextWithSpanContext(req.Context(), host)))
	}))
	t.Cleanup(srv.Close)

	start := postTraced(t, srv.URL+"/bonnie/v1/runs", StartRequest{Text: "hi"})
	var run RunResponse
	if err := json.NewDecoder(start.Body).Decode(&run); err != nil || run.RunID == "" {
		t.Fatalf("start = %+v, %v", run, err)
	}
	if resp := postTraced(t, srv.URL+"/bonnie/v1/runs/"+run.RunID+"/submissions", SubmissionRequest{RequestID: "r1", Text: "later"}); resp.StatusCode/100 != 2 {
		t.Fatalf("submit status = %d", resp.StatusCode)
	}
	items, err := r.Submissions(context.Background(), run.RunID)
	if err != nil || len(items) != 1 {
		t.Fatalf("submissions = %+v, %v", items, err)
	}
	got := items[0].Input.Trace["traceparent"]
	if got == testTraceparent || !strings.Contains(got, host.TraceID().String()) {
		t.Fatalf("journalled traceparent = %q, want the host span %s", got, host.TraceID())
	}
}
