package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie/agent"
	"github.com/mark3labs/bonnie/internal/evalui"
	"github.com/mark3labs/bonnie/internal/treetest"
)

// evalTreeMain is a tree whose agent echoes the input, so its evals need no
// provider.
const evalTreeMain = `package main

import (
	"context"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/runtime"
)

type echoAgent struct{ session *runtime.Session }

func (a *echoAgent) PromptResult(_ context.Context, msg string) (*kit.TurnResult, error) {
	if _, err := a.session.AppendMessage(kit.NewLLMUserMessage(msg)); err != nil {
		return nil, err
	}
	reply := "echo: " + msg
	if _, err := a.session.AppendMessage(kit.LLMMessage{Role: kit.LLMMessageRole("assistant"),
		Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: reply}}}); err != nil {
		return nil, err
	}
	return &kit.TurnResult{Response: reply}, nil
}

func (a *echoAgent) InjectSteer(string) {}
func (a *echoAgent) Close() error       { return nil }

func main() { newAgent().Serve() }

func newAgent() *bonnie.Agent {
	return bonnie.New(bonnie.WithAgentFactory(func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
		return &echoAgent{session: s}, nil
	}))
}
`

// evalTreeTest has one eval that passes its gates and misses one soft
// threshold.
const evalTreeTest = `//go:build eval

package main

import (
	"context"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/eval"
)

func judge(_ context.Context, req kit.DecisionRequest) (*kit.DecisionResult, error) {
	return &kit.DecisionResult{Answers: []kit.DecisionAnswer{{ID: "echoes", Type: kit.DecisionBoolean, Probability: 0.95}}}, nil
}

func TestEcho(t *testing.T) {
	t.Parallel()
	e := eval.New(t, newAgent(), eval.WithDecider(judge))
	turn := e.Send("hello").Succeeded()
	e.Check(turn.Reply(), eval.Contains("echo: hello"))
	e.Judge(eval.Bool("echoes", "Does the response repeat the input?")).AtLeast(0.9).Gate()
	e.Score("brevity", 0.4).AtLeast(0.5)
}

// TestCases and TestOther both have a subtest "b". A rerun of TestCases/b
// must not run TestOther/b.
func TestCases(t *testing.T) {
	for _, name := range []string{"a", "b"} {
		t.Run(name, func(t *testing.T) {
			eval.New(t, newAgent()).Send(name).Succeeded()
		})
	}
}

func TestOther(t *testing.T) {
	t.Run("b", func(t *testing.T) { eval.New(t, newAgent()).Send("b").Succeeded() })
}
`

