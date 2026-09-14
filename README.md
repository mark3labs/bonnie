<p align="center">
  <img src="logo.png" alt="BONNIE" width="180">
</p>

<h1 align="center">BONNIE</h1>

<p align="center">
  <b>Durable agent runs for Go.</b><br>
  Survive a crash. Wait days for a human. Answer over HTTP.
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/mark3labs/bonnie"><img src="https://pkg.go.dev/badge/github.com/mark3labs/bonnie.svg" alt="Go Reference"></a>
  <a href="https://go.dev/dl/"><img src="https://img.shields.io/badge/go-1.27%2B-00ADD8" alt="Go 1.27+"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="MIT"></a>
</p>

> [!WARNING]
> **Early and experimental. Use at your own risk.** BONNIE is pre-1.0
> software under active development. The API can change without notice,
> the durability and sandboxing claims are tested but not yet proven in
> production, and no release is suitable for workloads whose loss would
> hurt. Read [Limits](#limits) before you deploy anything with it.

---

An agent turn normally lives and dies with your process. Kill it mid-tool-call
and the work is gone. Ask the user a question and you have to hold the process
open until they answer.

BONNIE fixes that. It wraps the [Kit](https://github.com/mark3labs/kit) agent
SDK so a run becomes **durable**:

```go
run, _ := runner.Start(ctx, "deploy-42", runtime.Input{Text: "Deploy the app."})

if run.State == runtime.RunWaiting {
    fmt.Println(run.Suspend.Prompt) // "Which region?"
    os.Exit(0)                      // ← the process can end here
}
```

Come back tomorrow, in a different process, and finish it:

```go
run, _ := runner.Resume(ctx, "deploy-42",
    []runtime.InputResponse{{Text: "eu-west-1"}})

fmt.Println(run.Response) // "Deployed to eu-west-1."
```

The agent remembers the whole conversation, including which tools it already
called — so it does not repeat a side effect it has already performed.

## Contents

- [Install](#install) · [Quickstart: scaffold an agent](#quickstart-scaffold-an-agent) · [Quickstart](#quickstart) · [Park and resume](#park-and-resume)
- [Your own tools](#your-own-tools) · [Sandboxing](#sandboxing) · [Serve over HTTP](#serve-over-http) · [Chat channels](#chat-channels)
- [CLI](#cli) · [Storage](#storage) · [Streaming](#streaming) · [Steer and cancel](#steer-and-cancel)
- [How it works](#how-it-works) · [Limits](#limits) · [Docs](#documentation)

## Install

As a library:

```bash
go get github.com/mark3labs/bonnie
```

As a CLI:

```bash
go install github.com/mark3labs/bonnie/cmd/bonnie@latest
```

With Nix. This gives you the CLI with the microsandbox CLI (`msb`) already on
its PATH:

```bash
nix profile install github:mark3labs/bonnie   # or: nix run github:mark3labs/bonnie
```

Set a provider key. BONNIE uses whatever Kit is configured for:

```bash
export ANTHROPIC_API_KEY=sk-ant-...   # or OPENAI_API_KEY, or GEMINI_API_KEY
```

Requires Go 1.27+. Sandboxing is optional and needs Docker or `msb`.

### Development shell

The flake also gives you a shell with Go 1.27, `golangci-lint`, `goreleaser`,
and the microsandbox CLI:

```bash
nix develop
go test -race ./...
```

The repository ships an `.envrc`, so `direnv allow` enters the same shell on
`cd`.

Other flake outputs:

| Output | What it is |
|---|---|
| `packages.default`, `packages.bonnie` | the BONNIE CLI |
| `packages.microsandbox` | the `msb` CLI plus its `libkrunfw` |
| `apps.msb` | `nix run github:mark3labs/bonnie#msb` |
| `overlays.default` | both packages, for your own nixpkgs |

## Quickstart: scaffold an agent

Scaffold an agent, edit one file, run it.

```bash
bonnie init my-agent --model anthropic/claude-sonnet-4-5
cd my-agent
go mod tidy
# edit instructions.md — that file is the agent's system prompt
bonnie dev
```

The tree is four things: `main.go` (one call — this is where the model, the
sandbox, and the channels are configured, in code), `instructions.md` (the
system prompt, read fresh at every start), and `skills/` and `workspace/`
(seed directories — files under `workspace/` are mirrored into every run's
sandbox, and a file the model already wrote is never overwritten).

```go
// main.go — the whole default agent
package main

import "github.com/mark3labs/bonnie"

func main() {
	bonnie.Main(
		bonnie.WithModel("anthropic/claude-sonnet-4-5"),
	)
}
```

```bash
# talk to it over HTTP
curl -s localhost:8080/runs -d '{"text":"What are you?"}'

# or talk to it in the terminal — one durable conversation, live streamed
bonnie chat --addr :8080
```

There is no config file. A setting is either a file at a known path
(`instructions.md`, `workspace/`, `tools/`) or an option in `main.go`, so a
setting that does not exist is a compile error rather than a key nothing
reads. `-addr` and `-model` are operator flags on the built binary and win
over the options, so one binary can move port or model without a rebuild.

When you are ready to ship it, `bonnie build` compiles the tree — tools,
instructions, and seed files embedded — into one static binary that serves
on a host with no Go and no BONNIE install.

## Quickstart

A durable run in 20 lines. The journal on disk is what makes it durable.

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func main() {
	// Every message is journalled here before it is kept.
	journal, err := runtime.OpenFileJournal(".bonnie")
	if err != nil {
		log.Fatal(err)
	}
	defer journal.Close()

	runner := runtime.NewRunner(journal, runtime.KitAgent(
		kit.WithModel("anthropic/claude-sonnet-4-5"),
	))

	run, err := runner.Start(context.Background(), "run-1",
		runtime.Input{Text: "In one sentence, what is a durable agent run?"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(run.Response)
}
```

Run it again with a different message and the **same run ID**. BONNIE replays
the conversation first, so the agent remembers:

```go
run, _ := runner.Start(ctx, "run-1", runtime.Input{Text: "What did I just ask?"})
// `You asked me "In one sentence, what is a durable agent run?"`
```

Inspect what happened, without a server:

```bash
bonnie runs list --journal .bonnie
bonnie runs show --journal .bonnie run-1
```

```
RUN    STATE      STEPS  LAST
run-1  completed  2      You asked me "In one sentence, what is a durable agent run?"
```

## Park and resume

This is the headline feature. An agent asks a question, **your process exits**,
and a completely new process finishes the job.

BONNIE ships two tools for this. Register them and the model can call them:

| Tool | Parks the run to... |
|---|---|
| `ask_human` | ask the operator a question |
| `request_approval` | get approval before a risky action |

`runtime.KitAgent` registers both automatically.

```go
runner := runtime.NewRunner(journal, runtime.KitAgent(
	kit.WithModel("anthropic/claude-sonnet-4-5"),
	kit.WithSystemPrompt("Before you deploy anything, use ask_human to ask "+
		"which region to deploy to."),
))

run, err := runner.Start(ctx, "deploy-42", runtime.Input{Text: "Deploy the app."})
if err != nil {
	log.Fatal(err)
}

if run.State == runtime.RunWaiting {
	fmt.Println("agent asks:", run.Suspend.Prompt)
	return // nothing is holding compute — the process may exit
}
```

Later, anywhere, as long as it can read the same journal:

```go
run, err := runner.Resume(ctx, "deploy-42",
	[]runtime.InputResponse{{Text: "eu-west-1"}})
```

A parked run holds **no compute**. It costs nothing to wait a week.

See [`examples/hitl-restart`](examples/hitl-restart) for a runnable version
that genuinely calls `os.Exit` between the two phases:

```bash
go run ./examples/hitl-restart -phase ask
# ...process exits, run is parked on disk...
go run ./examples/hitl-restart -phase answer -answer "eu-west-1"
```

## Your own tools

A BONNIE tool is a Kit tool. Pass it through and it joins the sandboxed and
human-in-the-loop sets:

```go
type chargeInput struct {
	Amount int    `json:"amount" description:"Amount in cents."`
	UserID string `json:"user_id" description:"Who to charge."`
}

chargeCard := kit.NewTool("charge_card", "Charge a customer's card.",
	func(ctx context.Context, in chargeInput) (kit.ToolOutput, error) {
		// Runs in YOUR process, with your secrets. The model sees only
		// the result you return.
		if err := stripe.Charge(in.UserID, in.Amount); err != nil {
			return kit.ErrorResult(err.Error()), nil
		}
		return kit.TextResult("Charged."), nil
	})

runner := runtime.NewRunner(journal, runtime.KitAgent(
	kit.WithModel("anthropic/claude-sonnet-4-5"),
	kit.WithExtraTools(chargeCard),
))
```

You can also write a tool that **parks the run** — that is all `ask_human` is:

```go
return kit.ToolOutput{
	Content: "Waiting for the finance team.",
	Halt:    true,
	FinalValue: runtime.SuspendRequest{
		Kind:   "approval",
		Prompt: "Approve a $4,000 refund?",
	},
}, nil
```

The run stops, `Start` returns with `State == RunWaiting`, and
`run.Suspend.Prompt` carries your question.

Prefer scaffolding over hand-wiring? `bonnie init --tools` creates a tree
with one sample tool and a `main.go` you own; tools there live in
`tools/<name>/tool.go` as `func Tool() kit.Tool`, and the directory name is
the tool's name. `bonnie dev` and `bonnie build` regenerate the wiring, so
`main.go` never has to name a tool.

## Sandboxing

By default, tool calls run **as your process** — your files, your network, your
credentials. For anything untrusted, put them in a sandbox:

```go
provider := sandbox.Docker(sandbox.WithDockerImage("python:3.12-slim"))

runner := runtime.NewRunner(journal, sandbox.Agent(provider,
	kit.WithModel("anthropic/claude-sonnet-4-5"),
))
```

The model now gets `bash`, `read_file`, `write_file`, and `list_files` that
run inside a container rooted at `/workspace`.

| Backend | Isolation | You install | Extra Go deps |
|---|---|---|---|
| `sandbox.Local()` | **none** — dev only | — | 0 |
| `sandbox.Docker()` | container namespaces | Docker | 0 |
| `sandbox.Microsandbox()` | microVM, guest kernel | [`msb`](https://github.com/superradcompany/microsandbox) | 0 |

All three drive a CLI, so BONNIE stays a single static binary.

> The microsandbox adapter is **verified on Linux with KVM** (`msb` 0.6.18,
> all 18 conformance cases, network policies enforced with real egress). It
> has not been run on macOS with Apple Silicon, and its network policy is
> fixed at create time: reattaching under a different policy fails with
> `ErrPolicyMismatch` rather than silently using the old rules.

Lock down the network:

```go
provider := sandbox.Docker()
provider.SetNetworkPolicy(sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll})
```

Pick the best backend available, without silently falling back to no
isolation:

```go
provider, err := sandbox.Select(ctx, sandbox.Microsandbox(), sandbox.Docker())
```

The sandbox opens on the **first tool call that needs it**, so a parked run
holds no container. Read [`docs/SANDBOX.md`](docs/SANDBOX.md) before deploying.

## Serve over HTTP

```bash
bonnie serve --journal .bonnie --model anthropic/claude-sonnet-4-5 --sandbox docker
```

Or run an agent tree — its configuration is Go in its own `main.go`, so the
tree is served by running it. See [Quickstart: scaffold an
agent](#quickstart-scaffold-an-agent):

```bash
bonnie dev my-agent          # hot reload while you work on it
bonnie build my-agent        # one static binary, then run it anywhere
```

Or mount it in your own server:

```go
runner := runtime.NewRunner(journal, runtime.KitAgent(opts...))
http.ListenAndServe(":8080", bonniehttp.New(runner).Handler())
```

| Route | Does |
|---|---|
| `POST /runs` | start a run, or route to the one serving an address |
| `GET /runs/{id}` | report a run's durable state |
| `POST /runs/{id}` | send a message to an existing run |
| `POST /runs/{id}/respond` | answer a parked run |
| `POST /runs/{id}/cancel` | stop the turn in flight |
| `GET /runs/{id}/stream` | NDJSON event stream, resumable via `?cursor=` |

```bash
# Start a run. It parks on a question.
curl -s localhost:8080/runs -d '{"text":"Deploy the app. Ask me the region first."}'
# {"run_id":"run-e8b3fa...","state":"waiting",
#  "suspend":{"kind":"question","prompt":"Which region?"}}

# Answer it.
curl -s localhost:8080/runs/run-e8b3fa.../respond \
  -d '{"responses":[{"text":"eu-west-1"}]}'
# {"run_id":"run-e8b3fa...","state":"completed","response":"Deployed to eu-west-1."}
```

### Addresses

Chat platforms have threads, not run IDs. Pass an `address` and BONNIE keeps
the mapping **in the journal**, so a restart does not orphan a conversation:

```bash
curl -s localhost:8080/runs -d '{"address":"slack:C123/T456","text":"hi"}'
```

The same address always resolves to the same run. `POST /runs/{id}` is the
opposite: it targets one exact run and returns `404` rather than creating one.

## Chat channels

Slack, Discord, and Telegram put the same durable runs into a conversation.
Mount one in `main.go`, put its credentials in the environment, and run:

```go
bonnie.Main(
	bonnie.WithSlack(slack.Config{}),
	bonnie.WithDiscord(discord.Config{}),
	bonnie.WithTelegram(telegram.Config{Username: "mybot"}),
)
```

```bash
export SLACK_BOT_TOKEN=xoxb-... SLACK_SIGNING_SECRET=...
export DISCORD_BOT_TOKEN=... DISCORD_PUBLIC_KEY=...
export TELEGRAM_BOT_TOKEN=... TELEGRAM_WEBHOOK_SECRET=...
bonnie dev
```

Credentials are read from the environment and never from code; a missing one
is a startup error that names the variable.

Each channel mounts one webhook (`/slack/events`, `/discord/interactions`,
`/telegram`), verifies its platform's signature — Slack's v0 HMAC, Discord's
Ed25519, Telegram's shared secret — and answers within the platform's ACK
deadline while the turn runs on. The reply posts back to the thread; a
parked run posts its question, and the next message on the thread is the
answer.

The details are in [`docs/CHANNELS.md`](docs/CHANNELS.md): the per-platform
setup, the dispatch and steering rules, and what is deliberately not
implemented (streaming edits, button HITL, attachments, gateway
transports).

## CLI

```
bonnie init     Scaffold an agent tree (main.go, instructions, seeds)
bonnie dev      Run an agent tree with hot reload and the built-in TUI
bonnie build    Compile an agent tree into one static binary
bonnie serve    Mount the HTTP channel and serve durable runs, with no tree
bonnie chat     Talk to a running agent in a terminal
bonnie runs     List and inspect durable runs
bonnie sandbox  Reclaim the sandboxes of terminal runs (prune)
bonnie version  Print the version
```

```bash
bonnie init my-agent --model anthropic/claude-sonnet-4-5
bonnie init .                      adopt this directory; never overwrites
bonnie init my-agent --tools       add a sample Go tool

bonnie dev my-agent                hot reload + the terminal interface
bonnie build my-agent              one static binary at ./my-agent
bonnie chat --addr :8080           talk to any running channel in a terminal

bonnie serve --addr :8080 --journal .bonnie \
             --model anthropic/claude-sonnet-4-5 \
             --sandbox docker --sandbox-deny-network

bonnie runs list --journal .bonnie --state waiting
bonnie runs show --journal .bonnie run-1
bonnie runs show --journal .bonnie run-1 --json | jq '.[] | select(.kind=="message")'
```

`runs` reads the journal directly, so it works while the server is stopped —
which is exactly when you need it.

## Storage

The journal is the durability seam. Two ship in the box:

```go
runtime.NewMemoryJournal()          // tests and ephemeral runs
runtime.OpenFileJournal(".bonnie")  // JSONL, one file per run
```

`FileJournal` writes `<root>/runs/<run-id>.jsonl`, one JSON object per line,
fsynced by default. It is plain text, so `jq` works on it:

```bash
jq -c 'select(.kind=="message") | {seq, role}' .bonnie/runs/run-1.jsonl
```

Bring your own by implementing seven methods:

```go
type Journal interface {
	Append(ctx context.Context, rec Record) (seq int, err error)
	Replay(ctx context.Context, runID string) ([]Record, error)
	Checkpoint(ctx context.Context, runID string, state RunState) error
	State(ctx context.Context, runID string) (RunState, error)
	Runs(ctx context.Context, state RunState) ([]string, error)
	Persisted() bool
	Close() error
}
```

A table-driven conformance suite in `runtime/journal_conformance_test.go` runs
against every implementation. Add yours to it and it inherits the whole suite.

## Streaming

Subscribe to a run's events in-process:

```go
events, unsubscribe := runner.Events().Subscribe("run-1", 0)
defer unsubscribe()

for ev := range events {
	fmt.Println(ev.Seq, ev.Type, ev.Text)
}
```

Or over HTTP, one JSON object per line:

```bash
curl -sN localhost:8080/runs/run-1/stream
```

Every event carries a monotonic `seq`. If a client drops, reconnect with the
last one it saw and lose nothing:

```bash
curl -sN "localhost:8080/runs/run-1/stream?cursor=12"
```

## Steer and cancel

```go
runner.Steer("run-1", "actually, use eu-west-1")  // joins the running turn
runner.Cancel("run-1")                            // stops it
```

Cancelling keeps every completed step, so the run restores to a valid
conversation and can be continued with `Start`.

## Run states

```
pending → running → completed
                  → waiting    (parked for a human; resume with Resume)
                  → cancelled  (stopped by an operator; continue with Start)
                  → failed
```

## How it works

BONNIE is built on four public Kit extension points. No fork, no patched SDK:

| Need | Kit API |
|---|---|
| Journal every message | `Options.SessionManager` |
| Checkpoint each step | `Kit.OnStepFinish` |
| Inject replayed context | `Kit.OnContextPrepare` |
| Park for a human | `kit.ToolOutput{Halt, FinalValue}` |

Three properties are worth knowing, because they are the difference between a
demo and something you can deploy:

- **Replay is lossless.** Journalled messages keep their typed parts, so a
  resumed run knows which tools it called and what came back. It will not
  repeat a side effect it already performed.
- **A step commits atomically.** A tool call and its result reach the journal
  as one write and one fsync (`kit.StepAppender`, adopted from Kit `v0.106.0`),
  so a crash cannot leave an unanswered tool call. If a torn step still
  reaches disk — from an older journal, a non-batching journal, or a short
  write — restore drops that incomplete step and records the repair.
- **Cancelling keeps finished work.** Steps are persisted before the context is
  checked, so a cancelled turn loses only the step in flight.

Full detail, with the Kit citations, in [`docs/SPEC.md`](docs/SPEC.md).

## Limits

Stated plainly, because the failure modes are not obvious:

- **Sandboxing is opt-in.** Without it, tool calls run as your process.
- **Docker is namespaces, not a kernel.** Use microsandbox for hostile code.
- **microsandbox is verified on Linux/KVM only** — not on macOS with Apple
  Silicon. Every network policy mode is enforced, but the policy is fixed at
  create time; reattaching under a different policy fails with
  `ErrPolicyMismatch`.
- **Sandbox egress is open** unless you set a policy.
- **No auth verification on the HTTP channel.** It carries a `Principal`; it
  does not check one. Authenticate in front of it. The chat channels are
  different: each verifies its platform's signature, and a channel without
  its credentials refuses to serve. That verifies the platform, not the
  person — a user ID inside a verified Slack event is Slack's word.
- **Run ownership is per host.** The file journal locks each run with
  `flock`, so a second writer to the same run is refused rather than allowed
  to corrupt it. That lock does not work on a network filesystem, and it does
  not make two writers coordinate — one process still owns each run.
- **Events are journal-anchored.** The stream replays the journal past the
  in-memory backlog, so a reconnect — even after a restart — has no gap.
  Live-only deltas are the exception, marked as such.
- **Sandbox lifecycle is journalled, and reclaiming is manual.**
  `bonnie sandbox prune` deletes the sandboxes of terminal runs; `serve`
  does not sweep them on its own yet.
- **The mark3labs modules are publicly fetchable.** A scaffolded module runs
  `go mod tidy` and resolves `bonnie` and `kit` from the proxy; no `go.work`,
  no `GOPRIVATE`. Authoring an agent needs Go on your machine; the binary
  `bonnie build` produces needs nothing on the host.

## Examples

| Example | Shows |
|---|---|
| [`examples/minimal`](examples/minimal) | one durable run, start to finish |
| [`examples/hitl-restart`](examples/hitl-restart) | park, **exit the process**, resume |

```bash
go run ./examples/minimal -text "What is a durable agent run?"
```

See [`examples/README.md`](examples/README.md) for copy-pasteable commands.

## Documentation

| Document | Purpose |
|---|---|
| [`docs/HANDOVER.md`](docs/HANDOVER.md) | Picking up the project: state, pitfalls, what to do next |
| [`docs/SANDBOX.md`](docs/SANDBOX.md) | Sandbox backends and the contracts an adapter must honour |
| [`docs/SPEC.md`](docs/SPEC.md) | Specification: scope, verified Kit facts, known risks, invariants |
| [`docs/L2.md`](docs/L2.md) | The agent tree: the default layout, configuration as code, `init`/`dev`/`build`, and the codegen contract |
| [`docs/TASKS.md`](docs/TASKS.md) | Open work, and an archive of what shipped |
| [`docs/UPSTREAM.md`](docs/UPSTREAM.md) | The home for BONNIE's future asks of Kit; answered ones live in `docs/archive/` |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Boundary rule, workspace setup, commands |
| [`SECURITY.md`](SECURITY.md) | Disclosure, and what v0.1.0 does not protect you from |

## Contributing

```bash
go build ./...
go test -race ./...
golangci-lint run
```

Or run the same loop with `task check`, and CI parity with `task ci`.

The live-model tests are behind a build tag and need a provider key:

```bash
go test -race -tags integration ./runtime ./sandbox
```

They skip, never fail, when no key is present. One rule matters above the
rest: **BONNIE uses the public Kit SDK only**. See
[`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

MIT — see [LICENSE](LICENSE).
