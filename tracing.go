package bonnie

import (
	"go.opentelemetry.io/otel/trace"

	"github.com/mark3labs/bonnie/observe"
)

// WithTracing records each turn as OpenTelemetry spans through tp: one
// invoke_agent span per turn, with a chat span per model step and an
// execute_tool span per tool call. The agent's name ([WithName]) names the
// spans. See package observe for the span tree and for what a span records.
//
// Spans carry metadata only unless an option allows content:
//
//	bonnie.WithTracing(tp, observe.WithCapture(observe.CaptureAll))
//
// The caller owns tp and must shut it down after [Agent.Serve] or
// [Agent.Run] returns, or the last spans are lost. A nil tp disables
// tracing. To send traces to Langfuse, build tp with package
// observe/langfuse.
func WithTracing(tp trace.TracerProvider, opts ...observe.Option) Option {
	return func(c *config) {
		c.tracerProvider = tp
		c.traceOptions = opts
	}
}
