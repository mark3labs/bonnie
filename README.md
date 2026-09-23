<p align="center">
  <img src="logo.png" alt="BONNIE" width="180">
</p>

<h1 align="center">BONNIE</h1>

<p align="center">
  <b>Durable agent runs for Go.</b><br>
  Survive a crash. Wait days for a human. Answer over HTTP, Slack, Discord, Telegram, or GitHub.
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/mark3labs/bonnie"><img src="https://pkg.go.dev/badge/github.com/mark3labs/bonnie.svg" alt="Go Reference"></a>
  <a href="https://go.dev/dl/"><img src="https://img.shields.io/badge/go-1.27%2B-00ADD8" alt="Go 1.27+"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="MIT"></a>
</p>

> [!WARNING]
> **Early and experimental. Use at your own risk.** BONNIE is pre-1.0
> software under active development. The API can change without notice. The
> durability and the sandbox claims have tests, but no release is proven in
> production. Do not use a release for work whose loss would hurt. Read
> [Limits](#limits) before you deploy.

---

An agent turn usually lives and dies with the process. Stop the process during
a tool call and the work is gone. Ask the user a question and you must hold the
process open until the user answers.

BONNIE corrects that. It wraps the [Kit](https://github.com/mark3labs/kit)
agent SDK, and a run becomes **durable**:

```go
run, _ := runner.Start(ctx, "deploy-42", runtime.Input{Text: "Deploy the app."})

if run.State == runtime.RunWaiting {
    fmt.Println(run.Suspend.Prompt) // "Which region?"
    os.Exit(0)                      // ← the process can stop here
}
```

Come back tomorrow, in a different process, and complete the run:

```go
run, _ := runner.Resume(ctx, "deploy-42",
    []runtime.InputResponse{{Text: "eu-west-1"}})

fmt.Println(run.Response) // "Deployed to eu-west-1."
```

The agent keeps the full conversation, and the tools it already called. Thus it
does not do a side effect a second time.

## Contents

- [Install](#install) · [Scaffold an agent](#scaffold-an-agent) · [Use the library](#use-the-library) · [Park and resume](#park-and-resume)
- [Your own tools](#your-own-tools) · [Sandboxes](#sandboxes) · [HTTP API](#http-api) · [Chat channels](#chat-channels)
- [CLI](#cli) · [Journal](#journal) · [Events](#events) · [Session controls](#session-controls)
- [Run states](#run-states) · [How it works](#how-it-works) · [Limits](#limits) · [Docs](#documentation)

## Install

As a library:

```bash
go get github.com/mark3labs/bonnie
```

As a CLI, with the install script. It downloads the release binary for your
platform, verifies its SHA-256 checksum, and installs it. The default
directory is `~/.local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/mark3labs/bonnie/master/install.sh | bash
```

The script refuses a binary that it cannot verify. To pin a release or to
choose the directory, give the options after `bash -s --`:

```bash
curl -fsSL https://raw.githubusercontent.com/mark3labs/bonnie/master/install.sh \
  | bash -s -- --version v0.7.0 --bin-dir /usr/local/bin
```

The script also tells you when the host has no Landlock, no provider key, or
no Go. It does not change the host for these.

As a CLI, from source:

```bash
go install github.com/mark3labs/bonnie/cmd/bonnie@latest
```

With Nix. This gives you the CLI, and the microsandbox CLI (`msb`) on its PATH:

```bash
nix profile add github:mark3labs/bonnie   # or: nix run github:mark3labs/bonnie
```

Set a provider key. BONNIE uses the provider that Kit is configured for:

```bash
export ANTHROPIC_API_KEY=sk-ant-...   # or OPENAI_API_KEY, or GEMINI_API_KEY
```

BONNIE also reads a `.env` in the working directory when one is there, so you
can write the key (and any channel credential) into a file instead of
exporting it each time. An exported variable still wins over the file, and a
missing `.env` is not an error.

BONNIE needs **Linux**. To author an agent also needs Go 1.27+; the release
binary does not. The kernel must be 5.13 or newer with Landlock enabled, which
is the default on each current distribution. A sandbox is not optional, and
the default sandbox needs no installation. Docker or `msb` give stronger
isolation. macOS and Windows are not supported — see [Limits](#limits).

### Development shell

The flake also gives you a shell with Go 1.27, `golangci-lint`, `goreleaser`,
and the microsandbox CLI:

```bash
nix develop
go test -race ./...
```

The repository has an `.envrc`, thus `direnv allow` opens the same shell when
you `cd` into it.

| Flake output | What it is |
|---|---|
| `packages.default`, `packages.bonnie` | the BONNIE CLI |
| `packages.microsandbox` | the `msb` CLI and its `libkrunfw` |
| `apps.msb` | `nix run github:mark3labs/bonnie#msb` |
| `overlays.default` | both packages, for your own nixpkgs |

## Scaffold an agent

Scaffold an agent, edit one file, then run it.

```bash
bonnie init my-agent --model anthropic/claude-sonnet-4-5
cd my-agent
go mod tidy
# edit instructions.md — that file is the agent's system prompt
bonnie dev
```

`bonnie init` writes `instructions.md` (the system prompt), `main.go` (the one
call you own), `bonnie_gen.go` (the generated wiring), `go.mod`, and the seed
directories `skills/` and `workspace/`. With `--tools` it also writes a sample
tool at `tools/echo/tool.go`. It never replaces a file: if one file exists, it
refuses, names each file it found, and changes nothing.

```go
// main.go — the full default agent
package main

import "github.com/mark3labs/bonnie"

func main() {
	bonnie.New(
		bonnie.WithModel("anthropic/claude-sonnet-4-5"),
	).Serve()
}
```

```bash
# speak to it over HTTP
curl -s localhost:8080/bonnie/v1/runs -d '{"text":"What are you?"}'

# or speak to it in the terminal — one durable conversation, streamed live
bonnie chat --addr 127.0.0.1:8080
```

Files under `workspace/` are copied into each run's sandbox. A file that the
model already wrote is never replaced.

Files under `skills/` are the agent's skills: one `*.md` per skill, or one
subdirectory per skill with a `SKILL.md` in it, each with YAML frontmatter
that gives a `name` and a `description`. Those two fields go in the system
prompt; the body arrives only when the model calls `activate_skill`, so a
large skill set costs few tokens until it is used. The tree's `skills/` is the
whole set — an agent never inherits a skill from a `.agents/skills` directory
it happens to run beside.

There is no configuration file. A setting is a file at a known path
(`instructions.md`, `workspace/`, `skills/`, `tools/`) or an option in
`main.go`. Thus a setting that does not exist is a compile error, and not a key
that nothing reads. The built binary accepts two operator flags, `-addr` and
`-model`. Each flag wins over the related option, thus one binary can change
port or model without a new build.

When the agent is ready, `bonnie build` compiles the tree into one static
binary. The binary contains the tools, the instructions, the skills, and the
seed files. It serves on a host that has no Go and no BONNIE installation.

## Use the library

A durable run in 25 lines. The journal on disk is what makes the run durable.

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func main() {
	// Each message is journalled here before it is kept.
	journal, err := runtime.OpenSQLiteJournal(".bonnie")
	if err != nil {
		log.Fatal(err)
	}
	defer journal.Close()

	// sandbox.Agent gives the model a shell and a filesystem that are not
	// the host's. It also registers the human-in-the-loop tools.
	runner := runtime.NewRunner(journal, sandbox.Agent(
		sandbox.Landlock(),
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

> **`runtime.KitAgent` is the unsandboxed seam.** It builds a Kit agent whose
> core tools — shell, read, write, edit — run **in your process**, with your
> files and your credentials. `bonnie.New()` never uses it directly: it wraps
> it in `sandbox.Agent`. Call `runtime.KitAgent` only when your process is
> already inside isolation that you control. A working directory is not
> isolation: an absolute path leaves it.

Start the program again with a different message and the **same run ID**.
BONNIE replays the conversation first, thus the agent remembers:

```go
run, _ := runner.Start(ctx, "run-1", runtime.Input{Text: "What did I just ask?"})
// `You asked me "In one sentence, what is a durable agent run?"`
```

`runtime.Input` carries more than text:

| Field | Function |
|---|---|
| `Text` | the user's message — the one part that becomes conversation history |
| `Files` | file parts for the turn (`kit.LLMFilePart`) |
| `Context` | what the model must know for this turn only; shown before `Text`, never history |
| `Title` | names the run in operator listings; recorded on the first turn |
| `Origin` | where the conversation lives (`Channel`, `Kind`); recorded on the first turn |

Examine what occurred, with no server:

```bash
bonnie runs list --journal .bonnie
bonnie runs show --journal .bonnie run-1
```

```
RUN    TITLE              STATE      STEPS  LAST
run-1  What is a durab…   completed  2      You asked me "In one sentence, …"
```

## Park and resume

This is the primary function. An agent asks a question, **your process stops**,
and a new process completes the work.

BONNIE has two tools for this. The model can call them:

| Tool | Parks the run to... |
|---|---|
| `ask_human` | ask the operator a question |
| `request_approval` | get approval before a dangerous action |

`runtime.KitAgent` registers both, thus `sandbox.Agent` and `bonnie.New()`
register them too.

```go
runner := runtime.NewRunner(journal, sandbox.Agent(
	sandbox.Landlock(),
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
	return // no compute is held — the process can stop
}
```

Later, in any process that can read the same journal:

```go
run, err := runner.Resume(ctx, "deploy-42",
	[]runtime.InputResponse{{Text: "eu-west-1"}})
```

A parked run holds **no compute**. To wait one week costs nothing. The
[`examples/github-bot`](examples/github-bot) agent does this for real: it asks
a question in a comment, the process can stop, and a later delivery resumes
the run from the journal.

## Your own tools

A BONNIE tool is a Kit tool. Give it to the agent and it joins the sandboxed
set and the human-in-the-loop set:

```go
type chargeInput struct {
	Amount int    `json:"amount" description:"Amount in cents."`
	UserID string `json:"user_id" description:"Who to charge."`
}

chargeCard := kit.NewTool("charge_card", "Charge a customer's card.",
	func(ctx context.Context, in chargeInput) (kit.ToolOutput, error) {
		// This runs in YOUR process, with your secrets. The model sees
		// only the result you return.
		if err := stripe.Charge(in.UserID, in.Amount); err != nil {
			return kit.ErrorResult(err.Error()), nil
		}
		return kit.TextResult("Charged."), nil
	})

// In an agent tree:
bonnie.New(bonnie.WithTools(chargeCard)).Serve()

// Or one layer down:
runner := runtime.NewRunner(journal, sandbox.Agent(
	sandbox.Landlock(),
	kit.WithModel("anthropic/claude-sonnet-4-5"),
	kit.WithExtraTools(chargeCard),
))
```

You can also write a tool that **parks the run**. `ask_human` is exactly this:

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
`run.Suspend.Prompt` holds your question.

In an agent tree, `bonnie init --tools` writes one sample tool. A tool there is
`tools/<name>/tool.go` with `func Tool() kit.Tool`, and the directory name is
the tool's name. `bonnie dev` and `bonnie build` generate the wiring again,
thus `main.go` never names a tool.

## Sandboxes

Each tool call runs in a sandbox. There is no unsandboxed mode. `WithSandbox`
**selects** a backend, it does not enable one. If you do not call it, you get
`sandbox.Landlock()`, and not your process.

The default confines tool calls to the run's own workspace with the Linux
Landlock LSM, and needs no installation. That is why it is the floor: a default
that needs Docker is a default that people switch off.

```go
bonnie.New().Serve() // already sandboxed
```

Select a stronger backend when the work is not trusted. `bonnie init` writes
both lines as comments, thus the choice is visible:

```go
bonnie.New(
	bonnie.WithSandbox(sandbox.Docker(sandbox.WithDockerImage("python:3.12-slim"))),
	bonnie.WithNetwork(sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}),
).Serve()
```

If you wire the runner yourself, it is the same provider one layer down:

```go
provider := sandbox.Docker(sandbox.WithDockerImage("python:3.12-slim"))

runner := runtime.NewRunner(journal, sandbox.Agent(provider,
	kit.WithModel("anthropic/claude-sonnet-4-5"),
))
```

The model gets four tools that run in the sandbox: `bash`, `read_file`,
`write_file`, and `list_files`. Their root is `sandbox.Workspace`,
`/workspace`. A path that leaves the workspace, also through a symlink the
model made, gets `sandbox.ErrOutsideWorkspace`.

| Backend | Isolation | You install | Network policy |
|---|---|---|---|
| `sandbox.Landlock()` — **default** | filesystem **containment**, shared kernel | — | none: a policy is refused |
| `sandbox.Local()` | **none** — development only | — | none: a policy is refused |
| `sandbox.Docker()` | container namespaces | Docker | `allow-all`, `deny-all` |
| `sandbox.Microsandbox()` | microVM, guest kernel | [`msb`](https://github.com/superradcompany/microsandbox) | `allow-all`, `deny-all`, `allow-list` |

The Docker and microsandbox backends drive a CLI, and Landlock is pure Go.
Thus BONNIE stays one static binary.

> **The default is containment, not isolation.** Landlock confines the
> filesystem and keeps the host environment away from a command. Thus a model
> cannot read your journal or your API keys. It does **not** confine the
> network, and it does **not** give the command its own kernel. For hostile
> code, use Docker or microsandbox, and stop egress.

> Each Landlock command runs in a child process that re-executes BONNIE's
> binary, restricts itself, and then becomes the command. The restriction stays
> after `execve` and is inherited, thus a subshell cannot escape it.

> The microsandbox adapter is **verified on Linux with KVM** (`msb` 0.6.18, all
> 18 conformance cases, network policies enforced with real egress). Its
> network policy is fixed when the sandbox is made. To attach again with a
> different policy fails with `ErrPolicyMismatch`, and does not use the old
> rules in silence.

Constrain the network. A backend that cannot enforce a policy refuses it with
`ErrPolicyUnsupported`. It never permits everything in silence:

```go
provider := sandbox.Docker()
provider.SetNetworkPolicy(sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll})
```

Select the best backend that is available, with no silent fall-back to no
isolation:

```go
provider, err := sandbox.Select(ctx, sandbox.Microsandbox(), sandbox.Docker(), sandbox.Landlock())
```

`sandbox.Seeded(provider, dir)` copies a local directory into each sandbox, and
never replaces a file that the run already has. An agent tree wraps its
`workspace/` directory this way.

The sandbox opens at the **first tool call that needs it**, thus a parked run
holds no container. Read the godoc of each provider in
[package `sandbox`](https://pkg.go.dev/github.com/mark3labs/bonnie/sandbox)
before you deploy. Each provider states what it contains and what it does not.

## HTTP API

```bash
bonnie serve --journal .bonnie --model anthropic/claude-sonnet-4-5 --sandbox docker
```

Or run an agent tree. Its configuration is Go in its own `main.go`, thus you
serve the tree when you run it:

```bash
bonnie dev my-agent          # hot reload while you work
bonnie build my-agent        # one static binary, then run it anywhere
```

Or mount the channel in your own server. A channel gives you its routes, and
it is also the inbound surface that a handler resolves an address through:

```go
runner := runtime.NewRunner(journal, sandbox.Agent(provider, opts...))
ch := bonniehttp.New(runner)

mux := http.NewServeMux()
for _, rt := range ch.Routes() {
	handler := rt.Handler
	mux.HandleFunc(rt.Method+" "+rt.Path, func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, ch, nil) // nil: no other channel to hand off to
	})
}
http.ListenAndServe(":8080", mux)
```

Each route is under `/bonnie/v1`. The version segment is the wire contract.

| Route | Function |
|---|---|
| `GET /bonnie/v1/health` | liveness: `{"ok":true,"status":"ready"}`, no journal read |
| `GET /bonnie/v1/info` | agent name, BONNIE version, mounted channels |
| `POST /bonnie/v1/runs` | start a run, or route to the run that serves an address |
| `GET /bonnie/v1/addresses/{address}` | report the run that an address resolves to |
| `POST /bonnie/v1/addresses/{address}` | send to the run that an address resolves to |
| `GET /bonnie/v1/runs/{id}` | report a run's durable state |
| `POST /bonnie/v1/runs/{id}` | send a message to one exact run |
| `POST /bonnie/v1/runs/{id}/respond` | answer a parked run |
| `POST /bonnie/v1/runs/{id}/cancel` | stop the turn in progress |
| `POST /bonnie/v1/runs/{id}/reset` | retire the run and free its address |
| `POST /bonnie/v1/runs/{id}/clear` | drop the conversation, keep the run |
| `POST /bonnie/v1/runs/{id}/compact` | summarise the older messages now |
| `GET /bonnie/v1/runs/{id}/stream` | NDJSON event stream, resumable with `?cursor=` |

```bash
# Start a run. It parks on a question.
curl -s localhost:8080/bonnie/v1/runs -d '{"text":"Deploy the app. Ask me the region first."}'
# {"run_id":"run-e8b3fa...","cursor":4,"state":"waiting",
#  "suspend":{"kind":"question","prompt":"Which region?"}}

# Answer it.
curl -s localhost:8080/bonnie/v1/runs/run-e8b3fa.../respond \
  -d '{"responses":[{"text":"eu-west-1"}]}'
# {"run_id":"run-e8b3fa...","state":"completed","response":"Deployed to eu-west-1."}
```

Each error reply has a message and a stable code:
`{"error":"...","code":"run_not_found"}`. The codes include `run_not_found`,
`invalid_run_id`, `run_not_waiting`, `run_active`, `run_retired`,
`unknown_turn_policy`, `bad_request`, `too_large`, and `internal`. A client can
branch on the code, and not on the text.

`POST /bonnie/v1/runs` accepts `operation_id` as an idempotency key, and
`turn_policy` to select what occurs when a turn is already running: `steer`
(the default) injects the message into the turn, `queue` lets the turn finish
first.

### Addresses

Chat platforms have threads, not run IDs. Send an `address`, and BONNIE keeps
the mapping **in the journal**. Thus a restart does not orphan a conversation:

```bash
curl -s localhost:8080/bonnie/v1/runs -d '{"address":"session-42","text":"hi"}'
```

The same address always resolves to the same run.
`POST /bonnie/v1/runs/{id}` is the opposite: it targets one exact run, and
returns `404` instead of making a run.

## Chat channels

Slack, Discord, Telegram, and GitHub put the same durable runs into a
conversation. Mount a channel in `main.go`, put its credentials in the
environment, and run:

```go
bonnie.New(
	bonnie.WithSlack(slack.Config{}),
	bonnie.WithDiscord(discord.Config{}),
	bonnie.WithTelegram(telegram.Config{Username: "mybot"}),
	bonnie.WithGitHub(github.Config{BotName: "my-agent"}),
).Serve()
```

```bash
export SLACK_BOT_TOKEN=xoxb-... SLACK_SIGNING_SECRET=...
export DISCORD_BOT_TOKEN=... DISCORD_PUBLIC_KEY=...
export TELEGRAM_BOT_TOKEN=... TELEGRAM_WEBHOOK_SECRET=...
export GITHUB_APP_ID=... GITHUB_APP_PRIVATE_KEY=... GITHUB_WEBHOOK_SECRET=...
bonnie dev
```

Credentials come from the environment and never from code. A missing
credential is a startup error that names the variable. The GitHub bot name is
a setting and not a secret, thus it stays in the config.

| Channel | Webhook route | Verification | Address |
|---|---|---|---|
| `slack` | `POST /slack/events` | v0 HMAC signature | `<channel>/<thread_ts>`, or `<channel>` for a DM |
| `discord` | `POST /discord/interactions` | Ed25519 signature | the channel or thread ID |
| `telegram` | `POST /telegram` | shared secret header | `<chat_id>`, or `<chat_id>/<topic>` |
| `github` | `POST /github/events` | HMAC signature | `<owner>/<repo>/issues/<n>`, or `<owner>/<repo>/pulls/<n>/reviews/<id>` |

An address is channel-local. The framework puts the channel's name in front of
it, thus the durable form is `slack/C123/1700000000.000900`.

Each channel answers in the platform's ACK period while the turn continues. The
reply goes back to the thread. A parked run posts its question, and the next
message on the thread is the answer. `/new` in a thread retires the run and
starts a new conversation in the same place.

The GitHub channel is a GitHub App. A comment that names `@<BotName>` on an
issue, a pull request, or a review thread starts or continues a run. The
channel mints an installation token for each event, uses it only to post back
and to read the pull request's changed files, and never lets the token enter
the run. A review thread is its own conversation, separate from the pull
request's timeline. The hooks `OnIssue`, `OnPullRequest`, and `OnCheckSuite`
let the host start a turn from an event with no comment.

While a turn runs, the Slack channel shows what the agent is doing. The mode is
`slack.Config.Activity`: `ActivityMessage` (the default) edits one placeholder
message, `ActivityStatus` uses Slack's assistant status and needs the
`assistant:write` scope, and `ActivityOff` shows nothing until the reply.

A mounted channel can also start a conversation on another channel. A route
handler gets `channel.Outbound` beside `channel.Inbound`:

```go
to, ok := out.To("slack")
if ok {
	err := to.Receive(ctx, "C0123456789", "the nightly digest, please",
		channel.SendOptions{Auth: principal})
}
```

This is an agent hand-off and not a notification: the text becomes turn input,
and the model runs on the destination channel. The destination binds the
address before the turn runs, thus a platform event that arrives during the
turn continues this run.

The per-platform setup, the dispatch rules, and what is deliberately not
implemented are in the godoc of
[package `channel`](https://pkg.go.dev/github.com/mark3labs/bonnie/channel)
and each adapter below it.

## CLI

```
bonnie init     Scaffold an agent tree
bonnie dev      Run an agent tree with hot reload and the built-in TUI
bonnie build    Compile an agent tree into one static binary
bonnie serve    Mount the HTTP channel and serve durable runs, with no tree
bonnie chat     Interact with an agent over the HTTP channel in a terminal
bonnie runs     List and inspect durable runs
bonnie sandbox  Reclaim the sandboxes of finished runs
bonnie version  Print the BONNIE version
```

```bash
bonnie init my-agent --model anthropic/claude-sonnet-4-5
bonnie init .                      adopt this directory; never replaces a file
bonnie init my-agent --tools       add a sample Go tool

bonnie dev my-agent                hot reload and the terminal interface
bonnie dev my-agent --tui=false    the serve loop alone, for CI
bonnie dev my-agent --dry-run      print the discovery plan, build nothing

bonnie build my-agent              one static binary at ./my-agent
bonnie build my-agent --output bin/agent

bonnie chat --addr 127.0.0.1:8080 --run tui-default

bonnie serve --addr :8080 --journal .bonnie \
             --model anthropic/claude-sonnet-4-5 \
             --sandbox docker --sandbox-deny-network \
             --slack --discord --telegram

bonnie runs list --journal .bonnie --state waiting
bonnie runs show --journal .bonnie run-1
bonnie runs show --journal .bonnie run-1 --json | jq '.[] | select(.kind=="message")'

bonnie sandbox prune --journal .bonnie --sandbox docker --dry-run
```

`--sandbox` accepts `landlock` (the default), `docker`, `microsandbox`,
`local`, or `auto`. `none` is refused by name, because it was the old default
and still lives in scripts.

`bonnie serve` has no `--github` flag: the GitHub channel needs a bot name,
which is a setting in code and not an environment variable. Mount it from an
agent tree with `bonnie.WithGitHub`.

`runs` reads the journal directly, thus it works while the server is stopped —
which is when you need it.

## Journal

The journal is the durability seam. Two implementations are in the box:

```go
runtime.NewMemoryJournal()            // tests and ephemeral runs
runtime.OpenSQLiteJournal(".bonnie")  // SQLite, one database for each run
```

`SQLiteJournal` writes `<root>/journal.db` through a **pure-Go** driver
(`modernc.org/sqlite`). Thus BONNIE still builds and cross-compiles with
`CGO_ENABLED=0`, and `bonnie build` still makes one static binary. The database
uses WAL mode and fsyncs each commit. `runtime.WithFsync(runtime.FsyncRelaxed)`
trades a bounded loss window for throughput.

One record for each row, thus any SQLite client can read a run:

```bash
sqlite3 .bonnie/journal.db \
  "SELECT seq, role, text FROM records WHERE run_id = 'run-1' ORDER BY seq"
```

What the database gives, against the JSONL files that it replaced:

- **A step is atomic.** A tool-calling step is one transaction. Thus the torn
  single write that a file append permitted is closed, and not only smaller.
- **Concurrent writers are safe, and not refused.** SQLite serialises write
  transactions across processes, and the `(run_id, seq)` primary key makes a
  reused sequence number a constraint violation. The per-run lock file, and the
  `ErrRunOwnedElsewhere` that it caused, are gone.

**Upgrade.** A `.bonnie` directory that still has `runs/*.jsonl` from an
earlier BONNIE is imported at the first open. Records keep their sequence
numbers, and each source file is renamed to `<run>.jsonl.imported` and not
deleted. The import is idempotent, thus a crash in the middle costs one more
read.

Supply your own journal with seven methods:

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

The table-driven conformance suite in `runtime/journal_conformance_test.go`
runs against each implementation. Add yours, and it gets the full suite.

## Events

Subscribe to a run's events in the process:

```go
events, unsubscribe := runner.Events().Subscribe("run-1", 0)
defer unsubscribe()

for ev := range events {
	fmt.Println(ev.Seq, ev.Type, ev.Text)
}
```

The types are `run_state`, `run_suspend`, `run_resume`, and `run_response`, and
the live deltas that Kit forwards during a turn.

Or over HTTP, one JSON object for each line:

```bash
curl -sN localhost:8080/bonnie/v1/runs/run-1/stream
```

Each event has a monotonic `seq`. If a client disconnects, connect again with
the last `seq` it saw, and lose nothing:

```bash
curl -sN "localhost:8080/bonnie/v1/runs/run-1/stream?cursor=12"
```

Durable events are anchored to the journal record that caused them. Thus a
cursor keeps its meaning after a restart. Live-only deltas are never replayed.

## Session controls

```go
runner.Steer("run-1", "actually, use eu-west-1")   // joins the turn in progress
runner.Cancel("run-1")                             // stops the turn
runner.Clear(ctx, "run-1")                         // forgets the conversation
runner.Compact(ctx, "run-1")                       // summarises the older messages
runner.Retire(ctx, "run-1", "user asked for a new conversation")
```

To cancel keeps each completed step, thus the run restores to a valid
conversation and continues with `Start`. To retire is permanent: `Start` and
`Resume` then refuse the run with `ErrRunRetired`, and the run stays readable.

`runner.Snapshot(ctx, runID)` reports a run without a turn.
`runner.IsActive(runID)` reports whether a turn runs now.

## Run states

```
pending → running → completed
                  → waiting    (parked for a human; continue with Resume)
                  → cancelled  (stopped by an operator; continue with Start)
                  → failed     (the agent could not finish the turn)
                  → retired    (closed for good; Start and Resume refuse it)
```

## How it works

BONNIE uses four public Kit extension points. There is no fork and no patched
SDK:

| Need | Kit API |
|---|---|
| Journal each message | `Options.SessionManager` |
| Checkpoint each step | `Kit.OnStepFinish` |
| Inject replayed context | `Kit.OnContextPrepare` |
| Park for a human | `kit.ToolOutput{Halt, FinalValue}` |

Three properties are the difference between a demonstration and something you
can deploy:

- **Replay is lossless.** A journalled message keeps its typed parts, thus a
  resumed run knows which tools it called and what they returned. It does not
  do a side effect a second time.
- **A step commits atomically.** A tool call and its result reach the journal
  as one write and one fsync (`kit.StepAppender`, from Kit `v0.106.0`). Thus a
  crash cannot leave a tool call with no answer. If a torn step is on disk —
  from an older journal, a journal that does not batch, or a short write —
  restore removes that incomplete step and records the repair.
- **To cancel keeps finished work.** A step is written before the context is
  examined, thus a cancelled turn loses only the step in progress.

The full detail, with the Kit citations, is in the godoc of `runtime/`. The
code is the specification.

## Limits

Stated plainly, because the failure modes are not obvious:

- **Linux only.** BONNIE's floor is the Landlock LSM, thus a release builds for
  linux/amd64 and linux/arm64 and nothing else. macOS and Windows are not
  supported and are not on the roadmap: to restore macOS needs a seatbelt
  (`sandbox-exec`) backend of equal strength first, and not one more build
  target. A kernel older than 5.13, or a kernel booted with Landlock disabled,
  has no default sandbox. BONNIE refuses to start instead of a run with no
  confinement — use `--sandbox docker` there.
- **The default sandbox is containment, not isolation.** Landlock confines the
  filesystem and keeps host credentials away from a command. It does not
  confine the network, and it shares the host kernel.
- **Do not run BONNIE as a user in the `docker` group.** Landlock mediates the
  open of a file, and not the connection to a socket. Thus a tool call reaches
  `/var/run/docker.sock` when the process can, and that is a full host escape.
  No path setting closes it. Use an unprivileged user, or microsandbox.
- **Docker is namespaces, not a kernel.** Use microsandbox for hostile code.
- **microsandbox is verified on Linux with KVM.** Each network policy mode is
  enforced, but the policy is fixed when the sandbox is made. To attach again
  with a different policy fails with `ErrPolicyMismatch`.
- **Sandbox egress is open** until you set a policy, and the default backend
  cannot set one — it refuses the policy instead of ignoring it.
- **The HTTP channel verifies a caller only when you configure one.**
  `http.WithAuthenticator` (or `bonnie.WithHTTPAuthenticator`) checks every
  route but `GET /bonnie/v1/health` and mints the run's identity from what it
  proved. Without one the channel carries a `Principal` it does not examine,
  so authenticate in front of it, and `operation_id` is refused because an
  idempotency key with no proven owner reads another caller's run. The chat
  channels are different: each one verifies its platform's signature, and a
  channel with no credentials refuses to serve. That verifies the platform and
  not the person: a user ID in a verified Slack event is Slack's word.
- **Run ownership is per host, and the journal does not refuse a second
  writer.** SQLite serialises write transactions and rejects a reused sequence
  number, thus two processes that write one run cannot corrupt it. That is
  journal integrity and not turn coordination: two servers that both execute
  the same run still interleave the conversation. SQLite locking also needs
  POSIX locks that work, thus a journal on a network filesystem is still
  unsafe.
- **Events are journal-anchored.** The stream replays the journal after the
  in-memory backlog, thus a reconnect — also after a restart — has no gap.
  Live-only deltas are the exception, and they are marked.
- **Sandbox lifecycle is journalled, and reclamation is manual.**
  `bonnie sandbox prune` deletes the sandboxes of finished runs. `serve` does
  not sweep them yet.
- **The mark3labs modules are publicly fetchable.** A scaffolded module runs
  `go mod tidy` and resolves `bonnie` and `kit` from the proxy; no `GOPRIVATE`.
  To author an agent needs Go on your machine. The binary that `bonnie build`
  makes needs nothing on the host.

## Examples

Each example is an agent tree made with `bonnie init`, run with `bonnie dev`,
and shipped with `bonnie build` — the same shape as your own agent.

| Example | Shows |
|---|---|
| [`examples/github-bot`](examples/github-bot) | a durable agent on GitHub: issues, pull requests, review threads |
| [`examples/slack-bot`](examples/slack-bot) | a durable agent in Slack: threads, controls, a live activity indicator |

```bash
cd examples/github-bot
bonnie dev --addr 127.0.0.1:8081 --tui=false
```

See [`examples/README.md`](examples/README.md) for commands you can copy.

## Documentation

| Document | Purpose |
|---|---|
| [godoc](https://pkg.go.dev/github.com/mark3labs/bonnie) | The specification. Each exported symbol has its contract and, often, the defect that shaped it |
| [`CHANGELOG.md`](CHANGELOG.md) | What each release changed, and the limits it recorded |
| [`docs/RELEASE.md`](docs/RELEASE.md) | The release checklist, and what each tag confirmed |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Boundary rule, workspace setup, commands |
| [`AGENTS.md`](AGENTS.md) | The same rules, for a coding agent |
| [`SECURITY.md`](SECURITY.md) | Disclosure, and what BONNIE does not protect you from |

## Contributing

```bash
go build ./...
go test -race ./...
golangci-lint run
```

Or run the same loop with `task check`, and CI parity with `task ci`.

The live-model tests need a build tag and a provider key:

```bash
go test -race -tags integration ./runtime ./sandbox
```

They skip, and never fail, when no key is present. One rule is above the rest:
**BONNIE uses the public Kit SDK only**. See
[`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

MIT — see [LICENSE](LICENSE).
