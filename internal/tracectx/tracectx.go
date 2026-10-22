// Package tracectx reads W3C trace context from inbound requests, so a
// channel can make a turn a child of the request that started it.
//
// It uses the W3C propagator directly, not the global one: the global
// propagator is a no-op until an application sets one, and a channel must
// honour traceparent either way.
package tracectx

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var propagator = propagation.TraceContext{}

// Extract returns ctx with the remote span context that h carries. h is an
// http.Header or a nats.Header. It returns ctx unchanged when h has no valid
// traceparent, or when ctx already holds a span.
func Extract(ctx context.Context, h map[string][]string) context.Context {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	return propagator.Extract(ctx, carrierOf(h))
}

// FromContext returns the span context in ctx as the map that
// runtime.Input.Trace holds, or nil when ctx has no valid span context. A
// channel passes the context [Extract] returned, so a deferred turn gets the
// same parent that a direct turn from that request gets.
func FromContext(ctx context.Context) map[string]string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	carrier := propagation.MapCarrier{}
	propagator.Inject(trace.ContextWithSpanContext(context.Background(), sc), carrier)
	return carrier
}

// carrierOf copies the two trace headers under their canonical keys. HTTP
// canonicalises header keys, but a NATS header keeps the case a publisher
// wrote, and propagation.HeaderCarrier looks up canonical keys only.
func carrierOf(h map[string][]string) propagation.HeaderCarrier {
	out := make(http.Header, 2)
	for k, v := range h {
		if strings.EqualFold(k, "traceparent") || strings.EqualFold(k, "tracestate") {
			out[http.CanonicalHeaderKey(k)] = v
		}
	}
	return propagation.HeaderCarrier(out)
}
