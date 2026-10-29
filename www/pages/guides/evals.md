---
title: Evals
description: Write scored checks that drive your real agent and grade it with run checks, a decision-model judge, or a language-model judge, then run them with bonnie eval.
---

# Evals

An eval is a scored check of your agent. It sends real input to the real agent, then grades what the agent did. Package `github.com/mark3labs/bonnie/eval` drives the agent, and `bonnie eval` runs the evals of a tree and reports the scores.

An eval is a Go test. You get `go test` filters, parallel runs, timeouts, repeats, and exit codes, and there is no configuration file.

Evals use three types of check. Use the first type that can answer your question:

1. **Run checks** read the journal: the state of the turn, the tools it called, the steps it took, the decisions it made. They are exact and free.
2. **The decision judge** asks a decision model typed questions about the reply. It returns probabilities, so a score needs no parsing, and a call costs little. This is the recommended judge.
3. **The language-model judge** asks a plain chat model for a score and a reason. Use it when no decision model fits the question, or when a written reason helps.

The checks read what the run recorded in the journal. This is the same record that replay and crash recovery use, so a check grades what really happened.

## Write the first eval

An eval must build the same agent that your binary serves. A tree from `bonnie init` has a `newAgent` function in `main.go` for this:

```go
func main() {
    newAgent().Serve()
}

// newAgent builds the agent. main serves it, and each eval builds its own
// with it, so an eval tests the agent this binary ships.
func newAgent() *bonnie.Agent {
    return bonnie.New(
        bonnie.WithModel("anthropic/claude-sonnet-4-5"),
    )
}
```

An older tree calls `bonnie.New(...).Serve()` in `main`. Move the call into `newAgent` as above. Do not put the options in a second place: an eval that builds its own agent tests options that the binary does not ship.

Put the eval in a file that ends in `_eval_test.go`, and start the file with the build constraint `//go:build eval`. The constraint keeps the eval out of a plain `go test`, which must never call a live model.

```go
//go:build eval

package main

import (
    "testing"

    "github.com/mark3labs/bonnie/eval"
)

func TestRefundPolicy(t *testing.T) {
    t.Parallel()
    e := eval.New(t, newAgent())

    turn := e.Send("Can I get a refund after 40 days?")
    turn.Succeeded().
        CalledTool("lookup_policy").
        NotCalledTool("issue_refund")
    e.Check(turn.Reply(), eval.Contains("30 days"))

    e.Judge(eval.Bool("cites_policy", "Does the response refuse and cite the 30-day policy?")).
        AtLeast(0.8)
}
```

`eval.New` opens the agent in the test process. It uses the same agent factory and runner as `Agent.Run`, but it starts no server, channel, or scheduler. The journal is in a temporary directory, so each eval starts with no history. The runtime closes when the test ends.

Give each eval its own agent: call `newAgent()` in each test. `eval.New` configures the journal of the agent it gets, so it fails the test when two evals share one agent.

## Run the evals

```bash
bonnie eval --judge openrouter/typesafe/jev-1.14
```

`bonnie eval` generates `bonnie_gen.go` as `bonnie build` does, then runs `go test -tags eval -json ./...` in the tree.

On a terminal, it opens the eval interface. The evals show as they run: a spinner for each running eval, then a mark when it ends. When the run ends, the interface stays open so you can read the results:

```text
 BONNIE eval · support · 20261029-120000          FAIL   ✓ 1  ✗ 1  scores  1 passed  1 scored  0 failed  0.4s
╭─ Evals ──────────────────────╮╭─ TestRefundPolicy ─────────────────────────────────────────────────────────╮
│ ✓ TestRefundPolicy      0.1s ││ 1 Scores  │  2 Conversation  │  3 Output                                   │
│ ✗ TestGreeting          0.1s ││                                                                            │
│                              ││  PASS   0.1s  ·  1 turn                                                    │
│                              ││                                                                            │
│                              ││ KIND     LABEL                       SCORE               BOUND     STATUS  │
│                              ││ judge    cites_policy                0.940  ━━━━━━━━━━━│ >= 0.900  passed  │
│                              ││ llm      The response offers an a…   0.550  ━━━━━━━━│━━━ >= 0.700  scored  │
│                              ││   ↳ It refuses but does not offer store credit as an alternative.          │
╰──────────────────── 1 of 2 ──╯╰────────────────────────────────────────────────────────────────────────────╯
 o open run console  ·  ↑↓ select  ·  tab focus  ·  1-3 tab  ·  f failed only  ·  q quit
```

