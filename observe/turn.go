package observe

import (
	"context"
	"strings"
	"sync"

	kit "github.com/mark3labs/kit/pkg/kit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/mark3labs/bonnie/runtime"
)

// StartTurn implements [runtime.Observer].
func (t *Tracer) StartTurn(ctx context.Context, ts runtime.TurnStart) (context.Context, runtime.TurnObservation) {
	var capture Capture
	if t.capture != nil {
		capture = t.capture(ts)
	}
	previous := spanContextFrom(ts.Previous)

	parent := ctx
	var links []trace.Link
	switch {
	case ts.Kind == runtime.TurnRecovery && previous.IsValid():
		parent = trace.ContextWithRemoteSpanContext(ctx, previous)
	case spanContextFrom(ts.Parent).IsValid():
		parent = trace.ContextWithRemoteSpanContext(ctx, spanContextFrom(ts.Parent))
	}
	if ts.Kind == runtime.TurnResume && previous.IsValid() {
		links = append(links, trace.Link{
			SpanContext: previous,
			Attributes:  []attribute.KeyValue{KeyLinkType.String("resume")},
		})
	}

	common := []attribute.KeyValue{
		KeyConversationID.String(ts.RunID),
		KeyRunID.String(ts.RunID),
		KeyTurnID.String(ts.TurnID),
	}
	if t.agentName != "" {
		common = append(common, KeyAgentName.String(t.agentName))
	}
	if ts.Title != "" {
		common = append(common, KeyRunTitle.String(ts.Title))
	}
	attrs := append([]attribute.KeyValue{
		KeyOperationName.String(OperationInvokeAgent),
		KeyTurnKind.String(string(ts.Kind)),
	}, common...)
	if ts.Origin.Channel != "" {
		attrs = append(attrs, KeyOriginChan.String(ts.Origin.Channel))
	}
	if ts.Origin.Kind != "" {
		attrs = append(attrs, KeyOriginKind.String(ts.Origin.Kind))
	}
	if capture.Inputs && ts.Prompt != "" {
		attrs = append(attrs, KeyInputMessages.String(encodeMessages([]genAIMessage{
			{Role: "user", Parts: []genAIPart{textPart(ts.Prompt)}},
		})))
	}

	name := OperationInvokeAgent
	if t.agentName != "" {
		name += " " + t.agentName
	}
	turnCtx, span := t.tracer.Start(parent, name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
		trace.WithLinks(links...),
	)
	o := &turn{
		tracer:  t.tracer,
		ctx:     turnCtx,
		span:    span,
		common:  common,
		capture: capture,
		tools:   make(map[string]trace.Span),
	}
	if ts.Kind == runtime.TurnResume {
		o.answered(ts, previous, t.onAnswer)
	}
	return turnCtx, o
}

// turn is the observation of one turn. Kit delivers events on the agent's
// goroutine, but tools can run in parallel, so every field after mu is
// guarded.
type turn struct {
	tracer  trace.Tracer
	ctx     context.Context
	span    trace.Span
	common  []attribute.KeyValue
	capture Capture

	mu    sync.Mutex
	input string // the next step's input messages, from PrepareStep
	step  trace.Span
	// opened is true from the step-context hook that opened step until its
	// StepStartEvent, which then must not open a second span.
	opened   bool
	stepText strings.Builder
	calls    []genAIPart
	tools    map[string]trace.Span
	ended    bool
}

// Carrier implements [runtime.TurnObservation].
func (o *turn) Carrier() map[string]string {
	return carrierFor(o.span.SpanContext())
}

// Attach implements [runtime.TurnObservation].
func (o *turn) Attach(agent runtime.Agent) func() {
	var stops []func()
	if src, ok := agent.(eventSource); ok {
		stops = append(stops, src.Subscribe(o.event))
	}
	if sc, ok := agent.(stepContexter); ok {
		// Low priority runs last, so the chat span is the active span of the
		// provider request even when another hook adds a span of its own.
		stops = append(stops, sc.OnStepContext(kit.HookPriorityLow, o.stepContext))
	}
	if p, ok := agent.(stepPreparer); ok && o.capture.Inputs {
		// High priority runs first, and a nil result lets every other hook
		// run, so this only reads the step's messages.
		stops = append(stops, p.OnPrepareStep(kit.HookPriorityHigh, func(h kit.PrepareStepHook) *kit.PrepareStepResult {
			input := encodeMessages(fromKit(h.Messages))
			o.mu.Lock()
			o.input = input
			o.mu.Unlock()
			return nil
		}))
	}
	return func() {
		for _, stop := range stops {
			stop()
		}
	}
}

