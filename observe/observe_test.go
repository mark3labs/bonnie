package observe_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/observe"
	"github.com/mark3labs/bonnie/runtime"
)

// hermetic keeps a real Kit from reading the machine it runs on. See
// runtime/kit_seams_test.go.
func hermetic() kit.Option {
	return func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.DisableCoreTools = true
		o.Quiet = true
	}
}

func scriptedKit(m *fakemodel.Model, opts ...kit.Option) runtime.AgentFactory {
	return runtime.KitAgent(append([]kit.Option{hermetic(), m.Option()}, opts...)...)
}

func deployTool() kit.Tool {
	return kit.NewTool("deploy", "Deploy the app.",
		func(context.Context, struct{}) (kit.ToolOutput, error) {
			return kit.ToolOutput{Content: "deployed"}, nil
		})
}

// recorder is one process's tracer provider.
func recorder() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	sr := tracetest.NewSpanRecorder()
	return sr, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
}

func attr(s sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func str(s sdktrace.ReadOnlySpan, key attribute.Key) string {
	v, _ := attr(s, key)
	return v.AsString()
}

func byOperation(spans []sdktrace.ReadOnlySpan, op string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if str(s, observe.KeyOperationName) == op {
			out = append(out, s)
		}
	}
	return out
}

func one(t *testing.T, spans []sdktrace.ReadOnlySpan, op string) sdktrace.ReadOnlySpan {
	t.Helper()
	got := byOperation(spans, op)
	if len(got) != 1 {
		t.Fatalf("%d %s spans, want 1", len(got), op)
	}
	return got[0]
}

func TestTurnSpanTreeWithRealKit(t *testing.T) {
	t.Parallel()
	sr, tp := recorder()
	model := fakemodel.New(fakemodel.Call("deploy", `{}`), fakemodel.Say("Done."))
	r := runtime.NewRunner(runtime.NewMemoryJournal(), scriptedKit(model, kit.WithExtraTools(deployTool())),
		runtime.WithObserver(observe.New(tp, observe.WithAgentName("ops"))))

	run, err := r.Start(context.Background(), "trace-1", runtime.Input{Text: "Deploy.", Title: "Deploy request"})
	if err != nil || run.State != runtime.RunCompleted {
		t.Fatalf("Start = %+v, %v", run, err)
	}
	spans := sr.Ended()

	agent := one(t, spans, observe.OperationInvokeAgent)
	if agent.Name() != "invoke_agent ops" || agent.Parent().IsValid() {
		t.Fatalf("agent span %q, parent %v; want a root invoke_agent ops", agent.Name(), agent.Parent())
	}
	if str(agent, observe.KeyRunState) != string(runtime.RunCompleted) || str(agent, observe.KeyTurnID) != run.TurnID {
		t.Fatalf("agent span state %q turn %q", str(agent, observe.KeyRunState), str(agent, observe.KeyTurnID))
	}

	chats := byOperation(spans, observe.OperationChat)
	if len(chats) != 2 {
		t.Fatalf("%d chat spans, want one per model step", len(chats))
	}
	for _, c := range chats {
		if c.Parent().SpanID() != agent.SpanContext().SpanID() {
			t.Fatalf("chat span parent %v, want the agent span", c.Parent().SpanID())
		}
		if str(c, observe.KeyRequestModel) != fakemodel.ModelName || str(c, observe.KeyProviderName) != fakemodel.Provider {
			t.Fatalf("chat model %q provider %q", str(c, observe.KeyRequestModel), str(c, observe.KeyProviderName))
		}
		if v, _ := attr(c, observe.KeyUsageInputTokens); v.AsInt64() != 1 {
			t.Fatalf("chat input tokens = %v, want the scripted 1", v)
		}
		if c.SpanKind() != trace.SpanKindClient {
			t.Fatalf("chat span kind = %v", c.SpanKind())
		}
	}

	tool := one(t, spans, observe.OperationExecuteTool)
	if tool.Name() != "execute_tool deploy" || tool.Parent().SpanID() != agent.SpanContext().SpanID() {
		t.Fatalf("tool span %q parent %v", tool.Name(), tool.Parent().SpanID())
	}
	if str(tool, observe.KeyToolCallID) == "" || tool.Status().Code == codes.Error {
		t.Fatalf("tool span call id %q status %v", str(tool, observe.KeyToolCallID), tool.Status())
	}

	for _, s := range spans {
		if s.SpanContext().TraceID() != agent.SpanContext().TraceID() {
			t.Fatalf("span %q is in another trace", s.Name())
		}
		if str(s, observe.KeyConversationID) != "trace-1" || str(s, observe.KeyRunTitle) != "Deploy request" {
			t.Fatalf("span %q conversation %q title %q", s.Name(), str(s, observe.KeyConversationID), str(s, observe.KeyRunTitle))
		}
		// The default records no content.
		for _, kv := range s.Attributes() {
			if observe.IsInput(kv.Key) || observe.IsOutput(kv.Key) {
				t.Fatalf("span %q records %s with no capture policy", s.Name(), kv.Key)
			}
		}
	}
}