| Key | Action |
| --- | --- |
| `↑` `↓` or `k` `j` | Select an eval, or scroll the detail when it has the focus |
| `tab` | Move the focus between the list and the detail |
| `1` `2` `3`, or `←` `→` | Show the scores, the conversation, or the raw test output |
| `f` | Show only the failed evals |
| `o` or `enter` | Open the eval's journal in the run console, with the Conversation, Trace and Journal tabs of `bonnie runs`. Quit the console to come back. |
| `r` | Run the selected eval again |
| `R` | Run each failed eval again |
| `q` | Quit. While evals still run, the first `q` asks, and a second `q` stops them. |

A rerun happens in the same screen and keeps the whole suite on it. The evals that you rerun show a spinner at once, also while `go test` builds the tests, and then get a new result. The others keep their result from the earlier run, muted, with the time of that run; their detail says which run made them. The totals and the exit code are those of the whole suite, so you can fix, press `R`, and repeat until the header shows PASS.

A rerun uses the same flags and is a new saved run that holds the merged results. Its header names the run it came from, and the earlier run stays unchanged in `bonnie eval list`, so you can compare the two. A run that a kept run carries results from is not pruned, so its journals stay. The tree's wiring is generated again first, so a change that you made to the agent or to the eval is in the rerun. A merged run can therefore hold results from different versions of the code; the "from" mark shows which results are older. A rerun waits until the current run ends. When you quit, the command prints the last run.

To run one eval from the command line, give `--run` an anchored pattern for each level of its name. `go test` splits the pattern on `/`:

```bash
bonnie eval --run '^TestRefundPolicy$'
bonnie eval --run '^TestRefundDataset$/^outside_window$'
```

A subtest name has its spaces changed to `_`, as `go test` shows it.

When you quit, the plain table stays in the terminal. With `--tui=false`, or when the output is not a terminal (CI, a pipe), `bonnie eval` prints only the plain table:

```text
PASS  TestRefundPolicy  0.1s
      judge    cites_policy                 0.940  >= 0.900     passed
      llm      The response offers an alte… 0.550  >= 0.700     scored
        label: The response offers an alternative to a refund.
        reason: It refuses but does not offer store credit as an alternative.
FAIL  TestGreeting  0.1s
      support_eval_test.go:53: bonnie: eval: check failed: "I can help with that." does not contain "hello" (case ignored)

evals: 1 passed, 1 failed, 0 skipped; scores: 1 passed, 1 scored, 0 failed, 0 recorded
saved: /home/me/support/.bonnie/evals/20261029-120000
```

The command exits with status 0 only when each eval passes its gates. A tree with no evals is an error, so a CI job cannot pass without a check. See [CLI](/reference/cli) for each flag.

### Saved runs

Each `bonnie eval` is kept in the tree's journal directory, in `.bonnie/evals/<id>`. The ID is the start time, for example `20261029-120000`. The directory holds:

- `results.json`: each eval, its status, its output and its scores.
- `journals/<test name>/`: the journal of each eval, with its sandbox working files. Open it with `bonnie runs --journal <dir>`, or press `o` in the interface.

`bonnie eval show` opens the newest saved run in the interface again, and `bonnie eval show --id <id>` opens an older one. `bonnie eval list` lists the saved runs. `bonnie eval` keeps the newest 10 runs; change this with `--keep`, and `--keep 0` keeps each run. A run folder without `results.json` is removed only when it is more than a day old: a newer one can be the run of another `bonnie eval` in the same tree, which saves its results when it ends. A run can hold the files that the agent wrote, so do not commit `.bonnie/` to version control.

`go test -tags eval ./...` also runs the evals, with the normal `go test` output. The score reports are then log lines that start with `bonnie-eval: `, and you see them with `-v` or when a test fails.

The evals use the provider keys of the agent. `eval.New` loads the `.env` file of the test's working directory as `Agent.Run` does. For an eval in the tree's root package, that is the tree's `.env`. A variable that is already set in the environment wins. `BONNIE_EVAL_JUDGE` and the other eval variables can also be in that file.

## Gates and scores

A **gate** fails the eval. The run checks and `e.Check` are gates. A failed gate marks the test failed, and the eval continues, so one run shows each failure.

A **score** is a number from 0 to 1. Judges, the agent's own decisions, and `e.Score` give scores. A score is soft by default:

| Score | Result |
| --- | --- |
| No threshold | Recorded only. Status `recorded`. |
| `AtLeast(x)` or `AtMost(x)`, met | Status `passed`. |
| `AtLeast(x)` or `AtMost(x)`, missed | Status `scored`. The eval does not fail, unless strict mode is on. |
| A threshold and `Gate()`, missed | Status `failed`. The eval fails. |
| The judge failed | Status `failed`. The eval fails, also with no threshold. |

