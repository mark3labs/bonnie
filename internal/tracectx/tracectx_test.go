package tracectx

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

const (
	parent  = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	traceID = "0af7651916cd43dd8448eb211c80319c"
)

func TestExtractReadsHTTPAndNATSHeaders(t *testing.T) {
	t.Parallel()
	for name, h := range map[string]map[string][]string{
		"http canonical": http.Header{"Traceparent": {parent}},
		// A NATS header keeps the case its publisher wrote.
		"nats lowercase": {"traceparent": {parent}},
	} {
		sc := trace.SpanContextFromContext(Extract(context.Background(), h))
		if !sc.IsRemote() || sc.TraceID().String() != traceID {
			t.Fatalf("%s: span context = %v", name, sc)
		}
		if c := FromContext(Extract(context.Background(), h)); c["traceparent"] != parent {
			t.Fatalf("%s: carrier = %v", name, c)
		}
	}
}

func TestExtractIgnoresMissingAndMalformed(t *testing.T) {
	t.Parallel()
	for _, h := range []map[string][]string{nil, {"Traceparent": {"garbage"}}} {
		if sc := trace.SpanContextFromContext(Extract(context.Background(), h)); sc.IsValid() {
			t.Fatalf("headers %v gave span context %v", h, sc)
		}
		if c := FromContext(Extract(context.Background(), h)); c != nil {
			t.Fatalf("headers %v gave carrier %v", h, c)
		}
	}
}

// A context that already holds a span keeps it: the span a host started
// around a request is a better parent than a header the caller sent.
func TestExtractKeepsAnExistingSpan(t *testing.T) {
	t.Parallel()
	own := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{9}, SpanID: trace.SpanID{9}})
	ctx := trace.ContextWithSpanContext(context.Background(), own)
	ctx = Extract(ctx, http.Header{"Traceparent": {parent}})
	if got := trace.SpanContextFromContext(ctx); got.TraceID() != own.TraceID() {
		t.Fatalf("trace = %s, want the context's own", got.TraceID())
	}
	if c := FromContext(ctx); c["traceparent"] == parent || c["traceparent"] == "" {
		t.Fatalf("carrier = %v, want the context's own span", c)
	}
}