// event turns one live Kit event into span work.
func (o *turn) event(ev kit.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended {
		return
	}
	switch e := ev.(type) {
	case kit.StepStartEvent:
		if o.opened {
			o.opened = false
			return
		}
		o.endStep(codes.Error, "step superseded")
		o.startStep(e.StepNumber, e.Model)
	case kit.MessageUpdateEvent:
		if o.step != nil && o.capture.Outputs {
			o.stepText.WriteString(e.Chunk)
		}
	case kit.ToolCallEvent:
		if o.step != nil && o.capture.Outputs {
			o.calls = append(o.calls, toolCallPart(e.ToolCallID, e.ToolName, e.ToolArgs))
		}
	case kit.StepFinishEvent:
		if o.step == nil {
			return
		}
		o.step.SetAttributes(usageAttributes(e.Usage)...)
		if e.FinishReason != "" {
			o.step.SetAttributes(KeyFinishReasons.StringSlice([]string{e.FinishReason}))
		}
		if o.capture.Outputs {
			parts := o.calls
			if text := o.stepText.String(); text != "" {
				parts = append([]genAIPart{textPart(text)}, parts...)
			}
			if len(parts) > 0 {
				o.step.SetAttributes(KeyOutputMessages.String(encodeMessages([]genAIMessage{
					{Role: "assistant", Parts: parts, FinishReason: e.FinishReason},
				})))
			}
		}
		o.endStep(codes.Unset, "")
	case kit.StepResumeEvent:
		if o.step != nil {
			attrs := []attribute.KeyValue{attribute.Int("bonnie.step.resume", e.Resume)}
			if e.Error != nil {
				o.step.RecordError(e.Error)
			}
			o.step.AddEvent(EventStepResume, trace.WithAttributes(attrs...))
		}
		o.endStep(codes.Error, "step restarted")
	case kit.RetryEvent:
		target := o.span
		if o.step != nil {
			target = o.step
		}
		attrs := []attribute.KeyValue{
			attribute.Int("bonnie.retry.attempt", e.Attempt),
			attribute.Int64("bonnie.retry.delay_ms", e.Delay.Milliseconds()),
		}
		if e.Error != nil {
			attrs = append(attrs, KeyErrorType.String("provider_error"))
		}
		target.AddEvent(EventRetry, trace.WithAttributes(attrs...))
	case kit.ToolExecutionStartEvent:
		o.startTool(e.ToolCallID, e.ToolName, e.ToolKind, e.ToolArgs)
	case kit.ToolResultEvent:
		span, ok := o.tools[e.ToolCallID]
		if !ok {
			// A result without a start, for example a blocked call.
			o.startTool(e.ToolCallID, e.ToolName, e.ToolKind, e.ToolArgs)
			span = o.tools[e.ToolCallID]
		}
		delete(o.tools, e.ToolCallID)
		if o.capture.Outputs {
			span.SetAttributes(KeyToolResult.String(e.Result))
		}
		if e.IsError {
			span.SetAttributes(KeyErrorType.String("tool_error"))
			span.SetStatus(codes.Error, "tool returned an error")
		}
		span.End()
	case kit.CompactionEvent:
		attrs := []attribute.KeyValue{
			attribute.Int("bonnie.compaction.original_tokens", e.OriginalTokens),
			attribute.Int("bonnie.compaction.compacted_tokens", e.CompactedTokens),
			attribute.Int("bonnie.compaction.messages_removed", e.MessagesRemoved),
		}
		if e.Err != nil {
			attrs = append(attrs, KeyErrorType.String("compaction_failed"))
		}
		o.span.AddEvent(EventCompaction, trace.WithAttributes(attrs...))
	case kit.SteerConsumedEvent:
		o.span.AddEvent(EventSteer, trace.WithAttributes(attribute.Int("bonnie.steer.count", e.Count)))
	}
}

// stepContext opens the chat span of a step before the step starts, and
// puts it into the step's context. Kit gives that context to the provider
// request, so an HTTP span (for example from an otelhttp transport set with
// kit.WithHTTPTransport) is a child of the chat span. Kit calls the hook
// again for a resumed step, after the StepResumeEvent that ended its span.
func (o *turn) stepContext(h kit.StepContextHook) context.Context {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended {
		return nil
	}
	o.endStep(codes.Error, "step superseded")
	o.startStep(h.StepNumber, h.Model)
	o.opened = true
	return trace.ContextWithSpan(h.Context, o.step)
}

