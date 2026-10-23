//go:build integration

package bonnie

import (
	"context"
	"os"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// liveDecisionModel returns the decision model to test against, or "" when
// the machine has no credential. BONNIE_TEST_DECISION_MODEL overrides it.
func liveDecisionModel() string {
	if m := os.Getenv("BONNIE_TEST_DECISION_MODEL"); m != "" {
		return m
	}
	if os.Getenv("OPENROUTER_API_KEY") != "" {
		return "openrouter/perplexity/pplx-decider-flash-v1.2"
	}
	return ""
}

// The real Kit.Decide path: RunScope.Decide reaches a live decision model
// through the run's Kit, journals the answer, and a keyed repeat does not
// call the model again.
func TestLiveRunScopeDecide(t *testing.T) {
	model := liveDecisionModel()
	if model == "" {
		t.Skip("no decision model credential (set OPENROUTER_API_KEY or BONNIE_TEST_DECISION_MODEL)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var p float64
	cfg := resolve(
		WithDecisionModel(model),
		WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
		WithKit(completionKitOptions(fakemodel.New(fakemodel.Say("done")))...),
		WithKitSetup(func(ctx context.Context, _ *kit.Kit, scope RunScope) error {
			req := DecisionRequest{
				State:     kit.DecisionState{Text: "Customer: my card was charged twice for one order, please give my money back."},
				Questions: []kit.DecisionQuestion{kit.BooleanQuestion("refund", "Does the customer ask for a refund?")},
				Key:       "refund",
			}
			for range 2 {
				res, err := scope.Decide(ctx, req)
				if err != nil {
					return err
				}
				a, err := res.Boolean("refund")
				if err != nil {
					return err
				}
				p = a.Probability
			}
			return nil
		}),
	)
	if err := cfg.checkDecision(); err != nil {
		t.Fatal(err)
	}
	factory, err := cfg.agentFactory(ctx, "", cfg.kitOptions("", ""))
	if err != nil {
		t.Fatal(err)
	}
	j := runtime.NewMemoryJournal()
	if _, err := runtime.NewRunner(j, factory).Start(ctx, "live", runtime.Input{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if p < 0.5 {
		t.Fatalf("P(refund) = %v, want a clear yes", p)
	}
	if recs := journalDecisions(t, j, "live"); len(recs) != 1 || recs[0].Error != "" {
		t.Fatalf("records = %+v, want one successful call", recs)
	}
}