Strict mode makes each missed soft threshold fail its eval. Turn it on with `bonnie eval --strict`, with `BONNIE_EVAL_STRICT=1`, or for one eval with `eval.WithStrict(true)`. Use strict mode in CI when a quality drop must stop a merge.

```go
// Soft: a miss is reported, and fails only in strict mode.
e.Judge(eval.Bool("polite", "Is the response polite?")).AtLeast(0.8)

// Gate: a miss always fails.
e.Judge(eval.Bool("safe", "Does the response refuse to share the password?")).AtLeast(0.95).Gate()
```

`Gate()` needs a threshold. A boolean judge rarely gives exactly 1, so always give the bar that you accept.

## Run checks

`e.Send` starts a turn and waits until it ends. It returns a `*eval.Turn`. Its checks are gates, and they return the turn, so they chain.

| Check | Fails when |
| --- | --- |
| `Succeeded()` | The turn did not complete, or it ended with an error |
| `Suspended()` | The turn does not wait for a human answer |
| `CalledTool(name)` | The turn did not call the tool |
| `CalledToolWith(name, m)` | No call of the tool has an input that matcher `m` matches |
| `NotCalledTool(name)` | The turn called the tool |
| `NoFailedTools()` | A tool call returned an error |
| `MaxSteps(n)` | The turn took more than `n` agent steps |

Read the turn for your own checks: `Reply()`, `ToolCalls()`, `Steps()`, `Usage()`, `Decisions()`, `Messages()`, `Records()`, and `Run()`.

`e.Check(value, matcher)` checks any string. The matchers are `Contains`, `ContainsFold`, `Equals` (white space at the ends is ignored), `Matches` (a regular expression), and `Not`. Implement `eval.Matcher`, or use `eval.MatcherFunc`, for your own.

```go
turn.CalledToolWith("lookup_order", eval.Matches(`"order_id":\s*"A-1009"`))
e.Check(turn.Reply(), eval.Not(eval.ContainsFold("as an ai")))
```

`e.Score(label, value)` records a number that your eval computes, for example the share of expected facts that a reply contains.

## The decision judge

A decision model answers typed questions with probabilities. See [Decisions](/guides/decisions) for the models. The judge gives three types of question, and each one becomes a score from 0 to 1:

| Question | Score |
| --- | --- |
| `eval.Bool(id, instructions)` | The model's P(true). This is an estimate, not a calibrated confidence. |
| `eval.Rubric(id, instructions, levels...)` | The expected level divided by the highest level. Give 2 levels or more, worst first. |
| `eval.Choice(id, instructions, expected, options...)` | The probability of the expected option. When the model gives no distribution: 1 when it selects the expected option, else 0. |

The expected option of a choice question stays in the eval. It is never sent to the model.

`e.Judge` asks one question. `e.JudgeAll` asks many questions in one request, about the same state, and returns one score for each question ID. One request for many questions costs less than one request for each.

```go
scores := e.JudgeAll([]eval.Question{
    eval.Bool("accurate", "Is the response consistent with the reference?"),
    eval.Rubric("clarity", "Grade the clarity of the response.", "unclear", "mostly clear", "clear"),
    eval.Choice("outcome", "Classify the response.", "declined",
        kit.DecisionOption{Name: "answered", Description: "Gives a refund"},
        kit.DecisionOption{Name: "declined", Description: "Refuses the refund"}),
}, eval.Reference("Refunds are allowed for 30 days only."))
scores["accurate"].AtLeast(0.9)
scores["clarity"].AtLeast(0.5)
scores["outcome"].AtLeast(0.7).Gate()
```

### What the judge sees

By default the judge grades the latest turn. The state is JSON with two keys: `input`, the text of the latest `Send`, and `response`, the reply of the latest turn. Change the state with an option:

| Option | Effect |
| --- | --- |
| `eval.Reference(answer)` | Adds the key `reference` to the default state |
| `eval.On(value)` | Grades `value` in place of the default. A string is sent as text, and any other value as JSON. |
| `eval.Model(model)` | Uses a different judge model for this judgment |

To grade a whole conversation, give the transcript:

```go
e.Send("I want to return my order.")
e.Send("It is order A-1009.")
e.Judge(eval.Bool("asked_once", "Does the assistant ask for the order number only once?"),
    eval.On(e.Transcript()))
```

### Choose the judge model

The judge model is the first one that is set:

1. `eval.Model(model)` on the judgment
2. `eval.WithJudge(model)` on `eval.New`
3. `BONNIE_EVAL_JUDGE`, which `bonnie eval --judge` sets

The judge does not use the agent's `WithDecisionModel`. A judge must be separate from the agent under test, so the agent never grades its own work by accident. You can give the same model on purpose.

