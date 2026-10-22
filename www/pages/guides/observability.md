---
title: Observability
description: Trace BONNIE turns with OpenTelemetry, control which content a span records, and send traces to Langfuse.
---

# Observability

BONNIE records each turn as OpenTelemetry spans. Use any OTLP backend: Honeycomb, Grafana Tempo, Datadog, Jaeger, or Langfuse. Tracing is off until you give BONNIE a tracer provider.

## The span tree

Each turn gives one trace:

```text
invoke_agent <agent>          one turn
  ├── chat <model>            one model step
  ├── execute_tool <tool>     one tool call
  └── chat <model>            the next step
```

The names and attributes follow the OpenTelemetry GenAI semantic conventions (`gen_ai.*`) where they define one, and use `bonnie.*` where they do not.

| Span | Records |
| --- | --- |
| `invoke_agent` | run ID, turn ID, turn kind, title, origin, final state, total tokens, suspension |
| `chat` | provider, model, step number, input, output, cache and reasoning tokens, finish reason, retries |
| `execute_tool` | tool name, tool call ID, tool kind, error state |

Every span has `gen_ai.conversation.id`, which is the run ID. A backend can group the turns of one run into one session with it.

## Turns that span processes and days

A durable run does not fit in one trace. BONNIE keeps these relations:

- **A turn that recovers interrupted work** is a child of the turn it recovers, in the same trace. BONNIE journals the span context of each turn, so this is also true when a different process does the recovery. A process that crashed never ended its span, so the parent can be missing in the backend. The spans still share the trace.
- **A turn that resumes after a human answer** starts a new trace with a span link to the turn that waited. No span stays open while a run waits.
- **A turn started by a traced request** is a child of the caller's span. The HTTP and NATS channels read the W3C `traceparent` header. A queued submission or a spawned child run journals the header with its input, so the parent stays after a restart.

Replay, snapshots, and event streams rebuilt from the journal emit no spans.

## Enable tracing

Build a tracer provider and give it to `WithTracing`. You own the provider: shut it down after `Serve` returns, or the last spans are lost.

```go
package main

import (
    "context"
    "log"

    "github.com/mark3labs/bonnie"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func main() {
    ctx := context.Background()
    exp, err := otlptracehttp.New(ctx) // reads OTEL_EXPORTER_OTLP_* variables
    if err != nil {
        log.Fatal(err)
    }
    tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
    defer func() { _ = tp.Shutdown(ctx) }()

    bonnie.New(bonnie.WithTracing(tp)).Serve()
}
```

## Content

By default, spans record **metadata only**: names, models, token counts, tool names, states, and errors. Prompts, model messages, tool arguments, tool results, and responses can contain personal or secret data. BONNIE records them only when a capture policy allows it:

```go
bonnie.WithTracing(tp, observe.WithCapture(func(t runtime.TurnStart) observe.Capture {
    // Record content for the HTTP channel only.
    on := t.Origin.Channel == "http"
    return observe.Capture{Inputs: on, Outputs: on}
}))
```

`observe.CaptureAll` records all content for every turn.

| Direction | Attributes |
| --- | --- |
| Inputs | `gen_ai.input.messages`, `gen_ai.tool.call.arguments`, human answer text |
| Outputs | `gen_ai.output.messages`, `gen_ai.tool.call.result`, the question of a suspension |

The capture policy is a ceiling for all destinations. To send one destination less, wrap its exporter with `observe.NewExporter`. A destination cannot get back content that the capture policy removed.

```go
honeycomb := observe.NewExporter(otlp, observe.ExportPolicy{
    RedactInputs:  true,
    RedactOutputs: true,
})
```

An `ExportPolicy` can also drop spans (`Span`) or rewrite single attributes (`Attribute`). Policies apply in order. A policy callback that panics drops the span.

## Langfuse

Package `observe/langfuse` sends traces to Langfuse over its OTLP endpoint (`/api/public/otel/v1/traces`). It maps BONNIE's spans to the Langfuse data model:

- `invoke_agent` becomes an agent observation, `chat` a generation with model and usage, and `execute_tool` a tool observation.
- The run ID becomes the Langfuse session, on every span.
- The run title, or else the agent name, becomes the trace name.
- Captured messages become the observation's input and output.

```go
cfg := langfuse.ConfigFromEnv()
tp, err := langfuse.NewTracerProvider(ctx, cfg, "release-bot")
if err != nil {
    log.Fatal(err)
}
defer func() { _ = tp.Shutdown(ctx) }()

scorer, err := langfuse.NewScorer(cfg)
if err != nil {
    log.Fatal(err)
}
defer func() { _ = scorer.Close(ctx) }()

bonnie.New(bonnie.WithTracing(tp, observe.WithAnswerHook(scorer.Answer))).Serve()
```

`ConfigFromEnv` reads the variables the Langfuse SDKs use:

| Variable | Meaning |
| --- | --- |
| `LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY` | Project API keys. Both are required. |
| `LANGFUSE_BASE_URL` or `LANGFUSE_HOST` | Base URL. The default is `https://cloud.langfuse.com` (EU). |
| `LANGFUSE_TRACING_ENVIRONMENT` | Sets `langfuse.environment` on every span. |
| `LANGFUSE_RELEASE` | Sets `langfuse.release` on every span. |

The exporter sends `x-langfuse-ingestion-version: 4`, so traces show in real time. A self-hosted Langfuse needs v3.22.0 or later.

The `Scorer` records each approval answer as a `BOOLEAN` score named `approval` on the turn that asked for it. It uses the Langfuse REST API, which can show a score up to ten minutes late. A failed score goes to the OpenTelemetry error handler and never fails the run.

To send Langfuse less content than the capture policy allows, wrap its exporter:

```go
exp, err := langfuse.NewExporter(ctx, cfg)
// ...
tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(
    observe.NewExporter(exp, observe.ExportPolicy{RedactOutputs: true}),
))
```

## Limits

- Kit does not yet let BONNIE put the `chat` span into the provider request context, or wrap the provider's HTTP transport. Provider HTTP spans (for example from `otelhttp`) are therefore not children of `chat`. See [mark3labs/kit#248](https://github.com/mark3labs/kit/issues/248).
- The model on a `chat` span is the agent's model when the turn's agent starts. A model change during a turn is not shown.
- Inbound `traceparent` is accepted from every caller that passes the channel's authentication.