// bonnie eval runs the evals of a real tree with go test, reports the
// scores, keeps the run and each eval's journal in .bonnie/evals, and fails
// in strict mode on a soft miss. A tree without evals is an error.
func TestRunEvalOnTree(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "eval-agent")
	if _, err := agent.Scaffold(root, agent.InitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(evalTreeMain), 0o644); err != nil {
		t.Fatal(err)
	}
	treetest.LinkToCheckout(t, root)
	opts := evalOpts{count: 1, timeout: defaultEvalTimeout, keep: 2, env: treetest.BuildEnv()}

	var out bytes.Buffer
	if err := runEval(t.Context(), root, opts, &out); err == nil || !strings.Contains(err.Error(), "no evals ran") {
		t.Fatalf("a tree with no evals: %v\n%s", err, out.String())
	}

	if err := os.WriteFile(filepath.Join(root, "echo_eval_test.go"), []byte(evalTreeTest), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runEval(t.Context(), root, opts, &out); err != nil {
		t.Fatalf("runEval: %v\n%s", err, out.String())
	}
	for _, want := range []string{"PASS  TestEcho", "judge    echoes", "passed", "score    brevity", "scored", "PASS  TestCases/b", "evals: 4 passed, 0 failed", "saved: "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, out.String())
		}
	}

	// The run is saved with the eval's journal.
	sess, err := loadEvalRun(root, "")
	if err != nil {
		t.Fatal(err)
	}
	evals := sess.Snapshot().Evals
	if len(evals) != 4 {
		t.Fatalf("saved evals = %d, want 4", len(evals))
	}
	for _, r := range evals {
		if r.Journal == "" || r.Turns != 1 {
			t.Fatalf("saved eval %s has journal %q and %d turns", r.Name, r.Journal, r.Turns)
		}
		if _, err := os.Stat(filepath.Join(r.Journal, "journal.db")); err != nil {
			t.Fatalf("the journal of %s was not kept: %v", r.Name, err)
		}
	}

	out.Reset()
	strict := opts
	strict.strict = true
	if err := runEval(t.Context(), root, strict, &out); err == nil || !strings.Contains(err.Error(), "1 of 4 evals failed") {
		t.Fatalf("strict run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), `score "brevity" is 0.400`) {
		t.Errorf("strict output does not show the miss:\n%s", out.String())
	}

	// A rerun runs exactly the evals it names, and keeps the other results
	// of the run before it, in their place, as a new saved run.
	last, err := loadEvalRun(root, "")
	if err != nil {
		t.Fatal(err)
	}
	var pick []evalui.Result
	for _, r := range last.Snapshot().Evals {
		if r.Name == "TestEcho" || r.Name == "TestCases/b" {
			pick = append(pick, r)
		}
	}
	if len(pick) != 2 {
		t.Fatalf("evals of the last run = %+v", last.Snapshot().Evals)
	}
	out.Reset()
	runner, err := newEvalRunner(t.Context(), root, opts)
	if err != nil {
		t.Fatal(err)
	}
	started, err := runner.rerun(last, pick)
	if err != nil {
		t.Fatal(err)
	}
	// The rerun evals run from the start, before go test reports them.
	for _, r := range started.Snapshot().Evals {
		if (r.Name == "TestEcho" || r.Name == "TestCases/b") && r.Status != evalui.StatusRunning && r.Status != evalui.StatusPass {
			t.Errorf("rerun eval %s starts as %s", r.Name, r.Status)
		}
	}
	if err := runner.finish(&out, started, ""); err != nil {
		t.Fatalf("rerun: %v\n%s", err, out.String())
	}
	rerun, err := loadEvalRun(root, "")
	if err != nil {
		t.Fatal(err)
	}
	snap := rerun.Snapshot()
	if snap.ID == last.Snapshot().ID || snap.From != last.Snapshot().ID {
		t.Fatalf("rerun %q from %q, want a new run from %q", snap.ID, snap.From, last.Snapshot().ID)
	}
	var before, after []string
	for _, r := range last.Snapshot().Evals {
		before = append(before, r.Name)
	}
	ran := map[string]bool{}
	for _, r := range snap.Evals {
		after = append(after, r.Name)
		if r.Carried() {
			if r.Run != last.Snapshot().ID {
				t.Errorf("%s carried from %q, want %q", r.Name, r.Run, last.Snapshot().ID)
			}
		} else {
			ran[r.Name] = true
		}
	}
	if !slices.Equal(before, after) {
		t.Fatalf("the rerun shows %q, want the whole suite %q", after, before)
	}
	if len(ran) != 2 || !ran["TestEcho"] || !ran["TestCases/b"] {
		t.Fatalf("the rerun ran %v, want TestEcho and TestCases/b only", ran)
	}
	if !strings.Contains(out.String(), "PASS  TestOther/b  ") || !strings.Contains(out.String(), "(from "+last.Snapshot().ID+")") {
		t.Errorf("the plain table does not mark the carried results:\n%s", out.String())
	}

	// keep is 2: of the four runs, the two oldest are gone.
	ids, err := evalui.List(evalStore(root))
	if err != nil || len(ids) != 2 {
		t.Fatalf("saved runs = %v, %v", ids, err)
	}
	out.Reset()
	if err := listEvalRuns(&out, evalStore(root)); err != nil || !strings.Contains(out.String(), ids[0]) {
		t.Fatalf("list: %v\n%s", err, out.String())
	}
}

// Each group of a package and a parent test gets one anchored pattern, so a
// rerun never matches the cross product of two parents' subtests.
func TestRerunTargets(t *testing.T) {
	t.Parallel()
	got := rerunTargets([]evalui.Result{
		{Package: "a", Name: "TestEcho"},
		{Package: "a", Name: "TestCases/b"},
		{Package: "a", Name: "TestCases/c.d"},
		{Package: "a", Name: "TestOther/b"},
		{Package: "a", Name: "TestDeep/x/y"},
		{Package: "a", Name: "TestGreeting"},
		{Package: "z", Name: "TestEcho"},
	})
	want := []evalTarget{
		{pkg: "a", run: "^(TestEcho|TestGreeting)$"},
		{pkg: "a", run: `^TestCases$/^(b|c\.d)$`},
		{pkg: "a", run: "^TestOther$/^(b)$"},
		{pkg: "a", run: "^TestDeep$/^x$/^(y)$"},
		{pkg: "z", run: "^(TestEcho)$"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("targets = %+v\nwant      %+v", got, want)
	}
}
