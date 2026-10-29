// Package eval runs evals against a BONNIE agent: scored checks that drive
// the real agent and grade what it did.
//
// An eval is a Go test. It lives in the agent tree in a file that ends in
// _eval_test.go and starts with the build constraint //go:build eval, so a
// plain go test never calls a live model. `bonnie eval` runs the evals of a
// tree and prints the scores. go test -tags eval runs them too.
//
//	//go:build eval
//
//	func TestRefundPolicy(t *testing.T) {
//		t.Parallel()
//		e := eval.New(t, newAgent())
//		turn := e.Send("Can I get a refund after 40 days?")
//		turn.Succeeded().CalledTool("lookup_policy").NotCalledTool("issue_refund")
//		e.Check(turn.Reply(), eval.Contains("30 days"))
//		e.Judge(eval.Bool("policy", "The reply refuses and cites the 30-day policy.")).AtLeast(0.8)
//	}
//
// [New] runs the agent in process, with the same factory and runner as
// [bonnie.Agent.Run], on a journal in a temporary directory. The checks read
// that journal, so they grade what the run recorded, not a stream the eval
// parsed.
//
// # Gates and scores
//
// A gate fails the eval. The run checks on [Turn] and [T.Check] are gates.
// A [Score] is a number from 0 to 1. A score with no threshold is only
// recorded. [Score.AtLeast] and [Score.AtMost] set a soft threshold: a miss
// marks the eval "scored" but does not fail it, unless strict mode is on
// (BONNIE_EVAL_STRICT=1 or `bonnie eval --strict`). [Score.Gate] makes the
// threshold a gate.
//
// # Judges
//
// The recommended judge is a decision model ([T.Judge], [T.JudgeAll]). It
// answers typed questions with probabilities, so a score needs no parsing
// and costs little. A plain language model is the second choice
// ([T.JudgeLLM]): use it when no decision model fits, or when a written
// reason helps. Each judge is separate from the agent under test. A judge
// that cannot answer, for example because no model is set or a key is
// missing, fails the eval: an eval never skips a judgment silently.
package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/runtime"
)

// Environment variables that set defaults for every eval in a process.
// `bonnie eval` sets them from its flags.
const (
	// EnvJudge is the decision model of [T.Judge], in "provider/model"
	// format, when [WithJudge] is not given.
	EnvJudge = "BONNIE_EVAL_JUDGE"
	// EnvLLMJudge is the language model of [T.JudgeLLM], in
	// "provider/model" format, when [WithLLMJudge] is not given.
	EnvLLMJudge = "BONNIE_EVAL_LLM_JUDGE"
	// EnvStrict set to "1" or "true" makes a missed soft threshold fail
	// the eval.
	EnvStrict = "BONNIE_EVAL_STRICT"
	// EnvDir is a directory that keeps the journal of each eval after the
	// test ends, one subdirectory for each eval. `bonnie eval` sets it to a
	// directory under .bonnie/evals, so a person can read each
	// conversation later. When it is empty, the journal is in a temporary
	// directory and is removed with the test.
	EnvDir = "BONNIE_EVAL_DIR"
)

// RunID is the ID of the run an eval drives.
const RunID = "eval"

// ErrNoJudge means that a judgment has no model to ask.
var ErrNoJudge = errors.New("bonnie: eval: no judge model configured")

// Option configures an eval.
type Option func(*config)

type config struct {
	judge     string
	decider   runtime.Decider
	llmJudge  string
	llmFunc   LLMJudgeFunc
	strict    bool
	strictSet bool
}

// WithJudge sets the decision model of [T.Judge] and [T.JudgeAll], in
// "provider/model" format, for example "openrouter/typesafe/jev-1.14". It
// takes precedence over [EnvJudge]. See [kit.GetDecisionModels].
func WithJudge(model string) Option {
	return func(c *config) { c.judge = model }
}

// WithDecider replaces the provider call of the decision judge with d. A
// test uses it to script answers, and a host uses it to reach a model Kit
// cannot call. With a decider, a judgment needs no model.
func WithDecider(d runtime.Decider) Option {
	return func(c *config) { c.decider = d }
}

// WithLLMJudge sets the language model of [T.JudgeLLM], in "provider/model"
// format. It takes precedence over [EnvLLMJudge].
func WithLLMJudge(model string) Option {
	return func(c *config) { c.llmJudge = model }
}

// WithLLMJudgeFunc replaces the model call of [T.JudgeLLM] with f. A test
// uses it to script answers.
func WithLLMJudgeFunc(f LLMJudgeFunc) Option {
	return func(c *config) { c.llmFunc = f }
}

// WithStrict sets strict mode for one eval, in place of [EnvStrict]. In
// strict mode a missed soft threshold fails the eval.
func WithStrict(strict bool) Option {
	return func(c *config) { c.strict, c.strictSet = strict, true }
}

