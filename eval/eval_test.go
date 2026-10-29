package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// recorder is a testing.TB that keeps the failures and logs of the code
// under test, so a test can examine a gate that must fail.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
	logs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *recorder) errors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errs...)
}

// testAgent builds a hermetic agent whose model answers from model.
func testAgent(t *testing.T, model *fakemodel.Model, opts ...bonnie.Option) *bonnie.Agent {
	t.Helper()
	tool := kit.NewTool("lookup_policy", "Look up a policy.", func(_ context.Context, in struct {
		Topic string `json:"topic"`
	}) (kit.ToolOutput, error) {
		if in.Topic == "broken" {
			return kit.ErrorResult("policy store unavailable"), nil
		}
		return kit.TextResult("Refunds are allowed for 30 days."), nil
	})
	base := []bonnie.Option{
		bonnie.WithSystemPrompt("You are a support agent."),
		bonnie.WithContextFiles(""),
		bonnie.WithSkills(""),
		bonnie.WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
		bonnie.WithTools(tool),
		bonnie.WithKit(model.Option(), func(o *kit.Options) {
			o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.Quiet = true, true, true, true, true, true
		}),
	}
	return bonnie.New(append(base, opts...)...)
}

// scripted returns a decider that answers each question from answers by ID,
// and keeps each request.
func scripted(answers ...kit.DecisionAnswer) (runtime.Decider, *[]kit.DecisionRequest) {
	var mu sync.Mutex
	var reqs []kit.DecisionRequest
	return func(_ context.Context, req kit.DecisionRequest) (*kit.DecisionResult, error) {
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		res := &kit.DecisionResult{Model: "scripted-judge"}
		for _, q := range req.Questions {
			for _, a := range answers {
				if a.ID == q.ID {
					res.Answers = append(res.Answers, a)
				}
			}
		}
		return res, nil
	}, &reqs
}

// The run checks read the journal: tool calls, their results and the
// steps come from the records the turn wrote.
func TestRunChecks(t *testing.T) {
	t.Parallel()
	model := fakemodel.New(
		fakemodel.Call("lookup_policy", `{"topic":"refund"}`),
		fakemodel.Say("Refunds are allowed for 30 days."),
	)
	e := New(t, testAgent(t, model))
	turn := e.Send("Can I get a refund after 40 days?")
	turn.Succeeded().
		CalledTool("lookup_policy").
		CalledToolWith("lookup_policy", Contains(`"refund"`)).
		NotCalledTool("issue_refund").
		NoFailedTools().
		MaxSteps(2)
	e.Check(turn.Reply(), Contains("30 days"))

	calls := turn.ToolCalls()
	if len(calls) != 1 || !calls[0].Done || calls[0].Output != "Refunds are allowed for 30 days." {
		t.Fatalf("calls = %+v", calls)
	}
	if turn.Steps() != 2 {
		t.Fatalf("steps = %d, want 2", turn.Steps())
	}
	if got := e.Transcript(); !strings.Contains(got, "user: Can I get a refund") || !strings.Contains(got, "[call lookup_policy") {
		t.Fatalf("transcript = %q", got)
	}
}

// Each failed gate is reported, and the eval continues after it.
func TestRunChecksFail(t *testing.T) {
	t.Parallel()
	model := fakemodel.New(
		fakemodel.Call("lookup_policy", `{"topic":"broken"}`),
		fakemodel.Say("I cannot tell."),
	)
	r := &recorder{TB: t}
	e := New(r, testAgent(t, model))
	turn := e.Send("Can I get a refund?")
	turn.Suspended().
		CalledTool("missing").
		CalledToolWith("lookup_policy", Contains("refund")).
		NotCalledTool("lookup_policy").
		NoFailedTools().
		MaxSteps(1)
	e.Check(turn.Reply(), Contains("30 days"))
	want := []string{
		"is not suspended",
		`tool "missing" was not called; calls: lookup_policy`,
		`no input of tool "lookup_policy" matches`,
		`tool "lookup_policy" was called 1 times`,
		`tool "lookup_policy" failed: policy store unavailable`,
		"turn took 2 steps, more than 1",
		`check failed: "I cannot tell." does not contain "30 days"`,
	}
	errs := r.errors()
	if len(errs) != len(want) {
		t.Fatalf("errors = %q", errs)
	}
	for i, w := range want {
		if !strings.Contains(errs[i], w) {
			t.Errorf("error %d = %q, want %q", i, errs[i], w)
		}
	}
}

