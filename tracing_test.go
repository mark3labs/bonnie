package bonnie

import (
	"context"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/mark3labs/bonnie/runtime"
)

type tracedAgent struct{}

func (tracedAgent) PromptResult(context.Context, string) (*kit.TurnResult, error) {
	return &kit.TurnResult{Response: "ok"}, nil
}
func (tracedAgent) InjectSteer(string) {}
func (tracedAgent) Close() error       { return nil }

// WithTracing must reach the Runner that serving and scoped execution
// build, and the agent's name must name its spans.
func TestWithTracingReachesTheRunner(t *testing.T) {
	t.Parallel()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	c := resolve(WithName("ops"), WithTracing(tp))
	r := runtime.NewRunner(runtime.NewMemoryJournal(),
		func(context.Context, *runtime.Session) (runtime.Agent, error) { return tracedAgent{}, nil },
		c.runnerOptions()...)
	if _, err := r.Start(context.Background(), "traced", runtime.Input{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	spans := sr.Ended()
	if len(spans) != 1 || spans[0].Name() != "invoke_agent ops" {
		names := make([]string, len(spans))
		for i, s := range spans {
			names[i] = s.Name()
		}
		t.Fatalf("spans = %v, want one invoke_agent ops", names)
	}
}

func TestWithTracingNilDisables(t *testing.T) {
	t.Parallel()
	if c := resolve(WithTracing(nil)); len(c.runnerOptions()) != len(resolve().runnerOptions()) {
		t.Fatal("WithTracing(nil) added an observer")
	}
}
