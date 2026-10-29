---
title: Evals
description: Reference for package eval — the eval driver, run checks, matchers, scores, decision and language-model judges, reports, and environment variables.
---

# Evals

`github.com/mark3labs/bonnie/eval` drives an agent in a Go test and grades it. See [Evals](/guides/evals) for the guide. The godoc is the contract; this page is a summary.

An eval file ends in `_eval_test.go` and starts with `//go:build eval`. `bonnie eval` runs these files with `go test -tags eval`.

## Start an eval

```go
func New(tb testing.TB, agent *bonnie.Agent, opts ...Option) *T
```

`New` configures `agent` with a journal and opens its runtime with `Agent.WithRuntime`. The journal is in `tb.TempDir()` and is removed with the test. When `BONNIE_EVAL_DIR` is set, as `bonnie eval` does, the journal is kept in a new subdirectory of it, named after the test. It starts no server, channel, or scheduler. It loads the `.env` file of the working directory. The runtime closes in a cleanup of `tb`, and each score is reported then.

`New` fails the test when `agent` is nil or another eval already used it. Build a new agent for each eval.

| Option | Effect |
| --- | --- |
| `WithJudge(model)` | Decision model of `Judge` and `JudgeAll`, as `provider/model`. It takes precedence over `BONNIE_EVAL_JUDGE`. |
| `WithDecider(d runtime.Decider)` | Replaces the decision-judge call with `d`. With a decider, a judgment needs no model. |
| `WithLLMJudge(model)` | Chat model of `JudgeLLM`, as `provider/model`. It takes precedence over `BONNIE_EVAL_LLM_JUDGE`. |
| `WithLLMJudgeFunc(f LLMJudgeFunc)` | Replaces the `JudgeLLM` model call with `f`. |
| `WithStrict(bool)` | Sets strict mode for this eval, in place of `BONNIE_EVAL_STRICT`. |

```go
type LLMJudgeFunc func(ctx context.Context, system, prompt string) (string, error)
```

## Drive the run

Each eval drives one run, with the ID `eval.RunID` (`"eval"`). The methods of `*T` are for the test's goroutine.

| Method | Effect |
| --- | --- |
| `Send(text) *Turn` | Starts a turn with `text` and waits until it ends |
| `SendInput(runtime.Input) *Turn` | Starts a turn with context, a caller, or files |
| `Respond(responses ...runtime.InputResponse) *Turn` | Answers the question or approval that the run waits for |
| `Restart()` | Closes the runtime and opens a new one that shares only the journal |
| `Last() *Turn` | The latest turn, or nil |
| `Records() []runtime.Record` | Each journal record of the run |
| `Transcript() string` | The conversation as text: one paragraph per message, role first, tool calls as `[call name input]` |

A runner error, such as a refused input, fails the test at once with `Fatalf`. A turn that ends in the failed state is not a runner error: check it with `Succeeded`.

## Turn

A `*Turn` is one ended turn. It holds the run snapshot and the journal records that the turn wrote.

| Method | Returns |
| --- | --- |
| `Run()` | The `*runtime.Run` the turn ended with |
| `Reply()` | The final response |
| `Records()` | The records the turn wrote |
| `Messages()` | The conversation messages the turn wrote |
| `ToolCalls()` | Each `ToolCall`, in the order the model made them |
| `Steps()` | The number of completed agent steps |
| `Usage()` | The token usage, or zero |
| `Decisions()` | Each `runtime.DecisionRecord` that the agent's own decision calls wrote |

```go
type ToolCall struct {
    ID      string
    Name    string
    Input   string // the JSON input
    Output  string // the result text, or the error text
    IsError bool
    Done    bool   // false when the journal has no result
}
```

These checks are gates. Each one fails the test with `Errorf` and returns the turn:

| Check | Fails when |
| --- | --- |
| `Succeeded()` | The state is not completed, or the run has an error |
| `Suspended()` | The state is not waiting |
| `CalledTool(name)` | No call of `name` |
| `CalledToolWith(name, m Matcher)` | No call of `name` whose input `m` matches |
| `NotCalledTool(name)` | A call of `name` |
| `NoFailedTools()` | A tool result is an error |
| `MaxSteps(n)` | More than `n` steps |

`Decided(id) *Score` gives a score from the agent's latest decision on question `id` in the turn: P(true) for a boolean question, the position from 0 to 1 for a score question. A choice question or a missing decision gives a failed score.

## Matchers

```go
type Matcher interface {
    Match(s string) (ok bool, why string)
}
type MatcherFunc func(s string) (ok bool, why string)
```

`e.Check(value, m) bool` is a gate: it fails the test when `m` does not match.

| Matcher | Matches |
| --- | --- |
| `Contains(sub)` | A string that contains `sub` |
| `ContainsFold(sub)` | The same, with case ignored |
| `Equals(want)` | A string equal to `want`, with white space at the ends ignored |
| `Matches(pattern)` | A string the regular expression matches. An invalid pattern matches nothing. |
| `Not(m)` | A string that `m` does not match |

## Scores

