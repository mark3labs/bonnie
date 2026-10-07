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
directories `skills/` and `context/`. It copies authored context files into each
run; it does not add them to the prompt. With `--tools` it also writes a sample
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

Chat uses the full terminal width and an alternate-screen transcript. Use
Page Up/Down or the mouse wheel to scroll. New output follows the bottom only
when you are already there. Ctrl+Home goes to the first output; Ctrl+End goes
to the latest output. Enter sends a message; Shift+Enter adds a new line.

Local commands: `/help`, `/new`, `/retry`, `/cancel`, and `/exit` (or `/quit`).
`/new` starts a separate conversation and keeps the old run on the server.
`/retry` sends the last user message again as a new turn. It does not undo
history or tool effects, so an external action can repeat. Cancel an active
turn, or wait for it to finish, before you use `/new` or `/retry`.

Files under `context/` are context files. BONNIE copies them into each run's
isolated sandbox by default. It does not add them to the prompt or replace files
the model has written.

For a deliberate single-user development workflow, a host can use one shared
working-files directory across all runs:

```go
bonnie.New(
    bonnie.WithSharedDirectory("./working-files"),
)
```

This uses the directory directly instead of copying seed files. It keeps the
default Landlock filesystem confinement: runs share this directory, but do not
get unrestricted host access. Explicit Landlock and Local providers are also
supported; Local provides no isolation. Other backends are rejected.

Do not combine this option with `WithContextFiles`. Overlapping opens through the same provider are rejected, not queued. The guard does not coordinate separate processes or provider instances: use one server for this development directory. Run pruning does not remove the shared directory. Without this option, each run has separate working files.

For one-off tasks, enable automatic sandbox cleanup:

```go
bonnie.New(
    bonnie.WithRunSandboxCleanup(bonnie.SandboxCleanupPolicy{
        CompletedAfter: time.Hour,
        RetiredAfter:   time.Hour,
    }),
).Serve()
```

Import `time` for these durations. This works in the compiled agent; it does not
need the BONNIE CLI. Sandbox cleanup runs at startup and once per minute. Zero keeps the sandbox
for that state. `FailedAfter` and `CancelledAfter` can also be set.
Pending, running, and waiting runs are never cleaned up. The journal and channel
addresses remain. A later turn on a cleaned-up run starts without its earlier
files, so publish task output before the run completes.

Sandbox cleanup cannot be combined with `WithSharedDirectory`, shared provider modes,
or `WithAgentFactory`. Negative durations and providers without deletion support
are rejected at startup. Use one server for the journal and working files; cleanup
locks do not coordinate separate processes. Each deletion has a 30-second timeout.
Failures are logged and retried on the next sweep.

Files under `skills/` are the agent's skills: one `*.md` per skill, or one
subdirectory per skill with a `SKILL.md` in it, each with YAML frontmatter
that gives a `name` and a `description`. Those two fields go in the system
prompt; the body arrives only when the model calls `activate_skill`, so a
large skill set costs few tokens until it is used. The tree's `skills/` is the authored set. Skills in a run's working files can also be
discovered in the sandbox; skills beside the host process are not inherited.

There is no configuration file. A setting is a file at a known path
(`instructions.md`, `context/`, `skills/`, `tools/`) or an option in
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
register them too by default. To omit both built-in human-input tools:

```go
bonnie.New(bonnie.WithoutHumanInput()).Serve()
```

The model decides when to call these tools; there is no automatic approval
policy for external actions. `WithoutHumanInput` does not change sandbox
permissions, remove caller-supplied tools, or prevent a custom tool from
parking a run. The model can still ask questions in response text. This option
cannot be combined with `WithAgentFactory`, which owns the agent's tools.
Low-level hosts can use `sandbox.AgentWithoutHumanInput` or
`runtime.KitAgentWithoutHumanInput`.

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

The turn ends at the halting tool: the model is not asked again until the run
resumes. A tool that the model calls in the **same** step as the halting tool
still runs, because Kit runs the calls of one step together.

In an agent tree, `bonnie init --tools` writes one sample tool. A tool there is
`tools/<name>/tool.go` with `func Tool() kit.Tool`, and the directory name is
the tool's name. `bonnie dev` and `bonnie build` generate the wiring again,
thus `main.go` never names a tool.

## Sandboxes

