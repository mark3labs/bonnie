package langfuse

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/mark3labs/bonnie/internal/otelspan"
	"github.com/mark3labs/bonnie/observe"
)

// Langfuse attribute keys. See
// https://langfuse.com/integrations/native/opentelemetry.
const (
	KeyObservationType   = attribute.Key("langfuse.observation.type")
	KeyObservationInput  = attribute.Key("langfuse.observation.input")
	KeyObservationOutput = attribute.Key("langfuse.observation.output")
	KeyUsageDetails      = attribute.Key("langfuse.observation.usage_details")
	KeySessionID         = attribute.Key("langfuse.session.id")
	KeyTraceName         = attribute.Key("langfuse.trace.name")
	KeyEnvironment       = attribute.Key("langfuse.environment")
	KeyRelease           = attribute.Key("langfuse.release")
)

// exporter maps each span, then hands it to the OTLP exporter.
type exporter struct {
	next sdktrace.SpanExporter
	cfg  Config
}

func (e *exporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = e.cfg.Map(s)
	}
	return e.next.ExportSpans(ctx, out)
}

func (e *exporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

// observationTypes maps gen_ai.operation.name to a Langfuse observation type.
// A valid explicit type always wins in Langfuse, so a chat span is a
// generation even if a later Langfuse release infers types differently.
var observationTypes = map[string]string{
	observe.OperationInvokeAgent: "agent",
	observe.OperationChat:        "generation",
	observe.OperationExecuteTool: "tool",
}

// Map returns s with the Langfuse attributes added. The content attributes
// gen_ai.input.messages and gen_ai.output.messages move to the observation's
// input and output, so they are not sent twice. A span that BONNIE did not
// make keeps its own attributes and gets only the environment and release.
func (c Config) Map(s sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	src := s.Attributes()
	attrs := make([]attribute.KeyValue, 0, len(src)+6)
	var operation, title, agent string
	usage := map[string]int64{}
	for _, kv := range src {
		switch kv.Key {
		case observe.KeyOperationName:
			operation = kv.Value.AsString()
		case observe.KeyRunTitle:
			title = kv.Value.AsString()
		case observe.KeyAgentName:
			agent = kv.Value.AsString()
		case observe.KeyConversationID:
			attrs = append(attrs, KeySessionID.String(kv.Value.AsString()))
		case observe.KeyInputMessages:
			attrs = append(attrs, KeyObservationInput.String(kv.Value.AsString()))
			continue
		case observe.KeyOutputMessages:
			attrs = append(attrs, KeyObservationOutput.String(kv.Value.AsString()))
			continue
		case observe.KeyUsageInputTokens:
			usage["input"] = kv.Value.AsInt64()
		case observe.KeyUsageOutputTokens:
			usage["output"] = kv.Value.AsInt64()
		case observe.KeyUsageCacheReadTokens:
			usage["cache_read_input_tokens"] = kv.Value.AsInt64()
		case observe.KeyUsageCacheCreationTokens:
			usage["cache_creation_input_tokens"] = kv.Value.AsInt64()
		}
		attrs = append(attrs, kv)
	}
	if typ, ok := observationTypes[operation]; ok {
		attrs = append(attrs, KeyObservationType.String(typ))
	}
	switch {
	case title != "":
		attrs = append(attrs, KeyTraceName.String(title))
	case agent != "":
		attrs = append(attrs, KeyTraceName.String(agent))
	}
	if operation == observe.OperationChat && len(usage) > 0 {
		if data, err := json.Marshal(usage); err == nil {
			attrs = append(attrs, KeyUsageDetails.String(string(data)))
		}
	}
	if c.Environment != "" {
		attrs = append(attrs, KeyEnvironment.String(c.Environment))
	}
	if c.Release != "" {
		attrs = append(attrs, KeyRelease.String(c.Release))
	}
	return otelspan.Rewrite(s, attrs, s.Events())
}
