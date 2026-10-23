package bonnie

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// countingDecider answers each question with P(true) = 0.8, the first
// option, or the top level, and records the requests it got.
type countingDecider struct {
	calls  atomic.Int32
	models []string
}

func (d *countingDecider) decide(_ context.Context, req kit.DecisionRequest) (*kit.DecisionResult, error) {
	d.calls.Add(1)
	d.models = append(d.models, req.Model)
	res := &kit.DecisionResult{Model: "scripted"}
	for _, q := range req.Questions {
		a := kit.DecisionAnswer{ID: q.ID, Type: q.Type}
		switch q.Type {
		case kit.DecisionBoolean:
			a.Probability = 0.8
		case kit.DecisionChoice:
			a.Choice = q.Options[0].Name
		case kit.DecisionScore:
			a.Score, a.Level = float64(len(q.Levels)-1), len(q.Levels)-1
		}
		res.Answers = append(res.Answers, a)
	}
	return res, nil
}

func gateRequest(key string) DecisionRequest {
	return DecisionRequest{
		State:     kit.DecisionState{Text: "rm -rf /tmp/cache"},
		Questions: []kit.DecisionQuestion{kit.BooleanQuestion("risky", "Is the command risky?")},
		Key:       key,
	}
}

func journalDecisions(t *testing.T, j runtime.Journal, runID string) []runtime.DecisionRecord {
	t.Helper()
	recs, err := j.Replay(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []runtime.DecisionRecord
	for _, rec := range recs {
		if rec.Kind == runtime.RecordDecision {
			d, err := runtime.DecodeDecisionRecord(rec)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
	}
	return out
}

// RunScope.Decide reaches host code in a run, uses the default model, and
// journals the call. Agent.Decide works outside a run.
func TestRunScopeDecide(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := &countingDecider{}
	var answer float64
	cfg := resolve(
		WithDecisionModel("ollama/pplx-decider"),
		WithDecider(d.decide),
		WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
		WithKit(completionKitOptions(fakemodel.New(fakemodel.Say("done")))...),
		WithKitSetup(func(ctx context.Context, _ *kit.Kit, scope RunScope) error {
			if scope.Decide == nil {
				return errors.New("no Decide in scope")
			}
			res, err := scope.Decide(ctx, gateRequest("setup"))
			if err != nil {
				return err
			}
			a, err := res.Boolean("risky")
			answer = a.Probability
			return err
		}),
	)
	factory, err := cfg.agentFactory(ctx, "", cfg.kitOptions("", ""))
	if err != nil {
		t.Fatal(err)
	}
	j := runtime.NewMemoryJournal()
	if _, err := runtime.NewRunner(j, factory).Start(ctx, "gate", runtime.Input{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if answer != 0.8 {
		t.Fatalf("answer = %v", answer)
	}
	recs := journalDecisions(t, j, "gate")
	if len(recs) != 1 || recs[0].Key != "setup" || recs[0].Source != runtime.DecisionSourceHost || recs[0].Model != "ollama/pplx-decider" {
		t.Fatalf("records = %+v", recs)
	}

	agent := &Agent{cfg: cfg}
	res, err := agent.Decide(ctx, DecisionRequest{Model: "ollama/other", State: kit.DecisionState{Text: "x"},
		Questions: []kit.DecisionQuestion{kit.BooleanQuestion("q", "?")}})
	if err != nil || len(res.Answers) != 1 {
		t.Fatalf("Agent.Decide = %+v, %v", res, err)
	}
	if got := d.models; len(got) != 2 || got[1] != "ollama/other" {
		t.Fatalf("models = %v: an explicit model must win over the default", got)
	}
}

// Without a decision model, the scope has no Decide and Agent.Decide says so.
func TestDecideNotConfigured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := resolve(
		WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
		WithKit(completionKitOptions(fakemodel.New(fakemodel.Say("done")))...),
		WithKitSetup(func(_ context.Context, _ *kit.Kit, scope RunScope) error {
			if scope.Decide != nil {
				return errors.New("Decide set without a decision model")
			}
			return nil
		}),
	)
	factory, err := cfg.agentFactory(ctx, "", cfg.kitOptions("", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.NewRunner(runtime.NewMemoryJournal(), factory).Start(ctx, "plain", runtime.Input{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Agent{cfg: cfg}).Decide(ctx, gateRequest("")); !errors.Is(err, runtime.ErrNoDecider) {
		t.Fatalf("error = %v", err)
	}
}

// A decision model is checked before a run with Kit's own resolution, so a
// provider that takes any model name (local Ollama) passes, and a chat model
// or an unknown one does not. Nothing is sent.
func TestDecisionConfigurationChecks(t *testing.T) {
	t.Parallel()
	var callable string
	for _, m := range kit.GetDecisionModels() {
		if m.Wire != "" {
			callable = m.Model
			break
		}
	}
	if callable == "" {
		t.Fatal("the embedded model database lists no callable decision model")
	}
	d := &countingDecider{}
	for _, tc := range []struct {
		name string
		opts []Option
		want error
		text string
	}{
		{name: "catalog model", opts: []Option{WithDecisionModel(callable)}},
		{name: "unknown model", opts: []Option{WithDecisionModel("nope/nothing")}, want: kit.ErrNotDecisionModel},
		{name: "chat model", opts: []Option{WithDecisionModel("anthropic/claude-sonnet-4-5")}, want: kit.ErrNotDecisionModel},
		{name: "no provider", opts: []Option{WithDecisionModel("nothing")}, text: "WithDecisionModel"},
		{name: "any local Ollama model", opts: []Option{WithDecisionModel("ollama/my-local-decider")}},
		{name: "custom decider skips the catalog", opts: []Option{WithDecisionModel("custom/model"), WithDecider(d.decide)}},
		{name: "tool without a model", opts: []Option{WithDecisionTool()}, text: "WithDecisionTool needs"},
		{name: "tool with a decider", opts: []Option{WithDecisionTool(), WithDecider(d.decide)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := resolve(tc.opts...).checkDecision()
			switch {
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("error = %v, want %v", err, tc.want)
			case tc.text != "" && (err == nil || !strings.Contains(err.Error(), tc.text)):
				t.Fatalf("error = %v, want %q", err, tc.text)
			case tc.want == nil && tc.text == "" && err != nil:
				t.Fatalf("error = %v", err)
			}
		})
	}

	factory := WithAgentFactory(func(context.Context, *runtime.Session) (runtime.Agent, error) { return nil, nil })
	if got := resolve(WithDecisionTool(), factory).agentConflicts(); got != "WithDecisionTool" {
		t.Fatalf("conflict = %q", got)
	}
	if got := resolve(WithDecisionModel(callable), factory).agentConflicts(); got != "" {
		t.Fatalf("conflict = %q: Agent.Decide works beside a custom agent", got)
	}
}

func decideToolConfig(t *testing.T, d *countingDecider, model *fakemodel.Model) *config {
	t.Helper()
	return resolve(
		WithDecisionModel("ollama/pplx-decider"),
		WithDecider(d.decide),
		WithDecisionTool(),
		WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
		WithKit(completionKitOptions(model)...),
	)
}

const decideToolInput = `{"state":"Card charged twice","questions":[` +
	`{"id":"refund","type":"boolean","instructions":"Does the customer want a refund?"},` +
	`{"id":"team","type":"choice","instructions":"Which team?","options":[{"name":"billing"},{"name":"tech"}]},` +
	`{"id":"urgency","type":"score","instructions":"How urgent?","levels":["low","high"]}]}`

// toolResults returns the text of each tool result in the run.
func toolResults(t *testing.T, j runtime.Journal, runID string) []string {
	t.Helper()
	s, err := runtime.Restore(context.Background(), runID, j)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, msg := range s.GetMessages() {
		for _, part := range msg.Content {
			if p, ok := part.(kit.LLMToolResultPart); ok {
				switch o := p.Output.(type) {
				case kit.LLMToolResultOutputContentText:
					out = append(out, o.Text)
				case kit.LLMToolResultOutputContentError:
					out = append(out, "error: "+o.Error.Error())
				}
			}
		}
	}
	return out
}

// The model calls decide with all three question types. The answers come
// back as JSON and the call is journalled under the tool call's ID.
func TestDecideTool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := &countingDecider{}
	model := fakemodel.New(fakemodel.Call(DecideToolName, decideToolInput), fakemodel.Say("routed"))
	cfg := decideToolConfig(t, d, model)
	factory, err := cfg.agentFactory(ctx, "", cfg.kitOptions("", ""))
	if err != nil {
		t.Fatal(err)
	}
	j := runtime.NewMemoryJournal()
	run, err := runtime.NewRunner(j, factory).Start(ctx, "triage", runtime.Input{Text: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Response != "routed" || !model.Requests()[0].HasTool(DecideToolName) {
		t.Fatalf("run = %+v", run)
	}
	results := toolResults(t, j, "triage")
	if len(results) != 1 {
		t.Fatalf("results = %q", results)
	}
	var out struct {
		Answers []kit.DecisionAnswer `json:"answers"`
	}
	if err := json.Unmarshal([]byte(results[0]), &out); err != nil {
		t.Fatalf("result %q: %v", results[0], err)
	}
	if len(out.Answers) != 3 || out.Answers[0].Probability != 0.8 || out.Answers[1].Choice != "billing" || out.Answers[2].Level != 1 {
		t.Fatalf("answers = %+v", out.Answers)
	}
	recs := journalDecisions(t, j, "triage")
	if len(recs) != 1 || recs[0].Source != runtime.DecisionSourceTool || !strings.HasPrefix(recs[0].Key, "tool:") ||
		recs[0].Key != "tool:"+recs[0].ToolCallID {
		t.Fatalf("records = %+v", recs)
	}
}

// Bad input is a tool error the model can correct; nothing is sent.
func TestDecideToolRejectsBadInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := &countingDecider{}
	model := fakemodel.New(
		fakemodel.Call(DecideToolName, `{"state":"x","questions":[{"id":"a","type":"maybe","instructions":"?"}]}`),
		fakemodel.Call(DecideToolName, `{"state":" ","questions":[{"id":"a","type":"boolean","instructions":"?"}]}`),
		fakemodel.Say("gave up"),
	)
	cfg := decideToolConfig(t, d, model)
	factory, err := cfg.agentFactory(ctx, "", cfg.kitOptions("", ""))
	if err != nil {
		t.Fatal(err)
	}
	j := runtime.NewMemoryJournal()
	if _, err := runtime.NewRunner(j, factory).Start(ctx, "bad", runtime.Input{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	results := toolResults(t, j, "bad")
	if len(results) != 2 || !strings.Contains(results[0], `type "maybe"`) || !strings.Contains(results[1], "state is empty") {
		t.Fatalf("results = %q", results)
	}
	if d.calls.Load() != 0 || len(journalDecisions(t, j, "bad")) != 0 {
		t.Fatal("invalid input reached the decision model")
	}
}

// A decide call interrupted after its answer was journalled, but before its
// result was, is replayed in a new process from the journal: the decision
// model is not asked again.
func TestDecideToolRecoversFromJournal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	const callID = "call-1"

	// Process one: the tool started and its decision was journalled, then
	// the process stopped.
	first, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := json.Marshal(map[string]any{"id": callID, "name": DecideToolName, "args": decideToolInput, "safe": true})
	if _, err := first.Append(ctx, runtime.Record{RunID: "r", Kind: runtime.RecordToolIntent, Payload: intent}); err != nil {
		t.Fatal(err)
	}
	var in decideInput
	if err := json.Unmarshal([]byte(decideToolInput), &in); err != nil {
		t.Fatal(err)
	}
	req, err := in.request()
	if err != nil {
		t.Fatal(err)
	}
	firstDecider := &countingDecider{}
	kreq := kit.DecisionRequest{Model: "ollama/pplx-decider", State: req.State, Questions: req.Questions}
	if _, err := runtime.NewSession("r", first).Decide(ctx, firstDecider.decide, runtime.DecisionCall{
		Request: kreq, Key: "tool:" + callID, Source: runtime.DecisionSourceTool, ToolCallID: callID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Process two shares only the journal.
	second, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	d := &countingDecider{}
	cfg := decideToolConfig(t, d, fakemodel.New(fakemodel.Say("done")))
	factory, err := cfg.agentFactory(ctx, "", cfg.kitOptions("", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.NewRunner(second, factory).Start(ctx, "r", runtime.Input{Text: "continue"}); err != nil {
		t.Fatal(err)
	}
	if d.calls.Load() != 0 {
		t.Fatal("recovery asked the decision model again")
	}
	results := toolResults(t, second, "r")
	if len(results) != 1 || !strings.Contains(results[0], `"choice":"billing"`) {
		t.Fatalf("results = %q", results)
	}
	if got := journalDecisions(t, second, "r"); len(got) != 1 {
		t.Fatalf("records = %d, want the one from process one", len(got))
	}
}
