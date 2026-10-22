package observe

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Span names, as gen_ai.operation.name values.
const (
	OperationInvokeAgent = "invoke_agent"
	OperationChat        = "chat"
	OperationExecuteTool = "execute_tool"
)

// Attribute keys. The gen_ai.* keys are from the OpenTelemetry GenAI
// semantic conventions.
const (
	KeyOperationName  = attribute.Key("gen_ai.operation.name")
	KeyAgentName      = attribute.Key("gen_ai.agent.name")
	KeyConversationID = attribute.Key("gen_ai.conversation.id")
	KeyProviderName   = attribute.Key("gen_ai.provider.name")
	KeyRequestModel   = attribute.Key("gen_ai.request.model")
	KeyFinishReasons  = attribute.Key("gen_ai.response.finish_reasons")

	KeyUsageInputTokens         = attribute.Key("gen_ai.usage.input_tokens")
	KeyUsageOutputTokens        = attribute.Key("gen_ai.usage.output_tokens")
	KeyUsageCacheReadTokens     = attribute.Key("gen_ai.usage.cache_read.input_tokens")
	KeyUsageCacheCreationTokens = attribute.Key("gen_ai.usage.cache_creation.input_tokens")
	KeyUsageReasoningTokens     = attribute.Key("gen_ai.usage.reasoning.output_tokens")

	KeyToolName   = attribute.Key("gen_ai.tool.name")
	KeyToolType   = attribute.Key("gen_ai.tool.type")
	KeyToolCallID = attribute.Key("gen_ai.tool.call.id")
	KeyErrorType  = attribute.Key("error.type")

	KeyRunID        = attribute.Key("bonnie.run.id")
	KeyRunTitle     = attribute.Key("bonnie.run.title")
	KeyRunState     = attribute.Key("bonnie.run.state")
	KeyTurnID       = attribute.Key("bonnie.turn.id")
	KeyTurnKind     = attribute.Key("bonnie.turn.kind")
	KeyOriginChan   = attribute.Key("bonnie.origin.channel")
	KeyOriginKind   = attribute.Key("bonnie.origin.kind")
	KeyStepNumber   = attribute.Key("bonnie.step.number")
	KeyToolKind     = attribute.Key("bonnie.tool.kind")
	KeyLinkType     = attribute.Key("bonnie.link.type")
	KeySuspendKind  = attribute.Key("bonnie.suspend.kind")
	KeySuspendTool  = attribute.Key("bonnie.suspend.tool_call_id")
	KeyTurnInputTok = attribute.Key("bonnie.usage.input_tokens")
	KeyTurnOutTok   = attribute.Key("bonnie.usage.output_tokens")
)

// Content attribute keys. They are set only when the [CapturePolicy] allows
// it, and [ExportPolicy] redaction removes them.
const (
	// KeyInputMessages holds the messages a span received, as a JSON
	// array in the GenAI message format.
	KeyInputMessages = attribute.Key("gen_ai.input.messages")
	// KeyToolArguments holds a tool call's JSON arguments.
	KeyToolArguments = attribute.Key("gen_ai.tool.call.arguments")
	// KeyAnswerText holds a human answer on a resume event.
	KeyAnswerText = attribute.Key("bonnie.answer.text")

	// KeyOutputMessages holds the messages a span produced, as a JSON
	// array in the GenAI message format.
	KeyOutputMessages = attribute.Key("gen_ai.output.messages")
	// KeyToolResult holds a tool call's result.
	KeyToolResult = attribute.Key("gen_ai.tool.call.result")
	// KeySuspendPrompt holds the question a suspension asks.
	KeySuspendPrompt = attribute.Key("bonnie.suspend.prompt")
)

// Span event names.
const (
	EventSuspended  = "bonnie.run.suspended"
	EventResumed    = "bonnie.run.resumed"
	EventRetry      = "bonnie.model.retry"
	EventStepResume = "bonnie.step.resume"
	EventCompaction = "bonnie.compaction"
	EventSteer      = "bonnie.steer"
)

// IsInput reports whether key is input content.
func IsInput(key attribute.Key) bool {
	return key == KeyInputMessages || key == KeyToolArguments || key == KeyAnswerText
}

// IsOutput reports whether key is output content.
func IsOutput(key attribute.Key) bool {
	return key == KeyOutputMessages || key == KeyToolResult || key == KeySuspendPrompt
}

// propagator reads and writes W3C trace context only. BONNIE does not use
// the global propagator: it is a no-op until an application sets one, and
// the journal format must not depend on what an application set.
var propagator = propagation.TraceContext{}

// spanContextFrom decodes a W3C carrier. It returns an invalid span context
// for a nil, empty, or malformed carrier.
func spanContextFrom(carrier map[string]string) trace.SpanContext {
	if len(carrier) == 0 {
		return trace.SpanContext{}
	}
	ctx := propagator.Extract(context.Background(), propagation.MapCarrier(carrier))
	return trace.SpanContextFromContext(ctx)
}

// carrierFor encodes a span context as a W3C carrier. It returns nil for an
// invalid span context, which is what a no-op provider gives.
func carrierFor(sc trace.SpanContext) map[string]string {
	if !sc.IsValid() {
		return nil
	}
	carrier := propagation.MapCarrier{}
	propagator.Inject(trace.ContextWithSpanContext(context.Background(), sc), carrier)
	return carrier
}