// A judgment grades the latest turn by default, and the scores follow the
// question types. The expected option of a choice question never reaches
// the model.
func TestJudgeScores(t *testing.T) {
	t.Parallel()
	decide, reqs := scripted(
		kit.DecisionAnswer{ID: "polite", Type: kit.DecisionBoolean, Probability: 0.9},
		kit.DecisionAnswer{ID: "clarity", Type: kit.DecisionScore, Score: 1},
		kit.DecisionAnswer{ID: "outcome", Type: kit.DecisionChoice, Choice: "declined", Distribution: []kit.DecisionProbability{
			{Index: 0, Name: "answered", Probability: 0.25}, {Index: 1, Name: "declined", Probability: 0.75},
		}},
	)
	r := &recorder{TB: t}
	e := New(r, testAgent(t, fakemodel.New(fakemodel.Say("No refund after 30 days."))), WithDecider(decide), WithStrict(false))
	e.Send("Can I get a refund after 40 days?")
	scores := e.JudgeAll([]Question{
		Bool("polite", "Is the reply polite?"),
		Rubric("clarity", "Grade clarity.", "unclear", "mostly clear", "clear"),
		Choice("outcome", "Classify the reply.", "answered",
			kit.DecisionOption{Name: "answered"}, kit.DecisionOption{Name: "declined"}),
	}, Reference("Refunds are allowed for 30 days."))
	for id, want := range map[string]float64{"polite": 0.9, "clarity": 0.5, "outcome": 0.25} {
		if got := scores[id].Value(); got != want {
			t.Errorf("%s = %v, want %v", id, got, want)
		}
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %d, want 1 for all questions", len(*reqs))
	}
	state, err := json.Marshal((*reqs)[0].State.JSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"input":"Can I get a refund after 40 days?"`, `"response":"No refund after 30 days."`, `"reference":"Refunds are allowed for 30 days."`} {
		if !strings.Contains(string(state), want) {
			t.Errorf("state %s does not contain %s", state, want)
		}
	}

	// A soft miss is recorded, a gate miss fails.
	scores["polite"].AtLeast(0.8)
	scores["clarity"].AtLeast(0.75)
	if errs := r.errors(); len(errs) != 0 {
		t.Fatalf("a soft miss failed the eval: %q", errs)
	}
	scores["outcome"].AtLeast(0.5).Gate()
	if errs := r.errors(); len(errs) != 1 || !strings.Contains(errs[0], `score "outcome" is 0.250, want at least 0.500`) {
		t.Fatalf("errors = %q", errs)
	}
	for id, status := range map[string]string{"polite": StatusPassed, "clarity": StatusScored, "outcome": StatusFailed} {
		if got := scores[id].report("t", false).Status; got != status {
			t.Errorf("%s status = %s, want %s", id, got, status)
		}
	}
}

// In strict mode a soft miss fails the eval.
func TestStrictFailsSoftMiss(t *testing.T) {
	t.Parallel()
	decide, _ := scripted(kit.DecisionAnswer{ID: "polite", Type: kit.DecisionBoolean, Probability: 0.4})
	r := &recorder{TB: t}
	e := New(r, testAgent(t, fakemodel.New(fakemodel.Say("No."))), WithDecider(decide), WithStrict(true))
	e.Send("Refund?")
	e.Judge(Bool("polite", "Is the reply polite?")).AtLeast(0.5)
	if errs := r.errors(); len(errs) != 1 || !strings.Contains(errs[0], `"polite" is 0.400`) {
		t.Fatalf("errors = %q", errs)
	}
}

// A judgment that cannot ask a model fails, also with no threshold. An eval
// never skips a judgment silently.
func TestJudgeWithoutModelFails(t *testing.T) {
	t.Parallel()
	r := &recorder{TB: t}
	e := New(r, testAgent(t, fakemodel.New(fakemodel.Say("No."))), WithJudge(""), WithLLMJudge(""))
	e.Send("Refund?")
	s := e.Judge(Bool("polite", "Is the reply polite?"))
	l := e.JudgeLLM("The reply is polite.")
	if !errors.Is(s.Err(), ErrNoJudge) || !errors.Is(l.Err(), ErrNoJudge) {
		t.Fatalf("errors = %v, %v", s.Err(), l.Err())
	}
	if errs := r.errors(); len(errs) != 2 {
		t.Fatalf("errors = %q", errs)
	}
	if s.report("t", false).Status != StatusFailed {
		t.Fatal("a failed judgment is not reported as failed")
	}
}

// A judge error and a refused question fail the score that depends on them.
func TestJudgeErrors(t *testing.T) {
	t.Parallel()
	r := &recorder{TB: t}
	refuse := func(_ context.Context, req kit.DecisionRequest) (*kit.DecisionResult, error) {
		return &kit.DecisionResult{Answers: []kit.DecisionAnswer{{ID: "polite", Type: kit.DecisionBoolean, Refused: true}}}, nil
	}
	e := New(r, testAgent(t, fakemodel.New()), WithDecider(refuse))
	if s := e.Judge(Bool("polite", "?")); s.Err() == nil || !strings.Contains(s.Err().Error(), "no turn to judge") {
		t.Fatalf("judgment before a turn: %v", s.Err())
	}
	if s := e.Judge(Bool("polite", "?"), On("Thank you.")); !errors.Is(s.Err(), kit.ErrDecisionRefused) {
		t.Fatalf("refused: %v", s.Err())
	}
	if s := e.Judge(Bool("other", "?"), On("Thank you.")); !errors.Is(s.Err(), kit.ErrNoDecisionAnswer) {
		t.Fatalf("missing answer: %v", s.Err())
	}
	if s := e.Score("manual", 1.5); s.Err() == nil {
		t.Fatal("a score above 1 was accepted")
	}
	if len(r.errors()) != 4 {
		t.Fatalf("errors = %q", r.errors())
	}
}

// The language-model judge parses the score and the reason from a reply
// with text around the object.
func TestJudgeLLM(t *testing.T) {
	t.Parallel()
	var prompt string
	ask := func(_ context.Context, system, p string) (string, error) {
		prompt = p
		if strings.Contains(p, "bad") {
			return "I think it is fine.", nil
		}
		return "```json\n{\"score\": 0.7, \"reason\": \"It cites the policy.\"}\n```", nil
	}
	r := &recorder{TB: t}
	e := New(r, testAgent(t, fakemodel.New(fakemodel.Say("No refund after 30 days."))), WithLLMJudgeFunc(ask))
	e.Send("Refund?")
	s := e.JudgeLLM("The reply cites the policy.").AtLeast(0.6).Gate()
	if s.Value() != 0.7 || s.Reason() != "It cites the policy." || !s.Passed() {
		t.Fatalf("score = %v %q %v", s.Value(), s.Reason(), s.Err())
	}
	if !strings.Contains(prompt, "Criterion: The reply cites the policy.") || !strings.Contains(prompt, "No refund after 30 days.") {
		t.Fatalf("prompt = %q", prompt)
	}
	if s := e.JudgeLLM("bad"); s.Err() == nil {
		t.Fatal("a reply with no score was accepted")
	}
	if len(r.errors()) != 1 {
		t.Fatalf("errors = %q", r.errors())
	}
}

func TestParseLLMScore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reply string
		want  float64
		ok    bool
	}{
		{`{"score": 1, "reason": "yes"}`, 1, true},
		{`Here: {"score": 0}`, 0, true},
		{`{"reason": "no score"}`, 0, false},
		{`{"score": 2}`, 0, false},
		{`{"score": "high"}`, 0, false},
		{`no object`, 0, false},
	} {
		got, _, err := parseLLMScore(tc.reply)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseLLMScore(%q) = %v, %v", tc.reply, got, err)
		}
	}
}

