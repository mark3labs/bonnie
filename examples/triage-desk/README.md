# triage-desk

A reference for BONNIE's decision models. The agent reads a support inbox and
routes each ticket to a queue. A **decision model** does the routing: it
answers typed questions (which team, how urgent, is it a security incident)
with probabilities, not text. Code in this tree turns the probabilities into a
queue.

It needs no outside service: talk to it in the `bonnie dev` terminal.

## This directory is an agent tree

This directory was made with `bonnie init triage-desk`. It is its own Go
module, and you run and manage it with the `bonnie` CLI, the same way as your
own agent:

| File | What it is |
|---|---|
| `instructions.md` | the system prompt |
| `main.go` | the `bonnie.New(...)` call: the decision model, the `decide` tool, and the `file_ticket` tool |
| `triage.go` | `file_ticket`: the routing questions and the policy that reads the answers |
| `context/inbox.md` | a sample inbox, copied into each run |
| `bonnie_gen.go` | the wiring `bonnie dev` and `bonnie build` regenerate; do not edit |
| `skills/` | the agent's skills, empty here |
| `go.mod`, `go.sum` | pin a released BONNIE |

## What you need

- The CLI: `go install github.com/mark3labs/bonnie/cmd/bonnie@latest`.
- A chat model key: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, or `GEMINI_API_KEY`.
- A decision model. `main.go` uses `openrouter/perplexity/pplx-decider-flash-v1.2`,
  which needs `OPENROUTER_API_KEY`. To use a local model with no key, run
  Ollama and set `decisionModel` to an Ollama model, for example
  `ollama/pplx-decider`.

## Run it

```bash
cd examples/triage-desk
bonnie dev
```

In the terminal, ask:

```text
Triage the inbox.
```

The agent reads `inbox.md` and calls `file_ticket` once per ticket. You get
one line per ticket: the account takeover (T-1046) goes to the security queue
and pages on-call, the outage (T-1042) is critical, the prize message (T-1044)
is closed as spam.

## What to look at

**Each decision is in the journal.** Open the run console:

```bash
bonnie runs --journal .bonnie
```

The Journal tab has one `decision` record per ticket, with the questions, the
answers and the token usage. The Trace tab shows each one as a `decide` event
under its `file_ticket` call. A record never holds the ticket text itself.

**A keyed decision is durable.** `file_ticket` keys each decision on the
ticket ID. Ask the agent to triage the inbox again in the same conversation:
the routing comes back from the journal, and no new `decision` record is
written. This is also true after a crash. `file_ticket` is replay-safe, so a
call that a crash interrupted runs again at recovery and gets the stored
routing.

**The model decides nothing on its own.** The probabilities go to `route` in
`triage.go`. That function holds the policy: a security probability of 0.5 or
more goes to the security queue, a refused answer or an unavailable model
goes to a human. Change the thresholds there, not in the prompt.

**The model can ask too.** `WithDecisionTool` gives the chat model the
`decide` tool. The instructions tell it to use the tool on tickets that look
like spam.

See the [Decisions guide](https://go-bonnie.dev/guides/decisions) for the
whole API.
