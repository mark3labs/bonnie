# refund-desk

The refund assistant of a small electronics shop, with evals that grade it.
It is the reference for `bonnie eval`: each check in
[`refund_eval_test.go`](refund_eval_test.go) uses a different part of
package `eval`. It needs no outside service.

| File | What it holds |
|---|---|
| `instructions.md` | The prompt, with the refund policy |
| `orders.go` | `lookup_order` and `issue_refund`, on a fixed table of four orders |
| `main.go` | `newAgent`, which `main` serves and each eval builds |
| `refund_eval_test.go` | The evals |

## Run the agent

```bash
cd examples/refund-desk
bonnie dev
```

It needs a chat model key, for example `ANTHROPIC_API_KEY`. Ask it for a
refund of order `A-1001` (inside the 30-day window), `A-1002` (above
100 dollars, so it waits for your approval), `A-1003` (outside the window), or
`A-1004` (not delivered).

## Run the evals

```bash
export OPENROUTER_API_KEY=...
bonnie eval --judge openrouter/typesafe/jev-1.14 \
  --llm-judge anthropic/claude-sonnet-4-5
```

`--judge` is the decision model of `Judge` and `JudgeAll`. `--llm-judge` is
the chat model of `JudgeLLM`. A judgment with no model fails its eval; it
never skips. Run one eval with `--run`, for example
`bonnie eval --run '^TestRefundPolicy$/^outside_window$'`. Add `--strict` to
make each missed soft threshold fail, as in CI. Each run is saved in
`.bonnie/evals`; `bonnie eval show` opens the newest one again.

| Eval | What it shows |
|---|---|
| `TestRefundPolicy` | A dataset of subtests. Run checks on the tools called, a matcher on the reply, and a `Choice` judge. |
| `TestAsksForTheOrderOnce` | Two turns. A `Bool` judge as a gate, then `JudgeAll` on the whole transcript. |
| `TestLargeRefundWaitsForApproval` | `Suspended`, `Restart` with only the journal, then `Respond` with an approval. |
| `TestRejectedRefundIsNotMade` | `Respond` with a rejection, and the language-model judge. |

The run checks read the journal, so they are exact and cost nothing. The
judges cost a model call each, and their scores vary: run with `--count 5`
before you make a score a gate.

## Limits

- The evals call live models, and each run costs money.
- The approval limit of 100 dollars is in the prompt. The model can skip it,
  and `TestLargeRefundWaitsForApproval` catches that, but an eval is not a
  control. `issue_refund` enforces the window and a hard cap of 500 dollars in
  code; put each rule that must hold in code.
- `issue_refund` remembers refunds only in its process. A real desk gives the
  payment provider the refund ID as an idempotency key.