// startStep opens a chat span for one model step. model is the
// "provider/model" string Kit reports for the step. Callers hold mu.
func (o *turn) startStep(number int, model string) {
	provider, id, err := kit.ParseModelString(model)
	if err != nil {
		provider, id = "", model
	}
	name := OperationChat
	if id != "" {
		name += " " + id
	}
	attrs := append([]attribute.KeyValue{
		KeyOperationName.String(OperationChat),
		KeyStepNumber.Int(number),
	}, o.common...)
	if provider != "" {
		attrs = append(attrs, KeyProviderName.String(provider))
	}
	if id != "" {
		attrs = append(attrs, KeyRequestModel.String(id))
	}
	if o.input != "" {
		attrs = append(attrs, KeyInputMessages.String(o.input))
		o.input = ""
	}
	_, o.step = o.tracer.Start(o.ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	o.stepText.Reset()
	o.calls = nil
}

// endStep closes the open chat span, if any. Callers hold mu.
func (o *turn) endStep(code codes.Code, why string) {
	if o.step == nil {
		return
	}
	if code == codes.Error {
		o.step.SetStatus(code, why)
	}
	o.step.End()
	o.step = nil
	o.opened = false
	o.stepText.Reset()
	o.calls = nil
}

// startTool opens an execute_tool span. Tool spans are siblings of the chat
// span that asked for them, as the GenAI conventions draw them. Callers
// hold mu.
func (o *turn) startTool(id, name, kind, args string) {
	attrs := append([]attribute.KeyValue{
		KeyOperationName.String(OperationExecuteTool),
		KeyToolName.String(name),
		KeyToolType.String("function"),
		KeyToolCallID.String(id),
	}, o.common...)
	if kind != "" {
		attrs = append(attrs, KeyToolKind.String(kind))
	}
	if o.capture.Inputs && args != "" {
		attrs = append(attrs, KeyToolArguments.String(args))
	}
	_, span := o.tracer.Start(o.ctx, OperationExecuteTool+" "+name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	o.tools[id] = span
}

// answered records the answers that resume a run.
func (o *turn) answered(ts runtime.TurnStart, waited trace.SpanContext, hook func(context.Context, Answer)) {
	for _, resp := range ts.Responses {
		attrs := []attribute.KeyValue{}
		if resp.Approved != nil {
			attrs = append(attrs, attribute.Bool("bonnie.answer.approved", *resp.Approved))
		}
		text := ""
		if o.capture.Inputs {
			text = resp.Text
			if text != "" {
				attrs = append(attrs, KeyAnswerText.String(text))
			}
		}
		o.span.AddEvent(EventResumed, trace.WithAttributes(attrs...))
		if hook != nil {
			hook(o.ctx, Answer{RunID: ts.RunID, TurnID: resp.TurnID, Waited: waited, Approved: resp.Approved, Text: text})
		}
	}
}

// End implements [runtime.TurnObservation].
func (o *turn) End(end runtime.TurnEnd) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended {
		return
	}
	o.ended = true
	o.endStep(codes.Error, "turn ended during the step")
	for id, span := range o.tools {
		span.SetStatus(codes.Error, "turn ended during the tool call")
		span.End()
		delete(o.tools, id)
	}

	run := end.Run
	if run != nil {
		o.span.SetAttributes(KeyRunState.String(string(run.State)))
		if run.Usage != nil {
			o.span.SetAttributes(
				KeyTurnInputTok.Int64(run.Usage.InputTokens),
				KeyTurnOutTok.Int64(run.Usage.OutputTokens),
			)
		}
		if o.capture.Outputs && run.Response != "" {
			o.span.SetAttributes(KeyOutputMessages.String(encodeMessages([]genAIMessage{
				{Role: "assistant", Parts: []genAIPart{textPart(run.Response)}},
			})))
		}
		if sus := run.Suspend; run.State == runtime.RunWaiting && sus != nil {
			attrs := []attribute.KeyValue{KeySuspendKind.String(sus.Kind)}
			if sus.ToolCallID != "" {
				attrs = append(attrs, KeySuspendTool.String(sus.ToolCallID))
			}
			if o.capture.Outputs && sus.Prompt != "" {
				attrs = append(attrs, KeySuspendPrompt.String(sus.Prompt))
			}
			o.span.AddEvent(EventSuspended, trace.WithAttributes(attrs...))
		}
	}
	err := end.Err
	if err == nil && run != nil {
		err = run.Err
	}
	if err != nil || (run != nil && run.State == runtime.RunFailed) {
		o.span.SetAttributes(KeyErrorType.String("turn_failed"))
		msg := "turn failed"
		if err != nil {
			o.span.RecordError(err)
			msg = err.Error()
		}
		o.span.SetStatus(codes.Error, msg)
	}
	o.span.End()
}

// usageAttributes reports the token counts Kit gives for one step.
func usageAttributes(u kit.LLMUsage) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		KeyUsageInputTokens.Int64(u.InputTokens),
		KeyUsageOutputTokens.Int64(u.OutputTokens),
	}
	if u.CacheReadTokens != 0 {
		attrs = append(attrs, KeyUsageCacheReadTokens.Int64(u.CacheReadTokens))
	}
	if u.CacheCreationTokens != 0 {
		attrs = append(attrs, KeyUsageCacheCreationTokens.Int64(u.CacheCreationTokens))
	}
	if u.ReasoningTokens != 0 {
		attrs = append(attrs, KeyUsageReasoningTokens.Int64(u.ReasoningTokens))
	}
	return attrs
}