// Restart opens a new runtime that shares only the journal. The second turn
// sees the first, so the conversation came from the journal.
func TestRestartContinuesFromJournal(t *testing.T) {
	t.Parallel()
	model := fakemodel.New(fakemodel.Say("first answer"), fakemodel.Say("second answer"))
	e := New(t, testAgent(t, model))
	e.Send("first question").Succeeded()
	e.Restart()
	e.Send("second question").Succeeded()
	reqs := model.Requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1].Text(), "first answer") {
		t.Fatalf("the turn after the restart did not see the first turn: %d requests", len(reqs))
	}
}

// A turn that asks for approval waits. Respond answers it and the run
// continues.
func TestSuspendAndRespond(t *testing.T) {
	t.Parallel()
	model := fakemodel.New(
		fakemodel.Call("request_approval", `{"action":"issue a refund"}`),
		fakemodel.Say("The refund is issued."),
	)
	e := New(t, testAgent(t, model))
	turn := e.Send("Refund order 7.").Suspended().CalledTool("request_approval")
	if turn.Run().Suspend.Prompt != "issue a refund" {
		t.Fatalf("suspend = %+v", turn.Run().Suspend)
	}
	e.Restart()
	e.Respond(runtime.Approve("ok")).Succeeded()
	e.Check(e.Last().Reply(), Contains("issued"))
}

