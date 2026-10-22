// Package observe traces BONNIE runs with OpenTelemetry.
//
// [New] returns a [runtime.Observer] that turns each executed turn into a
// span tree:
//
//	invoke_agent <agent>          one turn
//	  ├── chat <model>            one model step
//	  └── execute_tool <tool>     one tool call
//
// The names and attributes follow the OpenTelemetry GenAI semantic
// conventions where they define one (gen_ai.*), and use bonnie.* where they
// do not. Every span carries gen_ai.conversation.id, which is the run ID, so
// a backend can group the turns of one run into one session.
//
// # One trace per turn
//
// A turn starts a new trace, or joins the trace of the request that
// delivered its input ([runtime.Input.Trace], or a span in the context the
// run started with). Two cases keep a relation to an earlier turn:
//
//   - A turn that recovers interrupted work is a child of the turn it
//     recovers, in the same trace. The runtime journals each turn's span
//     context, so this holds when a second process does the recovery. A
//     process that crashed never ended its span, so the recovered turn's
//     parent can be missing from the backend; its spans still share the
//     trace.
//   - A turn that resumes a run after a human answer starts a new trace
//     with a link to the turn that waited. No span stays open while a run
//     waits, which can be days.
//
// # Content
//
// By default spans carry metadata only: names, models, token counts, tool
// names, states, errors. Prompts, model messages, tool arguments, tool
// results, and responses can hold personal or secret data, so they are
// recorded only when a [CapturePolicy] allows it ([WithCapture]). The policy
// is a ceiling: an exporter wrapped with [NewExporter] can remove more, but
// nothing can add back what the policy kept out.
//
// # Ownership
//
// The caller owns the TracerProvider: it chooses the exporters and the
// sampler, and it must shut the provider down before the process exits, or
// the last spans are lost.
package observe

import (
	"context"

	kit "github.com/mark3labs/kit/pkg/kit"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/mark3labs/bonnie/runtime"
)

// ScopeName is the instrumentation scope of BONNIE's spans.
const ScopeName = "github.com/mark3labs/bonnie/observe"

// Capture says which content a turn's spans may record.
type Capture struct {
	// Inputs allows what goes into the agent: the prompt, the messages
	// each model step receives, tool arguments, and human answers.
	Inputs bool
	// Outputs allows what comes out: model text and tool calls, tool
	// results, the response, and the question of a suspension.
	Outputs bool
}

// CapturePolicy decides the content ceiling for one turn. It can look at
// the run's [runtime.Origin], for example to record content in development
// only, or for one channel only.
type CapturePolicy func(runtime.TurnStart) Capture

// CaptureAll records all content for every turn.
func CaptureAll(runtime.TurnStart) Capture { return Capture{Inputs: true, Outputs: true} }

// Answer is a human answer that resumed a waiting run. See [WithAnswerHook].
type Answer struct {
	RunID string
	// TurnID is the turn the answer resumes.
	TurnID string
	// Waited is the span context of the turn that waited for the answer.
	Waited trace.SpanContext
	// Approved is the verdict of an approval. Nil means the answer was not
	// an approval.
	Approved *bool
	// Text is the answer's text. It is empty unless the turn's
	// [Capture.Inputs] allows it.
	Text string
}

// Option configures a [Tracer].
type Option func(*Tracer)

// WithAgentName names the agent in span names and in gen_ai.agent.name.
func WithAgentName(name string) Option {
	return func(t *Tracer) { t.agentName = name }
}

// WithCapture sets the content ceiling. Nil records no content, which is the
// default.
func WithCapture(p CapturePolicy) Option {
	return func(t *Tracer) { t.capture = p }
}

// WithAnswerHook calls fn for each human answer that resumes a run, so an
// adapter can record it outside the trace: langfuse.Scorer records an
// approval as a score. fn runs on the turn's goroutine and must return
// promptly; an adapter that does I/O does it in the background.
func WithAnswerHook(fn func(context.Context, Answer)) Option {
	return func(t *Tracer) { t.onAnswer = fn }
}

// Tracer is the [runtime.Observer] that records turns as spans.
type Tracer struct {
	tracer    trace.Tracer
	agentName string
	capture   CapturePolicy
	onAnswer  func(context.Context, Answer)
}

var _ runtime.Observer = (*Tracer)(nil)

// New returns a Tracer that records spans through tp. A nil tp records
// nothing.
func New(tp trace.TracerProvider, opts ...Option) *Tracer {
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	t := &Tracer{tracer: tp.Tracer(ScopeName)}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// The optional parts of a [runtime.Agent] the tracer reads. *kit.Kit has
// all of them; a test double can have none.
type (
	eventSource interface {
		Subscribe(listener kit.EventListener) func()
	}
	stepContexter interface {
		OnStepContext(p kit.HookPriority, h func(kit.StepContextHook) context.Context) func()
	}
	stepPreparer interface {
		OnPrepareStep(p kit.HookPriority, h func(kit.PrepareStepHook) *kit.PrepareStepResult) func()
	}
)
