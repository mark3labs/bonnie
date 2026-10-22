// Package otelspan rewrites finished OpenTelemetry spans on their way to an
// exporter. A span processor cannot do it: OnEnd receives a read-only span.
package otelspan

import (
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// span is a finished span with replaced attributes and events. Embedding
// the interface keeps every other method, including the SDK's unexported
// one, so the result is still a ReadOnlySpan.
type span struct {
	sdktrace.ReadOnlySpan
	attrs  []attribute.KeyValue
	events []sdktrace.Event
}

func (s span) Attributes() []attribute.KeyValue { return s.attrs }
func (s span) Events() []sdktrace.Event         { return s.events }

// Rewrite returns s with attrs and events in place of its own.
func Rewrite(s sdktrace.ReadOnlySpan, attrs []attribute.KeyValue, events []sdktrace.Event) sdktrace.ReadOnlySpan {
	return span{ReadOnlySpan: s, attrs: attrs, events: events}
}

// Value returns the value of key on s, and false when s does not have it.
func Value(s sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}