A `*Score` is a value from 0 to 1. Its threshold methods return the score, so they chain.

| Method | Effect |
| --- | --- |
| `AtLeast(min)` | Soft threshold: the value must be `min` or more |
| `AtMost(max)` | Soft threshold: the value must be `max` or less |
| `Gate()` | Makes the thresholds a gate. It needs `AtLeast` or `AtMost`, before or after it. |
| `Label(name)` | Names the score in the report |
| `Value()` | The value; 0 when the score has an error |
| `Reason()` | The judge's reason, when there is one |
| `Err()` | Why the score has no value, or nil |
| `Passed()` | True when there is a value and it meets each threshold |

A missed soft threshold fails the test only in strict mode. A missed gate always fails. A score with an error fails the test when the score is made.

`e.Score(label, value) *Score` records a value that the eval computed. A value that is not from 0 to 1 gives a failed score.

## Decision judge

```go
func (e *T) Judge(q Question, opts ...JudgeOption) *Score
func (e *T) JudgeAll(qs []Question, opts ...JudgeOption) map[string]*Score
```

`JudgeAll` sends one request with each question and returns the scores by question ID. A question ID used twice fails the test.

| Question | Score |
| --- | --- |
| `Bool(id, instructions)` | P(true) |
| `Rubric(id, instructions, levels...)` | Expected level ÷ (number of levels − 1) |
| `Choice(id, instructions, expected, options...)` | P(expected), or 1 or 0 when there is no distribution. `expected` is not sent. |

| Judge option | Effect |
| --- | --- |
| `On(value)` | Grades `value`: a string as text, other values as JSON |
| `Reference(answer)` | Adds `reference` to the default state |
| `Model(model)` | Judge model of this judgment |

The default state is JSON: `{"input": <text of the latest Send>, "response": <reply of the latest turn>}`. A judgment before the first turn needs `On`.

The model is `Model`, then `WithJudge`, then `BONNIE_EVAL_JUDGE`. The call is `kit.Decide`, or the `WithDecider` function. These fail the score, also with no threshold: no model (`errors.Is(err, eval.ErrNoJudge)`), a provider error, a refused question (`kit.ErrDecisionRefused`), a missing answer (`kit.ErrNoDecisionAnswer`), and a value outside 0 to 1. Judge calls are not journalled.

## Language-model judge

```go
func (e *T) JudgeLLM(criterion string, opts ...JudgeOption) *Score
```

`JudgeLLM` sends one system instruction and one prompt with the criterion and the state. The reply must hold a JSON object `{"score": <0 to 1>, "reason": "..."}`; text around the object is ignored. The options and the default state are those of `Judge`.

The model is `Model`, then `WithLLMJudge`, then `BONNIE_EVAL_LLM_JUDGE`. The call creates the provider with `kit.CreateProvider` and calls it once with temperature 0, or calls the `WithLLMJudgeFunc` function. No model, a provider error, or a reply with no valid score fails the score.

## Reports

When the eval ends, each score is logged as one line: `eval.ReportPrefix` (`"bonnie-eval: "`) followed by a JSON `eval.Report`. `bonnie eval` reads these lines; `bonnie eval --json` prints them.

| Field | JSON | Meaning |
| --- | --- | --- |
| `Test` | `test` | Test name, with subtests |
| `Label` | `label` | Question ID, `Score` label, or the `JudgeLLM` criterion |
| `Kind` | `kind` | `judge`, `llm`, `decision`, or `score` |
| `Value` | `value` | 0 to 1 |
| `Min`, `Max` | `min`, `max` | Thresholds, when set |
| `Gate` | `gate` | True when the thresholds are a gate |
| `Status` | `status` | `recorded`, `passed`, `scored`, or `failed` |
| `Model` | `model` | The model that answered, when known |
| `Reason` | `reason` | The language-model judge's reason |
| `Error` | `error` | Why the score has no value |

## Kept journals

When `BONNIE_EVAL_DIR` is set, the eval also logs one line when it ends: `eval.RunPrefix` (`"bonnie-eval-run: "`) followed by a JSON `eval.RunInfo`.

| Field | JSON | Meaning |
| --- | --- | --- |
| `Test` | `test` | Test name |
| `Journal` | `journal` | The kept journal directory. Open it with `runtime.OpenSQLiteJournal`; the run ID is `eval.RunID`. |
| `Turns` | `turns` | Turns the eval drove |

## Environment

| Variable | Effect |
| --- | --- |
| `BONNIE_EVAL_JUDGE` | Default decision-judge model. `bonnie eval --judge` sets it. |
| `BONNIE_EVAL_LLM_JUDGE` | Default language-model judge. `bonnie eval --llm-judge` sets it. |
| `BONNIE_EVAL_STRICT` | `1` or `true` turns strict mode on. `bonnie eval --strict` sets it. |
| `BONNIE_EVAL_DIR` | Keeps each eval's journal in a subdirectory. `bonnie eval` sets it to `.bonnie/evals/<id>/journals`. |

`eval.Strict()` reports whether `BONNIE_EVAL_STRICT` turns strict mode on.