// Decided grades a decision the agent made itself, from the journal.
func TestDecided(t *testing.T) {
	t.Parallel()
	decide, _ := scripted(kit.DecisionAnswer{ID: "destructive", Type: kit.DecisionBoolean, Probability: 0.9})
	model := fakemodel.New(
		fakemodel.Call(bonnie.DecideToolName, `{"state":"rm -rf /","questions":[{"id":"destructive","type":"boolean","instructions":"Does this delete data?"}]}`),
		fakemodel.Say("I will not run it."),
	)
	r := &recorder{TB: t}
	e := New(r, testAgent(t, model, bonnie.WithDecider(decide), bonnie.WithDecisionTool()))
	turn := e.Send("Clean the disk.")
	if len(turn.Decisions()) != 1 {
		t.Fatalf("decisions = %+v", turn.Decisions())
	}
	s := turn.Decided("destructive").AtMost(0.2).Gate()
	if s.Value() != 0.9 {
		t.Fatalf("value = %v", s.Value())
	}
	if s := turn.Decided("absent"); s.Err() == nil {
		t.Fatal("a decision that is not in the turn has a value")
	}
	if errs := r.errors(); len(errs) != 2 || !strings.Contains(errs[0], "want at most 0.200") {
		t.Fatalf("errors = %q", errs)
	}
}

// When the eval ends, each score is logged as one JSON report.
func TestReportsWhenEvalEnds(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	t.Run("eval", func(t *testing.T) {
		r.TB = t
		e := New(r, testAgent(t, fakemodel.New()), WithStrict(false))
		e.Score("coverage", 0.5).AtLeast(0.75)
		e.Score("speed", 1)
	})
	var reports []Report
	for _, line := range r.logs {
		if rest, ok := strings.CutPrefix(line, ReportPrefix); ok {
			var rep Report
			if err := json.Unmarshal([]byte(rest), &rep); err != nil {
				t.Fatal(err)
			}
			reports = append(reports, rep)
		}
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %+v (logs %q)", reports, r.logs)
	}
	if reports[0].Label != "coverage" || reports[0].Status != StatusScored || *reports[0].Min != 0.75 || reports[0].Test != "TestReportsWhenEvalEnds/eval" {
		t.Errorf("coverage = %+v", reports[0])
	}
	if reports[1].Label != "speed" || reports[1].Status != StatusRecorded || reports[1].Kind != KindScore {
		t.Errorf("speed = %+v", reports[1])
	}
}

// Two evals must not share one agent: each configures its own journal.
func TestAgentUsedTwiceFails(t *testing.T) {
	t.Parallel()
	agent := testAgent(t, fakemodel.New())
	New(t, agent)
	fatal := make(chan string, 1)
	go func() {
		r := &fatalRecorder{recorder{TB: t}, fatal}
		New(r, agent)
		fatal <- ""
	}()
	if msg := <-fatal; !strings.Contains(msg, "used by another eval") {
		t.Fatalf("fatal = %q", msg)
	}
}

type fatalRecorder struct {
	recorder
	fatal chan string
}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fatal <- fmt.Sprintf(format, args...)
	// Stop the goroutine as testing.T.Fatalf does.
	goruntime.Goexit()
}