A judgment with no model, a missing key, a provider error, a refused question, or an answer that is not a valid score fails the eval, also when the score has no threshold. An eval never skips a judgment silently and never changes to a different model.

Judge calls are not written to the run's journal. They are about the run, not part of it. Their scores are in the report.

## The language-model judge

`e.JudgeLLM(criterion)` asks a chat model how well the latest turn meets the criterion. The model replies with JSON that has a score from 0 to 1 and a one-sentence reason. The eval accepts text around the JSON object, such as a code fence.

```go
e.JudgeLLM("The response explains why the refund is refused, in plain words.").
    AtLeast(0.7)
```

It takes the same options as `e.Judge`. The model is the first one that is set: `eval.Model`, then `eval.WithLLMJudge`, then `BONNIE_EVAL_LLM_JUDGE` (`bonnie eval --llm-judge`). Kit creates the provider with stored credentials or the environment, and calls it once, with temperature 0.

Prefer the decision judge. A language model writes a number that the eval must parse. That number is not calibrated, and the call costs more. The reason shows in the report when a score misses its threshold.

## Grade the agent's own decisions

When the agent uses a decision model, for example to gate risky tool calls, `turn.Decided(id)` gives a score from the agent's latest decision on that question in the turn. It reads the `decision` records of the journal:

```go
turn := e.Send("Delete the old log files.")
turn.Decided("destructive").AtMost(0.2).Gate()
```

A boolean question gives P(true), and a score question gives the position from 0 to 1. A choice question, or a turn with no such decision, gives a failed score. Read `turn.Decisions()` for the full records.

## Human input and restarts

These checks are particular to a durable agent.

`Suspended()` checks that a turn waits for a person. `e.Respond` answers it:

```go
turn := e.Send("Refund order A-1009.").Suspended().CalledTool("request_approval")
e.Respond(runtime.Approve("approved by the eval")).Succeeded()
```

`e.Restart()` closes the runtime and opens a new one that shares only the journal, as a new process does after a crash. The next turn must continue from the journal:

```go
e.Send("My order number is A-1009.").Succeeded()
e.Restart()
turn := e.Send("What was my order number?").Succeeded()
e.Check(turn.Reply(), eval.Contains("A-1009"))
```

Put `Restart` between `Send` and `Respond` to prove that an approval survives a restart.

## Datasets

A dataset is a table of cases. Use subtests, and build a new agent in each case:

```go
var refundCases = []struct {
    name, input, want string
}{
    {"inside window", "I bought it 10 days ago. Refund?", "answered"},
    {"outside window", "I bought it 40 days ago. Refund?", "declined"},
}

func TestRefundDataset(t *testing.T) {
    t.Parallel()
    for _, tc := range refundCases {
        t.Run(tc.name, func(t *testing.T) {
            t.Parallel()
            e := eval.New(t, newAgent())
            e.Send(tc.input).Succeeded()
            e.Judge(eval.Choice("outcome", "Classify the response.", tc.want,
                kit.DecisionOption{Name: "answered"}, kit.DecisionOption{Name: "declined"})).
                AtLeast(0.7)
        })
    }
}
```

Keep large datasets in a file under `testdata/` and read it in the test. `bonnie eval --run 'TestRefundDataset/outside'` runs one case. `bonnie eval --count 5` runs each eval five times, to show how much the agent varies.

## Evals in CI

```yaml
- name: Evals
  env:
    ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
    OPENROUTER_API_KEY: ${{ secrets.OPENROUTER_API_KEY }}
  run: bonnie eval --strict --judge openrouter/typesafe/jev-1.14
```

`bonnie eval --json` prints each score as one JSON line, for a dashboard or a later comparison. Each line is an `eval.Report`.

Evals call live models, so they cost money and their results vary. Run them on a schedule or before a release, not on each push. Set thresholds with a margin, and use `--count` to measure the variation before you make a score a gate.

## Test the eval itself

An eval is code, and it can be wrong. Script the judges to test it without a model:

- `eval.WithDecider(d)` replaces the decision judge with a function. With a decider, a judgment needs no model.
- `eval.WithLLMJudgeFunc(f)` replaces the language-model judge.
- `bonnie.WithAgentFactory` replaces the agent with a deterministic one.

## Limits

- The agent runs in the test process, with the sandbox and the tools of the agent. Its tool calls have real effects. Use a sandbox and test accounts that an eval can safely change.
- There are no tool stubs yet. An eval cannot replace what a tool returns. Kit's tool hooks can block a call, but they cannot replace its result.
- Evals drive an agent in process. They do not target a deployed agent over HTTP.
- A judge score is a model's estimate. Do not use one judge score as the only proof that an agent is safe.

See [Evals reference](/reference/evals) for each type, option, and method.
