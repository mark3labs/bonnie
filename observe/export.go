package observe

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/mark3labs/bonnie/internal/otelspan"
)

// ExportPolicy filters spans for one destination. The [CapturePolicy] is the
// ceiling for every destination; an ExportPolicy can only remove more.
type ExportPolicy struct {
	// Span reports whether to export a span. Nil exports every span. A
	// callback that panics drops the span.
	Span func(sdktrace.ReadOnlySpan) bool
	// RedactInputs removes input content (see [IsInput]) from spans and
	// their events.
	RedactInputs bool
	// RedactOutputs removes output content (see [IsOutput]) from spans and
	// their events.
	RedactOutputs bool
	// Attribute rewrites one attribute of a span or of an event. Return
	// false to drop it. Nil keeps every attribute. A callback that panics
	// drops the span.
	Attribute func(attribute.KeyValue) (attribute.KeyValue, bool)
}

// NewExporter returns an exporter that applies policies in order, then
// hands the spans that remain to next. Use it to send one destination less
// than another:
//
//	honeycomb := observe.NewExporter(otlp, observe.ExportPolicy{
//		RedactInputs: true, RedactOutputs: true,
//	})
func NewExporter(next sdktrace.SpanExporter, policies ...ExportPolicy) sdktrace.SpanExporter {
	return &policyExporter{next: next, policies: policies}
}

type policyExporter struct {
	next     sdktrace.SpanExporter
	policies []ExportPolicy
}

func (e *policyExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, 0, len(spans))
	for _, s := range spans {
		if kept, ok := e.apply(s); ok {
			out = append(out, kept)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return e.next.ExportSpans(ctx, out)
}

func (e *policyExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

// apply runs each policy on s. It reports false when a policy drops s.
func (e *policyExporter) apply(s sdktrace.ReadOnlySpan) (out sdktrace.ReadOnlySpan, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = nil, false
		}
	}()
	for _, p := range e.policies {
		if p.Span != nil && !p.Span(s) {
			return nil, false
		}
		if !p.RedactInputs && !p.RedactOutputs && p.Attribute == nil {
			continue
		}
		events := make([]sdktrace.Event, 0, len(s.Events()))
		for _, ev := range s.Events() {
			ev.Attributes = p.filter(ev.Attributes)
			events = append(events, ev)
		}
		s = otelspan.Rewrite(s, p.filter(s.Attributes()), events)
	}
	return s, true
}

func (p ExportPolicy) filter(attrs []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		if (p.RedactInputs && IsInput(kv.Key)) || (p.RedactOutputs && IsOutput(kv.Key)) {
			continue
		}
		if p.Attribute != nil {
			var keep bool
			if kv, keep = p.Attribute(kv); !keep {
				continue
			}
		}
		out = append(out, kv)
	}
	return out
}
