# BONNIE examples

Every example is a real program. `go build ./...` compiles them, so they cannot
go stale.

## Before you start

You need a model provider key. Kit reads the usual environment variables:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
# or
export OPENAI_API_KEY=sk-...
# or
export GEMINI_API_KEY=...
```

Pass `-model` to pick a model, for example
`-model anthropic/claude-sonnet-4-5`. Without it, Kit uses its configured
default.

Every example writes its journal to `.bonnie/journal.db`. That directory is
in `.gitignore`. Delete it to start again.

## minimal

One durable run, one answer.

```bash
go run ./examples/minimal -text "In one sentence, what is a durable agent run?"
```

The run is on disk when the program ends:

```bash
bonnie runs list --journal .bonnie
bonnie runs show --journal .bonnie minimal-1
```

Send a second message to the same run. BONNIE replays the conversation first,
so the agent remembers:

```bash
go run ./examples/minimal -text "What did I just ask you?"
```

## hitl-restart

The headline demonstration. The agent asks a question, the process **exits**,
and a new process finishes the run.

```bash
# Phase 1. The run parks and the process calls os.Exit.
go run ./examples/hitl-restart -phase ask
```

The run now holds no compute. Look at it:

```bash
bonnie runs list --journal .bonnie --state waiting
bonnie runs show --journal .bonnie hitl-1
```

Wait as long as you like. A minute or a week makes no difference.

```bash
# Phase 2. A new process. It shares only the journal.
go run ./examples/hitl-restart -phase answer -answer "eu-west-1"
```

The second command prints a completed run. Nothing was kept in memory between
the two commands.

## Over HTTP

The same run is reachable from outside the process:

```bash
go run ./cmd/bonnie serve --journal .bonnie
```

```bash
# Start a run.
curl -s localhost:8080/runs \
  -d '{"text":"Deploy the app. Ask me which region first."}'

# Watch it. The stream is newline-delimited JSON.
curl -sN localhost:8080/runs/<run-id>/stream

# Answer the question.
curl -s localhost:8080/runs/<run-id>/respond \
  -d '{"responses":[{"text":"eu-west-1"}]}'
```

Reconnect to a stream without a gap by passing the last sequence number you
saw:

```bash
curl -sN "localhost:8080/runs/<run-id>/stream?cursor=12"
```