Each tool call runs in a sandbox. There is no unsandboxed mode. `WithSandbox`
**selects** a backend, it does not enable one. If you do not call it, you get
`sandbox.Landlock()`, and not your process.

The default confines tool calls to the run's own working files with the Linux
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

To let an operator select a backend without rebuilding the agent, declare
its permitted providers with `WithSandboxes`:

```go
bonnie.New(
	bonnie.WithSandboxes(
		sandbox.Microsandbox(
			sandbox.WithMicrosandboxImage("python:3.12-slim"),
			sandbox.WithMicrosandboxMemory(2048),
			sandbox.WithMicrosandboxCPUs(2),
		),
		sandbox.Local(), // No isolation; select explicitly for development only.
	),
).Serve()
```

```sh
./my-agent                        # First provider: microsandbox.
./my-agent --sandbox microsandbox # Keeps the configured image, memory, and CPUs.
./my-agent --sandbox local        # Explicit host execution for development.
```

Compiled agents use Cobra and Fang for styled help and startup errors. Run
`./my-agent --help` to see the permitted sandbox names and the default. This
adds no separate executable; Microsandbox still requires `msb`. Host-registered
Go flags remain supported, as do single-dash long flags such as `-addr`.
`Agent.Run(ctx)` does not parse flags or render command help.

The first provider is the default. Only the selected provider must be available;
a failure never selects another backend. An unknown name returns an error with
the permitted names. The list must be non-empty, with non-nil providers and
unique, non-empty names. Do not combine `WithSandboxes` with `WithSandbox` or
`WithAgentFactory`, or call `WithSandboxes` more than once.

`WithSandbox` permits just its one provider. With neither option, a compiled
agent permits only Landlock. `Agent.Run` does not read flags and uses the first
provider. Network and environment options apply to the selected backend; a
backend that cannot enforce the network policy returns an error.

If you wire the runner yourself, it is the same provider one layer down:

```go
provider := sandbox.Docker(sandbox.WithDockerImage("python:3.12-slim"))

runner := runtime.NewRunner(journal, sandbox.Agent(provider,
	kit.WithModel("anthropic/claude-sonnet-4-5"),
))
```

The model gets four tools that run in the sandbox: `shell`, `read_file`,
`write_file`, and `list_files`. Their root is the sandbox work directory, `sandbox.WorkDir`, whose path is
`/workspace`. A path that leaves the work directory, including through a
symlink the model made, gets `sandbox.ErrOutsideWorkDir`.

The `shell` tool checks for Bash inside the sandbox on each call. It uses
Bash when available and falls back to `sh` otherwise. Each result names the
selected shell. A failed command is never retried under another shell.
Detection does not open the sandbox before the first tool call.