func TestMatchers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		m    Matcher
		s    string
		want bool
	}{
		{Contains("30"), "30 days", true},
		{ContainsFold("DAYS"), "30 days", true},
		{Equals("ok"), "  ok\n", true},
		{Equals("ok"), "okay", false},
		{Matches(`^\d+ days$`), "30 days", true},
		{Matches(`(`), "anything", false},
		{Not(Contains("x")), "abc", true},
		{Not(Contains("a")), "abc", false},
		// A check that cannot run fails, also when it is negated.
		{Not(Matches(`(`)), "anything", false},
		{Not(Not(Matches(`(`))), "anything", false},
	} {
		if got, _ := tc.m.Match(tc.s); got != tc.want {
			t.Errorf("match %q = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// With EnvDir set, the journal stays after the test, in a new directory for
// each eval, and the eval logs where it is. A directory that exists is never
// used again. This test sets the process
// environment, so it does not run in parallel.
func TestKeptJournal(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvDir, root)
	r := &recorder{}
	for range 2 {
		t.Run("case/a", func(t *testing.T) {
			r.TB = t
			e := New(r, testAgent(t, fakemodel.New(fakemodel.Say("kept"))))
			e.Send("hello").Succeeded()
		})
	}
	var dirs []string
	for _, line := range r.logs {
		if rest, ok := strings.CutPrefix(line, RunPrefix); ok {
			var info RunInfo
			if err := json.Unmarshal([]byte(rest), &info); err != nil {
				t.Fatal(err)
			}
			if info.Turns != 1 {
				t.Errorf("turns = %d", info.Turns)
			}
			dirs = append(dirs, info.Journal)
		}
	}
	// go test names the second subtest case/a#01. The -2 suffix is for a
	// test that runs again with the same name (go test -count).
	want := []string{filepath.Join(root, "TestKeptJournal__case__a"), filepath.Join(root, "TestKeptJournal__case__a_01")}
	if !slices.Equal(dirs, want) {
		t.Fatalf("journals = %q, want %q", dirs, want)
	}
	if got, _ := journalDirIn(t, root, "TestKeptJournal/case/a"); got != want[0]+"-2" {
		t.Errorf("a name used again got %s", got)
	}
	journal, err := runtime.OpenSQLiteJournal(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.Close() }()
	recs, err := journal.Replay(t.Context(), RunID)
	if err != nil || !strings.Contains(transcript(recs), "assistant: kept") {
		t.Fatalf("kept journal: %v, %q", err, transcript(recs))
	}
}

// journalDirIn calls journalDir as a test named name would.
func journalDirIn(t *testing.T, root, name string) (string, bool) {
	t.Setenv(EnvDir, root)
	return journalDir(namedTB{TB: t, name: name})
}

type namedTB struct {
	testing.TB
	name string
}

func (n namedTB) Name() string { return n.name }

// Gate can come before the threshold. A gate with no threshold when the
// eval ends fails the eval and is reported as failed.
func TestGateBeforeThreshold(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	var early []string
	t.Run("eval", func(t *testing.T) {
		r.TB = t
		e := New(r, testAgent(t, fakemodel.New()), WithStrict(false))
		e.Score("met", 0.9).Gate().AtLeast(0.8)
		e.Score("under", 0.1).Gate().AtMost(0.2)
		e.Score("missed", 0.9).Gate().AtLeast(0.95)
		e.Score("bare", 0.5).Gate()
		// A NaN bound would make every comparison false, so the gate would pass.
		e.Score("nan-min", 0.5).AtLeast(math.NaN()).Gate()
		e.Score("nan-max", 0.5).Gate().AtMost(math.NaN())
		early = r.errors()
	})
	if len(early) != 3 || !strings.Contains(early[0], `"missed" is 0.900`) ||
		!strings.Contains(early[1], `"nan-min": threshold NaN is not a number`) || !strings.Contains(early[2], `"nan-max": threshold NaN`) {
		t.Fatalf("errors before the eval ended = %q", early)
	}
	errs := r.errors()
	if len(errs) != 4 || !strings.Contains(errs[3], `score "bare": Gate needs AtLeast or AtMost`) {
		t.Fatalf("errors = %q", errs)
	}
	status := map[string]string{}
	for _, line := range r.logs {
		if rest, ok := strings.CutPrefix(line, ReportPrefix); ok {
			var rep Report
			if err := json.Unmarshal([]byte(rest), &rep); err != nil {
				t.Fatal(err)
			}
			status[rep.Label] = rep.Status
		}
	}
	want := map[string]string{"met": StatusPassed, "under": StatusPassed, "missed": StatusFailed, "bare": StatusFailed, "nan-min": StatusFailed, "nan-max": StatusFailed}
	if !reflect.DeepEqual(status, want) {
		t.Fatalf("statuses = %v, want %v", status, want)
	}
}