// T drives one agent through one run and grades it. Make it with [New].
// Its methods are for the goroutine of the test.
type T struct {
	tb    testing.TB
	agent *bonnie.Agent
	cfg   config

	scope    *scope
	turns    []*Turn
	scores   []*Score
	lastText string
	// journal is the kept journal directory, or "" for a temporary one.
	journal string
}

// journalDir returns the journal directory of one eval, and whether it is
// kept after the test. A kept directory is new and unique in [EnvDir]: a
// test that runs more than once (go test -count) gets one for each run.
// The temporary directory is made first in either case, so its cleanup,
// which runs last, follows the runtime's close.
func journalDir(tb testing.TB) (string, bool) {
	tb.Helper()
	tmp := tb.TempDir()
	root := os.Getenv(EnvDir)
	if root == "" {
		return tmp, false
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		tb.Fatalf("bonnie: eval: create %s: %v", root, err)
	}
	base := filepath.Join(root, dirName(tb.Name()))
	for i := 1; i < 10000; i++ {
		dir := base
		if i > 1 {
			dir = fmt.Sprintf("%s-%d", base, i)
		}
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, true
		}
		if !errors.Is(err, os.ErrExist) {
			tb.Fatalf("bonnie: eval: create %s: %v", dir, err)
		}
	}
	tb.Fatalf("bonnie: eval: no free journal directory for %s in %s", tb.Name(), root)
	return "", false
}

// dirName turns a test name into one safe path element.
func dirName(test string) string {
	var b strings.Builder
	for _, r := range test {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		case r == '/':
			b.WriteString("__")
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "eval"
	}
	return b.String()
}

// agents holds each agent an eval has used. An eval configures its agent,
// so two evals that share one agent would race on its journal.
var agents sync.Map

// New starts an eval of agent. It configures agent with a journal in a
// temporary directory and opens the agent's runtime, as [bonnie.Agent.Run]
// does, but with no server, channels or scheduler. Like Run, it loads the
// .env file of the working directory. The runtime closes when the test ends.
//
// Give each eval its own agent: build it in the test, for example with the
// tree's newAgent function. New fails the test when an agent is used again.
func New(tb testing.TB, agent *bonnie.Agent, opts ...Option) *T {
	tb.Helper()
	if agent == nil {
		tb.Fatalf("bonnie: eval: New needs an agent")
	}
	if _, used := agents.LoadOrStore(agent, true); used {
		tb.Fatalf("bonnie: eval: the agent is used by another eval; build a new agent for each eval")
	}
	journal, kept := journalDir(tb)
	agent.Configure(bonnie.WithJournal(journal))
	e := &T{tb: tb, agent: agent}
	if kept {
		e.journal = journal
	}
	e.open()
	tb.Cleanup(e.finish)
	// The environment is read after open, which loads the tree's .env file,
	// so a judge model set there applies.
	e.cfg = config{judge: os.Getenv(EnvJudge), llmJudge: os.Getenv(EnvLLMJudge)}
	for _, opt := range opts {
		opt(&e.cfg)
	}
	if !e.cfg.strictSet {
		e.cfg.strict = Strict()
	}
	return e
}

// Strict reports whether [EnvStrict] turns strict mode on.
func Strict() bool {
	v := os.Getenv(EnvStrict)
	return v == "1" || strings.EqualFold(v, "true")
}

// scope holds the runtime of one [bonnie.Agent.WithRuntime] call open until
// done is closed.
type scope struct {
	rt     *bonnie.Runtime
	done   chan struct{}
	exited chan error
}

func (e *T) open() {
	e.tb.Helper()
	s := &scope{done: make(chan struct{}), exited: make(chan error, 1)}
	ready := make(chan *bonnie.Runtime, 1)
	// The scope outlives the test's context, which ends before cleanup.
	ctx := context.WithoutCancel(e.tb.Context())
	go func() {
		s.exited <- e.agent.WithRuntime(ctx, func(_ context.Context, rt *bonnie.Runtime) error {
			ready <- rt
			<-s.done
			return nil
		})
	}()
	select {
	case s.rt = <-ready:
		e.scope = s
	case err := <-s.exited:
		if err == nil {
			err = errors.New("runtime closed before it opened")
		}
		e.tb.Fatalf("bonnie: eval: open agent: %v", err)
	}
}

func (e *T) close() error {
	if e.scope == nil {
		return nil
	}
	s := e.scope
	e.scope = nil
	close(s.done)
	return <-s.exited
}

// finish closes the runtime and reports the kept journal and each score.
func (e *T) finish() {
	if err := e.close(); err != nil {
		e.tb.Errorf("bonnie: eval: close agent: %v", err)
	}
	if e.journal != "" {
		b, err := json.Marshal(RunInfo{Test: e.tb.Name(), Journal: e.journal, Turns: len(e.turns)})
		if err != nil {
			e.tb.Errorf("bonnie: eval: encode run: %v", err)
		} else {
			e.tb.Logf("%s%s", RunPrefix, b)
		}
	}
	for _, s := range e.scores {
		s.finalize()
		r := s.report(e.tb.Name(), e.cfg.strict)
		b, err := json.Marshal(r)
		if err != nil {
			e.tb.Errorf("bonnie: eval: encode score: %v", err)
			continue
		}
		e.tb.Logf("%s%s", ReportPrefix, b)
	}
}