func TestCaptureAllRecordsContent(t *testing.T) {
	t.Parallel()
	sr, tp := recorder()
	model := fakemodel.New(fakemodel.Call("deploy", `{}`), fakemodel.Say("Done."))
	r := runtime.NewRunner(runtime.NewMemoryJournal(), scriptedKit(model, kit.WithExtraTools(deployTool())),
		runtime.WithObserver(observe.New(tp, observe.WithCapture(observe.CaptureAll))))
	if _, err := r.Start(context.Background(), "trace-2", runtime.Input{Text: "Deploy."}); err != nil {
		t.Fatal(err)
	}
	spans := sr.Ended()

	agent := one(t, spans, observe.OperationInvokeAgent)
	if in := str(agent, observe.KeyInputMessages); !strings.Contains(in, "Deploy.") {
		t.Fatalf("agent input = %q", in)
	}
	if out := str(agent, observe.KeyOutputMessages); !strings.Contains(out, "Done.") {
		t.Fatalf("agent output = %q", out)
	}
	chats := byOperation(spans, observe.OperationChat)
	if len(chats) != 2 {
		t.Fatalf("%d chat spans", len(chats))
	}
	if in := str(chats[0], observe.KeyInputMessages); !strings.Contains(in, "Deploy.") {
		t.Fatalf("first step input = %q, want the prompt", in)
	}
	if out := str(chats[0], observe.KeyOutputMessages); !strings.Contains(out, `"tool_call"`) || !strings.Contains(out, `"deploy"`) {
		t.Fatalf("first step output = %q, want the deploy call", out)
	}
	if in := str(chats[1], observe.KeyInputMessages); !strings.Contains(in, "deployed") {
		t.Fatalf("second step input = %q, want the tool result", in)
	}
	tool := one(t, spans, observe.OperationExecuteTool)
	if str(tool, observe.KeyToolArguments) != "{}" || str(tool, observe.KeyToolResult) != "deployed" {
		t.Fatalf("tool args %q result %q", str(tool, observe.KeyToolArguments), str(tool, observe.KeyToolResult))
	}
}

// blockingAgent blocks until its turn context ends, as a turn does when the
// process that runs it stops.
type blockingAgent struct {
	started chan struct{}
	once    sync.Once
}