| Backend | Isolation | You install | Network policy |
|---|---|---|---|
| `sandbox.Landlock()` — **default** | filesystem **containment**, shared kernel | — | none: a policy is refused |
| `sandbox.Local()` | **none** — development only | — | none: a policy is refused |
| `sandbox.Docker()` | container namespaces | Docker | `allow-all`, `deny-all` |
| `sandbox.Microsandbox()` | microVM, guest kernel | [`msb`](https://github.com/superradcompany/microsandbox) | `allow-all`, `deny-all`, `allow-list` |

The Docker and microsandbox backends drive a CLI, and Landlock is pure Go.
When microsandbox is selected, startup first uses `msb` from `PATH`, then
`<journal>/msb` or `<journal>/microsandbox/bin/msb` (`<journal>` is `.bonnie` by
default). Existing binaries are not upgraded. If none exists, BONNIE downloads
its tracked release (`sandbox.MicrosandboxVersion`, currently `v0.7.7`), checks
its pinned SHA-256, and installs `msb` and `libkrunfw` in
`<journal>/microsandbox`. The first install needs network access and a writable
journal directory. The host still needs microsandbox's system requirements,
including KVM on Linux. Automatic installation supports Linux amd64/arm64 and
macOS arm64. Use `bonnie.WithSandboxDownload(false)` to disable downloads.
A custom `sandbox.WithMicrosandboxBinary` path is never downloaded or replaced.
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
`context/` directory this way.

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
runtime.OpenSQLiteJournal(".bonnie")  // SQLite, one database for the journal root
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
- **A halt stops the turn, not the step.** Kit runs the tool calls of one step
  together, thus a tool that the model calls in the same step as
  `request_approval` (or another halting tool) still runs before the run
  parks. Approval gates the next step, not a sibling call.
- **A skill's bundled files stay on the host.** Kit names a skill's
  `scripts/`, `references/`, and `assets/` files in the text of the
  activation, with a host path. The tools run in a sandbox that does not have
  that path, thus the model is told about a file that it cannot open. Put what
  the model must read in the skill body, and put a file that it must open in
  `context/`, which is copied into the sandbox.
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
- **Sandbox lifecycle is journalled; cleanup is opt-in.**
  `bonnie sandbox prune` deletes the sandboxes of finished runs. Compiled agents
  can use `WithRunSandboxCleanup` for retention-based sandbox cleanup; the CLI `serve`
  command does not sweep them. Cleanup keeps waiting runs and history, but deletes
  earlier files of eligible finished runs. Publish output before completion.
  Cleanup locks do not coordinate separate processes or Runner instances.
- **The mark3labs modules are publicly fetchable.** A scaffolded module runs
  `go mod tidy` and resolves `bonnie` and `kit` from the proxy; no `GOPRIVATE`.
  To author an agent needs Go on your machine. The binary that `bonnie build`
  makes needs nothing on the host.

## NATS tasks

A deployed agent can subscribe to Core NATS and publish outcomes asynchronously:

```go
// Import natschannel "github.com/mark3labs/bonnie/channel/nats".
bonnie.New(
    bonnie.WithModel("opencode/kimi-k3"),
    bonnie.WithNATS(natschannel.Config{
        Subject:       "agents.review.tasks",
        AnswerSubject: "agents.review.answers",
        ResultSubject: "agents.review.results",
    }),
).Serve()
```

Set `NATS_URL` to the broker URL and configure the model's provider key. Each
subject must be a distinct literal subject.

### Activity logging

Add this option to `bonnie.New(...)` to log agent activity to stdout:

```go
bonnie.WithActivityLogger(bonnie.NewActivityLogger(nil)),
```

Activity logging works with every channel, including NATS. It is off by
default. The default logger uses `charmbracelet/log` with timestamps. Info
output includes run states, suspensions, resumes, tool names and call IDs,
and final response text. Failures use Error level. Warnings and retries use
Warn level. Other lifecycle events and raw payloads use Debug level.

To select JSON output or Debug level, supply a Charm logger:

```go
// Import "os" and "github.com/charmbracelet/log".
logger := log.NewWithOptions(os.Stdout, log.Options{
    Formatter:       log.JSONFormatter,
    Level:           log.DebugLevel,
    ReportTimestamp: true,
})
// Add this option to bonnie.New(...):
bonnie.WithActivityLogger(bonnie.NewActivityLogger(logger)),
```

Logs can contain sensitive data. Info output includes final responses;
Debug output also includes prompts, tool arguments, results, and reasoning.
Only live events are logged; journal replay does not log them again. Logging
is synchronous, so a slow output writer delays the run. A host can also
supply its own `runtime.ActivityLogger` through the same option.

### NATS authentication

For NKey authentication, set `NATS_NKEY_SEED` to the **user seed value**, not
its file path. BONNIE reads it when it builds the channel; the `WithNATS`
setup above does not need to change. Load the value from your secret store.
Do not put it in source code, logs, or the agent sandbox environment.
The server must authorize the corresponding public user NKey.

You can also set `NKeySeed` in `natschannel.Config` explicitly. An explicit
value takes precedence over `NATS_NKEY_SEED`. Malformed seeds and non-user
seeds fail validation without including the seed in the error. This works
with both Core NATS and JetStream.

For bearer token authentication, set `NATS_TOKEN` or `Config.Token`. For
user/password authentication, set `NATS_USERNAME` and `NATS_PASSWORD`, or
`Config.Username` and `Config.Password`. A username is required when a password
is set; an empty password is permitted. Each explicit nonempty field takes
precedence over its environment fallback. Use TLS to protect credentials in
transit.

Configure only one method: NKey seed, token, or user/password. This includes
environment fallbacks; unset variables for methods you do not use. Do not
combine URL credentials with authentication fields. Conflicts fail validation
without including credential values in the error.

For JWT credentials or other connection options, supply an authenticated
`Config.Conn` instead. Do not combine `Conn` with `URL` or authentication fields.
When `Conn` is supplied, BONNIE ignores all connection environment variables
and does not close the connection; the caller owns its lifetime.

Start a result subscriber before publishing a task:

```bash
nats sub agents.review.results
# In another terminal:
nats pub agents.review.tasks '{"task_id":"review-42","text":"Review the supplied code."}'
```

Each task ID starts an independent durable run. The result contains `task_id`,
`run_id`, `state`, and `response`, or an `error`. A waiting result contains
`suspend`, including its `tool_call_id`. Publish an explicit answer to resume it:

```bash
nats pub agents.review.answers '{"task_id":"review-42","tool_call_id":"CALL_ID_FROM_RESULT","responses":[{"text":"Use staging."}]}'
```

A repeated task is rejected, not interpreted as an answer. Answers must match
the current suspension. Workers and buffers are bounded; `Concurrency` defaults
to 4 and `Buffer` to 64. Shutdown cancels active turns and waits for workers.

**This adapter follows the asynchronous GitHub pattern, not a durable broker
queue.** It uses Core NATS, not JetStream. Offline subscribers, buffer overflow,
and process failure can lose tasks or result delivery. The journal preserves run
state, but the adapter does not automatically retry interrupted tasks or result
publication. Use broker permissions to restrict publishers and subscribers;
message payloads do not verify identity. Run one owner for each task namespace;
multiple ordinary subscribers each receive a copy and can repeat the work.

### Root subjects and task statuses

Use a root subject to enable JetStream and derive all protocol subjects:

```go
bonnie.WithNATS(natschannel.Config{
    RootSubject: "agents.review",
    WorkerID: "review-1",
    CreateStream: true,
})
```

The defaults are `<root>.tasks`, `.results`, `.events`, `.answers`, `.commands`,
and `.queries`. Answers, commands, and queries append `.<worker_id>`. Explicit
subject fields override individual defaults. Any literal root is valid, including
`tasks` or `company.team.agents.review`; no wildcard or empty token is permitted.
Root configuration creates stable input, result, and event stream names.
`CreateStream` permits creation of all three. JetStream must be enabled on the
server, and the connection needs stream administration permissions. Existing
resources are validated, never changed. Without `CreateStream`, provision them
first. Stream names can be overridden.

The client uses the same root:

```go
c, err := natsclient.New(nc, natsclient.Config{
    RootSubject: "agents.review",
    CreateStream: true,
})
```

`ConsumeEvents(ctx, handler)` delivers durable `task_accepted` and `run_state`
events. State values include pending, running, waiting, completed, failed, and
cancelled. Acceptance confirms a saved task/attempt/run mapping, not execution.
Events contain IDs, timestamps, and journal cursors, not agent text or tool
activity. The worker recovers unpublished states from its journal after restart.
Delivery is at least once: discard duplicate `event_id` values and order each
run by `seq`. Results remain on `.results` and retain responses and input requests.
Events and results are independent streams; do not assume cross-stream ordering.
Separate applications need separate `EventConsumer` and `ResultConsumer` names
when each needs all messages. Stream retention limits can remove old events.

Use `c.Status(ctx, event.Target)` to query an exact attempt and
`c.Cancel(ctx, event.Target)` to request cancellation. These are worker-routed
NATS request/reply calls, not durable queued commands. A reply's `Error` reports
rejection; a Go error reports transport or decoding failure. `CancelRequested`
confirms the request only. The final state arrives separately. Cancellation stops
an active turn; waiting and finished runs return not-active. External effects
are not undone. A timeout does not prove task failure. Keep each worker's identity
and journal stable, and run only one live owner of that identity.

Status queries include `Active`: a saved running state with `Active: false`
indicates interruption, not current execution. Querying an unknown attempt returns
an error. There is no global task lookup across separate worker journals. Protect
worker routes and reply inboxes with NATS permissions. Replies use `_INBOX.*`
subjects; custom inbox prefixes are not supported.

### Targeted task delivery

Enable `TargetedTasks: true` on both the NATS channel and typed client. This
requires JetStream. A worker reads shared tasks and tasks for its own `WorkerID`.

```go
bonnie.WithNATS(natschannel.Config{
    RootSubject: "agents.review",
    WorkerID: "review-1",
    TargetedTasks: true,
    CreateStream: true,
})

// nc is a caller-owned NATS connection.
c, err := natsclient.New(nc, natsclient.Config{
    RootSubject: "agents.review",
    TargetedTasks: true,
    CreateStream: true,
})
if err != nil { return err }
_, err = c.SubmitTo(ctx, "review-1", natsclient.Task{
    TaskID: "review-42", Text: "Review the code.",
})
```

`Submit` still sends shared work. `SubmitTo` stores work on
`<task-subject>.worker.<worker-id>`. Each worker has a separate durable targeted
consumer. An offline target's task stays in JetStream within retention limits;
it never falls back to another worker. Keep one live owner per worker identity.
Answers and results use their existing routes. Delivery remains at least once.

New input streams include `<task-subject>.worker.*`. For an existing stream,
add that subject explicitly before enabling targeted delivery. BONNIE does not
change existing streams. Update NATS permissions to permit these routes. Task
IDs are deduplicated per route: sending the same ID to shared work or another
worker is a separate submission and can execute again.

### JetStream and the typed client

Set `Stream` to enable JetStream. Leave `Consumer` empty to share tasks through
a stable consumer derived from the task subject. Each worker must have a unique,
stable `WorkerID` and its own journal and sandbox data. Workers do not exchange
run state. Set `Consumer` explicitly for separate processing groups or to bind
an existing consumer. Changing its name can replay retained tasks.
`natschannel.DefaultConsumerName(subject)` gives the derived name for operators.

```go
bonnie.WithNATS(natschannel.Config{
    Subject: "agents.review.tasks", AnswerSubject: "agents.review.answers",
    ResultSubject: "agents.review.results",
    Stream: "REVIEW-TASKS", WorkerID: "review-1",
    CreateStream: true,
})
```

`CreateStream` explicitly permits input stream creation. Provision a result
stream before workers start, or use the typed client's `CreateStream` option.
Existing streams and consumers are validated, not changed. Input streams must
retain the task subject and `agents.review.answers.*` with limits retention.
The worker creates or binds durable pull consumers. Answers use a separate
consumer for each worker.

The main service can use [`client/nats`](client/nats/README.md) instead of raw JSON:

```go
// nc is a caller-owned *nats.Conn.
c, err := natsclient.New(nc, natsclient.Config{
    TaskSubject: "agents.review.tasks", AnswerSubject: "agents.review.answers",
    ResultSubject: "agents.review.results",
    CreateStream: true,
})
if err != nil { return err }
_, err = c.Submit(ctx, natsclient.Task{TaskID: "review-42", Text: "Review the code."})
if err != nil { return err }
return c.Consume(ctx, func(ctx context.Context, outcome natsclient.Outcome) error {
    // Store the result or input request. Return an error if storage fails.
    return storeOutcome(ctx, outcome)
})
```

The client derives stable result stream and consumer names from `ResultSubject`.
Override `ResultStream` for an existing stream or `ResultConsumer` for independent
readers. The default consumer shares processing across main-service instances.
Stream creation still requires `CreateStream: true`.

Import `natsclient "github.com/mark3labs/bonnie/client/nats"`. `Submit` confirms
broker storage, not execution. `Consume` acknowledges only after handler success.
Call `c.Answer(ctx, outcome, responses)` for a waiting outcome; the client checks
and selects its worker route. The service does not construct subjects or JSON.
Raw publishers use protocol version 1; the typed client supplies it for tasks.

Workers send acknowledgement progress while executing and publish a confirmed
result before acknowledging each input. A waiting result also releases the
input. Outcomes are saved locally so redelivery to the same worker can retry
publication without repeating completed execution. An admitted answer can
recover a completed or new-waiting outcome after restart. An interrupted answer
that cannot be safely continued returns an explicit failure instead of guessing.
Oversized outcomes return a bounded error; full run data stays in the journal.

**Delivery is at least once, not exactly once.** Redelivery to another worker
can execute the task again. Interrupted tasks start fresh attempts. Track
`task_id`, `attempt_id`, and `run_id`, and make external effects and result
handlers safe to repeat. Broker deduplication has a bounded window. A waiting
run needs its original worker and stored state to answer; lost worker state
requires a new task. Use separate task consumers for intentional agent fan-out,
and separate result consumers for applications that each need all outcomes.

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
| [`docs/SCHEDULES.md`](docs/SCHEDULES.md) | Define cron schedules in code, trigger them over HTTP, and understand their delivery limits |
| [`docs/RELEASE.md`](docs/RELEASE.md) | The release checklist, and what each tag confirmed |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Boundary rule, work-directory setup, commands |
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