func (e *T) runner() *runtime.Runner {
	e.tb.Helper()
	if e.scope == nil {
		e.tb.Fatalf("bonnie: eval: the agent is closed")
	}
	return e.scope.rt.Runner()
}

// Send starts a turn with text and waits until the turn ends.
func (e *T) Send(text string) *Turn {
	e.tb.Helper()
	return e.SendInput(runtime.Input{Text: text})
}

// SendInput starts a turn with in and waits until the turn ends. Use it to
// give a turn context, a caller or files.
func (e *T) SendInput(in runtime.Input) *Turn {
	e.tb.Helper()
	e.lastText = in.Text
	return e.turn(func(ctx context.Context, r *runtime.Runner) (*runtime.Run, error) {
		return r.Start(ctx, RunID, in)
	})
}

// Respond answers the question or approval the run waits for, and waits
// until the turn ends. Build an approval answer with [runtime.Approve] or
// [runtime.Reject].
func (e *T) Respond(responses ...runtime.InputResponse) *Turn {
	e.tb.Helper()
	return e.turn(func(ctx context.Context, r *runtime.Runner) (*runtime.Run, error) {
		return r.Resume(ctx, RunID, responses)
	})
}

// Restart closes the agent's runtime and opens a new one that shares only
// the journal, as a new process does after a crash. The next turn proves
// that the run continues from what the journal holds.
func (e *T) Restart() {
	e.tb.Helper()
	if err := e.close(); err != nil {
		e.tb.Fatalf("bonnie: eval: restart: %v", err)
	}
	e.open()
}

func (e *T) turn(fn func(context.Context, *runtime.Runner) (*runtime.Run, error)) *Turn {
	e.tb.Helper()
	ctx := e.tb.Context()
	r := e.runner()
	before, err := e.replay(ctx, r)
	if err != nil {
		e.tb.Fatalf("bonnie: eval: read journal: %v", err)
	}
	run, err := fn(ctx, r)
	if err != nil {
		e.tb.Fatalf("bonnie: eval: turn: %v", err)
	}
	all, err := e.replay(ctx, r)
	if err != nil {
		e.tb.Fatalf("bonnie: eval: read journal: %v", err)
	}
	t := &Turn{e: e, run: run, records: all[len(before):], input: e.lastText}
	e.turns = append(e.turns, t)
	return t
}

func (e *T) replay(ctx context.Context, r *runtime.Runner) ([]runtime.Record, error) {
	recs, err := r.Journal().Replay(ctx, RunID)
	if errors.Is(err, runtime.ErrRunNotFound) {
		return nil, nil
	}
	return recs, err
}

// Records returns every journal record of the run, in order.
func (e *T) Records() []runtime.Record {
	e.tb.Helper()
	recs, err := e.replay(e.tb.Context(), e.runner())
	if err != nil {
		e.tb.Fatalf("bonnie: eval: read journal: %v", err)
	}
	return recs
}

// Transcript returns the conversation of the run as text, one message for
// each paragraph, with the role first. Tool calls show as their name and
// input. Give it to a judge with [On] to grade a whole conversation.
func (e *T) Transcript() string {
	e.tb.Helper()
	return transcript(e.Records())
}

// Last returns the latest turn, or nil before the first.
func (e *T) Last() *Turn {
	if len(e.turns) == 0 {
		return nil
	}
	return e.turns[len(e.turns)-1]
}

// Score records a score that the eval computed itself. value must be from
// 0 to 1.
func (e *T) Score(label string, value float64) *Score {
	e.tb.Helper()
	s := e.newScore(KindScore, label)
	if value < 0 || value > 1 || value != value {
		s.fail(fmt.Errorf("score %v is not from 0 to 1", value))
		return s
	}
	s.value = value
	return s
}

func (e *T) newScore(kind, label string) *Score {
	s := &Score{e: e, kind: kind, label: label}
	e.scores = append(e.scores, s)
	return s
}

// messages decodes the message records of recs.
func messages(recs []runtime.Record) []kit.LLMMessage {
	var out []kit.LLMMessage
	for _, rec := range recs {
		if rec.Kind != runtime.RecordMessage {
			continue
		}
		var msg kit.LLMMessage
		if len(rec.Payload) == 0 || json.Unmarshal(rec.Payload, &msg) != nil {
			msg = kit.LLMMessage{Role: kit.LLMMessageRole(rec.Role), Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: rec.Text}}}
		}
		out = append(out, msg)
	}
	return out
}