func (a *blockingAgent) PromptResult(ctx context.Context, _ string) (*kit.TurnResult, error) {
	a.once.Do(func() { close(a.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}
func (a *blockingAgent) InjectSteer(string) {}
func (a *blockingAgent) Close() error       { return nil }

type answerAgent struct{ response string }

func (a answerAgent) PromptResult(context.Context, string) (*kit.TurnResult, error) {
	return &kit.TurnResult{Response: a.response}, nil
}
func (answerAgent) ContinueResult(context.Context) (*kit.TurnResult, error) {
	return &kit.TurnResult{Response: "continued"}, nil
}
func (answerAgent) InjectSteer(string) {}
func (answerAgent) Close() error       { return nil }

// A turn that a second process recovers continues the trace of the turn the
// first process left, as its child. The processes share only the journal.
func TestRecoveryContinuesTheTraceAcrossProcesses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := runtime.NewMemoryJournal()

	srA, tpA := recorder()
	blocked := &blockingAgent{started: make(chan struct{})}
	runA := runtime.NewRunner(journal, func(context.Context, *runtime.Session) (runtime.Agent, error) { return blocked, nil },
		runtime.WithObserver(observe.New(tpA)))
	done := make(chan struct{})
	go func() {
		_, _ = runA.Start(ctx, "trace-3", runtime.Input{Text: "long job"})
		close(done)
	}()
	<-blocked.started
	if err := runA.Interrupt("trace-3"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt did not stop the turn")
	}
	first := one(t, srA.Ended(), observe.OperationInvokeAgent)
	if str(first, observe.KeyRunState) != string(runtime.RunInterrupted) {
		t.Fatalf("first turn state = %q", str(first, observe.KeyRunState))
	}

	srB, tpB := recorder()
	runB := runtime.NewRunner(journal, func(context.Context, *runtime.Session) (runtime.Agent, error) { return answerAgent{"done"}, nil },
		runtime.WithObserver(observe.New(tpB)))
	if _, err := runB.StartRetry(ctx, "trace-3", runtime.Input{Text: "long job"}); err != nil {
		t.Fatal(err)
	}
	second := one(t, srB.Ended(), observe.OperationInvokeAgent)
	if second.SpanContext().TraceID() != first.SpanContext().TraceID() {
		t.Fatal("the recovered turn started a new trace")
	}
	if second.Parent().SpanID() != first.SpanContext().SpanID() || !second.Parent().IsRemote() {
		t.Fatalf("recovered turn parent = %v, want the interrupted turn %v", second.Parent().SpanID(), first.SpanContext().SpanID())
	}
	if str(second, observe.KeyTurnKind) != string(runtime.TurnRecovery) {
		t.Fatalf("turn kind = %q", str(second, observe.KeyTurnKind))
	}
}

// A run that waits for a human ends its trace; the resume starts a new one
// that links back, and the answer reaches the hook with the waiting span.
func TestResumeLinksToTheTurnThatWaited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := runtime.NewMemoryJournal()

	srA, tpA := recorder()
	model := fakemodel.New(fakemodel.Call("request_approval", `{"action":"deploy"}`))
	runA := runtime.NewRunner(journal, scriptedKit(model, kit.WithExtraTools(deployTool())),
		runtime.WithObserver(observe.New(tpA, observe.WithCapture(observe.CaptureAll))))
	run, err := runA.Start(ctx, "trace-4", runtime.Input{Text: "Deploy."})
	if err != nil || run.State != runtime.RunWaiting {
		t.Fatalf("Start = %+v, %v; want waiting", run, err)
	}
	waited := one(t, srA.Ended(), observe.OperationInvokeAgent)
	var suspended bool
	for _, ev := range waited.Events() {
		if ev.Name == observe.EventSuspended {
			suspended = true
			for _, kv := range ev.Attributes {
				if kv.Key == observe.KeySuspendKind && kv.Value.AsString() != runtime.SuspendApproval {
					t.Fatalf("suspend kind = %q", kv.Value.AsString())
				}
			}
		}
	}
	if !suspended {
		t.Fatal("the waiting turn has no suspended event")
	}

	srB, tpB := recorder()
	var answers []observe.Answer
	hook := func(_ context.Context, a observe.Answer) { answers = append(answers, a) }
	model2 := fakemodel.New(fakemodel.Call("deploy", `{}`), fakemodel.Say("Deployed."))
	runB := runtime.NewRunner(journal, scriptedKit(model2, kit.WithExtraTools(deployTool())),
		runtime.WithObserver(observe.New(tpB, observe.WithAnswerHook(hook))))
	if run, err = runB.Resume(ctx, "trace-4", []runtime.InputResponse{runtime.Approve("ship it")}); err != nil || run.State != runtime.RunCompleted {
		t.Fatalf("Resume = %+v, %v", run, err)
	}
	resumed := one(t, srB.Ended(), observe.OperationInvokeAgent)
	if resumed.SpanContext().TraceID() == waited.SpanContext().TraceID() || resumed.Parent().IsValid() {
		t.Fatal("the resume continued the waiting trace; it must start a new one")
	}
	links := resumed.Links()
	if len(links) != 1 || links[0].SpanContext.SpanID() != waited.SpanContext().SpanID() {
		t.Fatalf("resume links = %+v, want the waiting turn", links)
	}
	if len(answers) != 1 || answers[0].Approved == nil || !*answers[0].Approved {
		t.Fatalf("answers = %+v", answers)
	}
	if answers[0].Waited.SpanID() != waited.SpanContext().SpanID() {
		t.Fatal("the answer does not name the turn that waited")
	}
	// The resuming tracer records no content, so the answer text stays out.
	if answers[0].Text != "" {
		t.Fatalf("answer text = %q with no capture policy", answers[0].Text)
	}
}

func TestInputTraceIsTheParent(t *testing.T) {
	t.Parallel()
	sr, tp := recorder()
	r := runtime.NewRunner(runtime.NewMemoryJournal(),
		func(context.Context, *runtime.Session) (runtime.Agent, error) { return answerAgent{"ok"}, nil },
		runtime.WithObserver(observe.New(tp)))
	const parent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	if _, err := r.Start(context.Background(), "trace-5", runtime.Input{Text: "hi", Trace: map[string]string{"traceparent": parent}}); err != nil {
		t.Fatal(err)
	}
	span := one(t, sr.Ended(), observe.OperationInvokeAgent)
	if span.SpanContext().TraceID().String() != "0af7651916cd43dd8448eb211c80319c" || span.Parent().SpanID().String() != "b7ad6b7169203331" {
		t.Fatalf("trace %s parent %s", span.SpanContext().TraceID(), span.Parent().SpanID())
	}
}

func TestNilProviderRecordsNothing(t *testing.T) {
	t.Parallel()
	r := runtime.NewRunner(runtime.NewMemoryJournal(),
		func(context.Context, *runtime.Session) (runtime.Agent, error) { return answerAgent{"ok"}, nil },
		runtime.WithObserver(observe.New(nil)))
	run, err := r.Start(context.Background(), "trace-6", runtime.Input{Text: "hi"})
	if err != nil || run.State != runtime.RunCompleted {
		t.Fatalf("Start = %+v, %v", run, err)
	}
	recs, err := r.Journal().Replay(context.Background(), "trace-6")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Kind == runtime.RecordTurn && len(rec.Payload) != 0 {
			t.Fatalf("a no-op tracer journalled %s", rec.Payload)
		}
	}
}

func TestFailedTurnIsAnError(t *testing.T) {
	t.Parallel()
	sr, tp := recorder()
	r := runtime.NewRunner(runtime.NewMemoryJournal(),
		func(context.Context, *runtime.Session) (runtime.Agent, error) { return nil, errFactory },
		runtime.WithObserver(observe.New(tp)))
	if _, err := r.Start(context.Background(), "trace-7", runtime.Input{Text: "hi"}); err == nil {
		t.Fatal("Start succeeded with a failing factory")
	}
	span := one(t, sr.Ended(), observe.OperationInvokeAgent)
	if span.Status().Code != codes.Error || str(span, observe.KeyErrorType) != "turn_failed" {
		t.Fatalf("status %v error.type %q", span.Status(), str(span, observe.KeyErrorType))
	}
}

type factoryError struct{}

func (factoryError) Error() string { return "no model" }

var errFactory error = factoryError{}
