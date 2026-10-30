# BONNIE examples

Every example is an **agent tree**, the same shape as your own agent. Each one
was made with `bonnie init`, is its own Go module, and is run and managed with
the `bonnie` CLI:

| Command | What it does |
|---|---|
| `bonnie dev` | serve the tree with hot reload |
| `bonnie build` | compile the tree into one static binary |
| `bonnie runs list` | show the durable runs in the tree's journal |
| `bonnie eval` | run the tree's evals and report the scores |

Each tree's `go.mod` pins a released BONNIE, so an example runs as a user
gets it. `go test ./examples/` in this repository also compiles every tree
against the current checkout and refuses a tree that drifts from the
`bonnie init` shape, so an example cannot go stale.

## Before you start

Install the CLI and set a model provider key. Kit reads the usual environment
variables:

```bash
go install github.com/mark3labs/bonnie/cmd/bonnie@latest
export ANTHROPIC_API_KEY=sk-ant-...
# or OPENAI_API_KEY, or GEMINI_API_KEY
```

## github-bot

An agent that lives on GitHub: mention it in an issue, a pull request, or a
review thread and it answers there, and the thread is a durable run.

```bash
cd examples/github-bot
bonnie dev --addr 127.0.0.1:8081 --tui=false
```

It needs a GitHub App and a public URL. See
[github-bot/README.md](github-bot/README.md) for how to make the App by hand.

## refund-desk

The refund assistant of a small shop, with evals that grade it. It is the
reference for `bonnie eval`: a dataset, run checks that read the journal,
decision and language-model judges, and an approval that survives a restart.
It needs no outside service.

```bash
cd examples/refund-desk
bonnie dev
bonnie eval --judge openrouter/typesafe/jev-1.14 --llm-judge anthropic/claude-sonnet-4-5
```

It needs a chat model key and judge models. See
[refund-desk/README.md](refund-desk/README.md).

## self-contained-agent

The customer FAQ assistant of a computer repair shop, as an agent that needs
nothing outside its own process: LFM2.5-2.6B, run on the CPU by Kronk, answers
from `context/faq.md`. It is the reference for a custom model provider, and it
needs no provider key and no GPU.

```bash
cd examples/self-contained-agent
bonnie dev
```

The first turn downloads the model (1 GB). It needs libffi on the host.
See [self-contained-agent/README.md](self-contained-agent/README.md).

## slack-bot

An agent that lives in Slack: mention it in a channel, reply in a thread it
joined, or DM it, and it answers there, and the conversation is a durable run.

```bash
cd examples/slack-bot
bonnie dev --addr 127.0.0.1:8081 --tui=false
```

It needs a Slack app and a public URL. See
[slack-bot/README.md](slack-bot/README.md) for how to make the app by hand.

## triage-desk

A support triage agent that routes each ticket in a sample inbox with a
decision model. Each routing is journalled and keyed on the ticket ID, so it
is the same after a restart. It needs no outside service.

```bash
cd examples/triage-desk
bonnie dev
```

It needs a chat model key and a decision model: `OPENROUTER_API_KEY`, or a
local Ollama model. See [triage-desk/README.md](triage-desk/README.md).

## Start your own from an example

An example is a tree, so you can copy it and start from there:

```bash
cp -r examples/slack-bot ~/my-bot
cd ~/my-bot
```

Or start from nothing with `bonnie init ~/my-bot` and copy in the
`bonnie.New(...)` options you want.

## Add an example

1. `bonnie init examples/<name>` in this repository.
2. Edit `instructions.md` and `main.go`. Keep the prompt in
   `instructions.md`: the guard refuses `WithSystemPrompt`,
   `WithInstructions`, `WithSkills`, and `WithContextFiles` in an example.
3. `cd examples/<name> && go mod tidy && bonnie build`, so `go.sum` and
   `bonnie_gen.go` are the files a user gets.
4. Write a `README.md` that runs it with `bonnie dev`, never with `go run`.
5. `go test ./examples/`.
