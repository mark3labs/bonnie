package observe_test

import (
	"context"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
	"go.opentelemetry.io/otel/codes"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/observe"
	"github.com/mark3labs/bonnie/runtime"
)

func decideOnce(p float64, err error) runtime.Decider {
	return func(_ context.Context, req kit.DecisionRequest) (*kit.DecisionResult, error) {
		if err != nil {
			return nil, err
		}
		res := &kit.DecisionResult{Usage: kit.DecisionUsage{InputTokens: 9}}
		for _, q := range req.Questions {
			res.Answers = append(res.Answers, kit.DecisionAnswer{ID: q.ID, Type: q.Type, Probability: p})
		}
		return res, nil
	}
}

// A decision call is a decide span: under the tool that made it, or under
// the agent span when setup code made it. A key that answers from the
// journal makes no span, and the answers are output content.
func TestDecisionSpans(t *testing.T) {
	t.Parallel()
	for _, capture := range []bool{false, true} {
		sr, tp := recorder()
		req := kit.DecisionRequest{
			Model:     "ollama/pplx-decider",
			State:     kit.DecisionState{Text: "x"},
			Questions: []kit.DecisionQuestion{kit.BooleanQuestion("risky", "Risky?")},
		}
		model := fakemodel.New(fakemodel.Call("gate", `{}`), fakemodel.Say("Done."))
		factory := runtime.KitAgentWithSetup(false, func(ctx context.Context, k *kit.Kit, s *runtime.Session) error {
			if _, err := s.Decide(ctx, decideOnce(0, kit.ErrDecisionRefused), runtime.DecisionCall{Request: req}); err == nil {
				t.Error("refusal was not returned")
			}
			k.AddTools(kit.NewTool("gate", "Gate.", func(ctx context.Context, _ struct{}) (kit.ToolOutput, error) {
				for range 2 {
					if _, err := s.Decide(ctx, decideOnce(0.3, nil), runtime.DecisionCall{Request: req, Key: "k"}); err != nil {
						return kit.ToolOutput{}, err
					}
				}
				return kit.TextResult("ok"), nil
			}))
			return nil
		}, hermetic(), model.Option())
		opts := []observe.Option{}
		if capture {
			opts = append(opts, observe.WithCapture(observe.CaptureAll))
		}
		r := runtime.NewRunner(runtime.NewMemoryJournal(), factory, runtime.WithObserver(observe.New(tp, opts...)))
		if _, err := r.Start(context.Background(), "d", runtime.Input{Text: "go"}); err != nil {
			t.Fatal(err)
		}
		spans := sr.Ended()
		agent := one(t, spans, observe.OperationInvokeAgent)
		tool := one(t, spans, observe.OperationExecuteTool)
		decides := byOperation(spans, observe.OperationDecide)
		if len(decides) != 2 {
			t.Fatalf("%d decide spans, want 2: the keyed repeat is not a call", len(decides))
		}
		var failed, gated int
		for _, s := range decides {
			if s.Name() != "decide pplx-decider" || str(s, observe.KeyProviderName) != "ollama" {
				t.Fatalf("span %q provider %q", s.Name(), str(s, observe.KeyProviderName))
			}
			switch s.Parent().SpanID() {
			case agent.SpanContext().SpanID():
				failed++
				if s.Status().Code != codes.Error || str(s, observe.KeyErrorType) != "refused" {
					t.Fatalf("failed decision status %v error %q", s.Status(), str(s, observe.KeyErrorType))
				}
			case tool.SpanContext().SpanID():
				gated++
				if str(s, observe.KeyDecisionKey) != "k" || str(s, observe.KeyDecisionSrc) != runtime.DecisionSourceHost {
					t.Fatalf("tool decision attrs %v", s.Attributes())
				}
				if _, ok := attr(s, observe.KeyDecisionAnswers); ok != capture {
					t.Fatalf("answers recorded = %v with capture %v", ok, capture)
				}
			default:
				t.Fatalf("decide span parent %v", s.Parent().SpanID())
			}
		}
		if failed != 1 || gated != 1 {
			t.Fatalf("failed %d gated %d", failed, gated)
		}
	}
}
