package observe_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/mark3labs/bonnie/observe"
)

// exportOne records one span with attrs and an event, sends it through
// policies, and returns what reached the destination.
func exportOne(t *testing.T, policies ...observe.ExportPolicy) []tracetest.SpanStub {
	t.Helper()
	sink := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(observe.NewExporter(sink, policies...)))
	_, span := tp.Tracer("test").Start(context.Background(), "chat m")
	span.SetAttributes(
		observe.KeyOperationName.String(observe.OperationChat),
		observe.KeyInputMessages.String("secret prompt"),
		observe.KeyOutputMessages.String("secret answer"),
		observe.KeyToolArguments.String(`{"token":"x"}`),
	)
	span.AddEvent(observe.EventSuspended, trace.WithAttributes(observe.KeySuspendPrompt.String("may I?")))
	span.End()
	// Read before Shutdown: shutting the in-memory exporter down clears it.
	spans := sink.GetSpans()
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	return spans
}

func has(attrs []attribute.KeyValue, key attribute.Key) bool {
	for _, kv := range attrs {
		if kv.Key == key {
			return true
		}
	}
	return false
}

func TestExportPolicyRedactsContentFromSpansAndEvents(t *testing.T) {
	t.Parallel()
	spans := exportOne(t, observe.ExportPolicy{RedactInputs: true, RedactOutputs: true})
	if len(spans) != 1 {
		t.Fatalf("%d spans exported", len(spans))
	}
	s := spans[0]
	for _, key := range []attribute.Key{observe.KeyInputMessages, observe.KeyOutputMessages, observe.KeyToolArguments} {
		if has(s.Attributes, key) {
			t.Fatalf("redacted span still has %s", key)
		}
	}
	if !has(s.Attributes, observe.KeyOperationName) {
		t.Fatal("redaction removed metadata")
	}
	if len(s.Events) != 1 || has(s.Events[0].Attributes, observe.KeySuspendPrompt) {
		t.Fatalf("event attributes = %v, want the prompt removed", s.Events)
	}
}

func TestExportPolicyRedactsOneDirection(t *testing.T) {
	t.Parallel()
	s := exportOne(t, observe.ExportPolicy{RedactInputs: true})[0]
	if has(s.Attributes, observe.KeyInputMessages) || !has(s.Attributes, observe.KeyOutputMessages) {
		t.Fatalf("attributes = %v, want inputs gone and outputs kept", s.Attributes)
	}
}

func TestExportPolicyDropsAndRewrites(t *testing.T) {
	t.Parallel()
	if spans := exportOne(t, observe.ExportPolicy{Span: func(sdktrace.ReadOnlySpan) bool { return false }}); len(spans) != 0 {
		t.Fatalf("%d spans exported, want the span dropped", len(spans))
	}
	spans := exportOne(t, observe.ExportPolicy{Attribute: func(kv attribute.KeyValue) (attribute.KeyValue, bool) {
		if kv.Key == observe.KeyToolArguments {
			return kv.Key.String("[redacted]"), true
		}
		return kv, kv.Key != observe.KeyOutputMessages
	}})
	s := spans[0]
	if has(s.Attributes, observe.KeyOutputMessages) {
		t.Fatal("a dropped attribute was exported")
	}
	for _, kv := range s.Attributes {
		if kv.Key == observe.KeyToolArguments && kv.Value.AsString() != "[redacted]" {
			t.Fatalf("arguments = %q, want the replacement", kv.Value.AsString())
		}
	}
}

// A policy callback that panics must not take the exporter down, and it must
// not export the span it could not judge.
func TestExportPolicyPanicDropsTheSpan(t *testing.T) {
	t.Parallel()
	spans := exportOne(t, observe.ExportPolicy{Span: func(sdktrace.ReadOnlySpan) bool { panic("bad policy") }})
	if len(spans) != 0 {
		t.Fatalf("%d spans exported after a panic", len(spans))
	}
}

// Policies apply in order; a later one sees what an earlier one left.
func TestExportPoliciesApplyInOrder(t *testing.T) {
	t.Parallel()
	var saw bool
	spans := exportOne(t,
		observe.ExportPolicy{RedactInputs: true},
		observe.ExportPolicy{Span: func(s sdktrace.ReadOnlySpan) bool {
			for _, kv := range s.Attributes() {
				if kv.Key == observe.KeyInputMessages {
					saw = true
				}
			}
			return true
		}},
	)
	if len(spans) != 1 || saw {
		t.Fatalf("the second policy saw input content the first removed (spans %d)", len(spans))
	}
}
