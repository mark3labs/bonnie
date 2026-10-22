package langfuse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/mark3labs/bonnie/observe"
)

// A Langfuse that never answers must not hold a score goroutine for ever
// when the supplied client has no timeout: Close would then wait until its
// own context ended, and the goroutine and connection would leak.
func TestScoreRequestHasItsOwnDeadline(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	s, err := NewScorer(Config{Host: srv.URL, PublicKey: "pk", SecretKey: "sk", HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	s.timeout = 50 * time.Millisecond
	yes := true
	waited := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}})
	s.Answer(context.Background(), observe.Answer{Waited: waited, Approved: &yes})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v; the score request outlived its deadline", err)
	}
}
