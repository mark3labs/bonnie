# BONNIE

**B**uilder **O**f **N**eural **N**etwork **I**ntelligence **E**ngines

A durable agent framework built on the [Kit](https://github.com/mark3labs/kit) SDK.

Kit gives you an excellent agent *kernel*: models, tools, hooks, events, MCP,
and a branching session tree. Its unit of work is one in-process turn, which
lives and dies with the process. BONNIE adds the layer above it, so a run

- survives process death,
- parks indefinitely for human input without holding compute,
- is reachable from outside the process.

> Status: `v0.1.0` scope is complete. The durable executor, the JSONL journal,
> the HTTP channel, and the CLI are implemented and tested. The suspend and
> resume path is verified against a live model, not only against fakes. See
> [`docs/SPEC.md`](docs/SPEC.md) for the specification and
> [`docs/TASKS.md`](docs/TASKS.md) for the work items.

## Design rules

**1. Public Kit API only.** BONNIE is a separate Go module whose path is not a
prefix of Kit's, so the Go compiler rejects any import of
`github.com/mark3labs/kit/internal/...`. BONNIE also never imports
`charm.land/fantasy`: Kit re-exports every model type as a `kit.LLM*` alias.
The boundary is enforced by the toolchain, not by review. A `depguard` rule and
a CI job restate it for a readable failure.

If BONNIE needs something Kit does not export, the fix is to export it from
`pkg/kit` upstream. That pressure is deliberate: BONNIE is Kit's first serious
external consumer, so every gap it hits is a gap real SDK users hit.

**2. Durability is a seam, not a rewrite.** L1 is built from four public Kit
extension points and adds no fork:

| Need | Kit public API |
|---|---|
| Journal every message | `Options.SessionManager` (`kit.SessionManager`) |
| Checkpoint each step | `Kit.OnStepFinish` |
| Inject replayed context | `Kit.OnContextPrepare` |
| Suspend for human input | `kit.ToolOutput{Halt, FinalValue}` |

**3. Headless.** BONNIE never renders a terminal. The TUI is Kit's job.

## Layers

```
L4  CLI (serve, runs)                  ✅ implemented
L3  channel/ + channel/http            ✅ implemented
L2  agent/ tree discovery + codegen    (planned)
L1  Durable run executor               ✅ implemented
L0  github.com/mark3labs/kit/pkg/kit   (upstream)

    sandbox/  isolated tool execution   ✅ implemented
```

Each layer is usable alone. That is what makes this a framework and not a
monolith.

## Sandboxing

BONNIE runs the tool calls a model chooses. Without a sandbox those calls run
as the host process, with its files, its network, and its credentials.

```go
provider := sandbox.Docker(sandbox.WithDockerImage("python:3.12-slim"))

runner := runtime.NewRunner(journal, sandbox.Agent(provider,
    kit.WithModel("anthropic/claude-sonnet-4-5"),
))
```

```sh
bonnie serve --sandbox docker --sandbox-image python:3.12-slim
```

| Backend | Isolation | User installs | New Go deps |
|---|---|---|---|
| `Local()` | **none** | — | 0 |
| `Docker()` | container namespaces | Docker | 0 |
| `Microsandbox()` | microVM, guest kernel | `msb` | 0 |

All three drive a CLI through `os/exec`, so they add no dependency and the
release stays a single static binary.

The sandbox opens on the **first tool call that needs it**. A run that parks
for human input holds no sandbox compute, and a model that never calls a tool
never starts a container. Read [`docs/SANDBOX.md`](docs/SANDBOX.md) before
deploying.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/mark3labs/bonnie/runtime"
)

func main() {
	ctx := context.Background()

	// A file journal is what makes the run durable.
	journal, err := runtime.OpenFileJournal(".bonnie")
	if err != nil {
		log.Fatal(err)
	}
	defer journal.Close()

	r := runtime.NewRunner(journal, runtime.KitAgent())

	run, err := r.Start(ctx, "run-1", runtime.Input{Text: "Deploy the app."})
	if err != nil {
		log.Fatal(err)
	}

	// The model called ask_human, so the run is parked. Nothing holds compute:
	// this process can exit now, and another process can finish the run.
	if run.State == runtime.RunWaiting {
		fmt.Println("agent asks:", run.Suspend.Prompt)

		run, err = r.Resume(ctx, "run-1",
			[]runtime.InputResponse{{Text: "eu-west-1"}})
		if err != nil {
			log.Fatal(err)
		}
	}

	fmt.Println(run.Response)
}
```

That is [`examples/minimal`](examples/minimal) with the suspension branch
added. [`examples/hitl-restart`](examples/hitl-restart) does the same across a
**real** process exit, which is the demonstration that matters. See
[`examples/README.md`](examples/README.md).

## Over HTTP

```bash
bonnie serve --journal .bonnie --addr :8080
```

| Route | Purpose |
|---|---|
| `POST /runs` | Start a run, or resolve an address to the run that serves it |
| `GET /runs/{id}` | Report a run's durable state |
| `POST /runs/{id}` | Send a message to an existing run |
| `POST /runs/{id}/respond` | Answer a suspended run |
| `POST /runs/{id}/cancel` | Stop the turn a run is executing |
| `GET /runs/{id}/stream` | NDJSON event stream, resumable with `?cursor=` |

`From(address)` resolves a channel-local address — a Slack thread, a browser
session — to whichever run serves it now, and creates one when the address is
new. `Attach(runID)` targets exactly one run and never creates: an unknown ID
is a 404. The address map lives in the journal, so a restart does not orphan a
conversation.

## Inspect a run

```bash
bonnie runs list --journal .bonnie --state waiting
bonnie runs show --journal .bonnie run-1
bonnie runs show --journal .bonnie run-1 --json | jq '.[] | select(.kind=="message")'
```

The journal is JSONL, one file per run, so `jq` works on it directly.

## Bring your own storage

`FileJournal` and `MemoryJournal` ship with BONNIE. Implement
`runtime.Journal` — seven methods — to back runs with Postgres, S3, or
anything else.

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
against every implementation. Add yours to it.

## What durability means here

- **Replay is lossless.** `Record.Payload` carries the JSON-encoded
  `kit.LLMMessage`, so tool calls, tool results, files, and reasoning survive a
  resume. `Record.Text` is a display projection, never the source of truth.
- **A torn write is repaired.** Kit journals the assistant message and the tool
  message of one step separately, so a crash between them leaves an unanswered
  tool call that every provider rejects. `Restore` drops that incomplete
  trailing step and journals the repair. A mismatch anywhere but the tail is
  real corruption, and `Restore` reports it instead of rewriting history.
- **A cancelled turn keeps its work.** Kit persists a step's messages before it
  checks the context, so `Runner.Cancel` never throws away a finished step.
- **Run state is durable and branch-aware.** `Session` implements
  `kit.SessionManager`, so Kit's branching model keeps working: fork a
  conversation, collapse a branch, compact the context.
  `AppendExtensionData` / `GetExtensionData` is a key/value store that survives
  a crash.

## Testing

```bash
go build ./...
go test -race ./...
go vet ./...
golangci-lint run
```

The hermetic suite uses a fake agent and needs no credentials.

The live-model test is behind a build tag, because it costs money and needs a
network:

```bash
export ANTHROPIC_API_KEY=sk-ant-...     # or OPENAI_API_KEY, or GEMINI_API_KEY
go test -race -tags integration ./runtime
```

Set `BONNIE_TEST_MODEL` to choose the model, for example
`BONNIE_TEST_MODEL=anthropic/claude-sonnet-4-5`. With no credential the test
skips; it never fails for a missing key.

## Local development

BONNIE and Kit are separate repositories. Use a Go workspace **in the parent
directory**, never a `replace` directive in a published `go.mod`:

```
~/Workspace/
  go.work        <- use (./kit ./bonnie)
  kit/
  bonnie/
```

```bash
cd ~/Workspace
go work init ./kit ./bonnie
```

CI sets `GOWORK=off` so it always builds against the published Kit version in
`go.mod`. Without that, a workspace hides real breakage.

## Limits of v0.1.0

State them plainly, because the failure modes are not obvious:

- **Sandboxing is opt-in.** Without `--sandbox`, or without
  `sandbox.Agent(...)`, BONNIE runs the tool calls a model chooses as the host
  process. See [`docs/SANDBOX.md`](docs/SANDBOX.md).
- **Docker is namespaces, not a kernel.** Use microsandbox when the threat
  model includes hostile code.
- **Sandbox egress is open by default.** Pass `--sandbox-deny-network`, or set
  a policy, for untrusted work.
- **No auth verification.** The HTTP channel carries a `Principal`; it does not
  check one. Authenticate in front of it.
- **One process per run.** The file journal takes no cross-process lock. Two
  processes that write the same run interleave records.
- **Events are not durable.** The event bus keeps a bounded in-memory backlog
  for reconnecting clients. The journal is the durable record.
- **Sandbox lifecycle is not journalled.** A deleted container leaves a run
  whose files are gone; the conversation survives, the workspace does not.

## Documentation

| Document | Purpose |
|---|---|
| [`docs/SPEC.md`](docs/SPEC.md) | Specification: scope, verified Kit facts, known risks, invariants, eve analogues |
| [`docs/SANDBOX.md`](docs/SANDBOX.md) | Sandbox backends, the contracts an adapter must honour, and why the CLI |
| [`docs/TASKS.md`](docs/TASKS.md) | Numbered work items with acceptance criteria |
| [`docs/UPSTREAM.md`](docs/UPSTREAM.md) | The changes BONNIE asks of Kit, with evidence |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | The boundary rule, the workspace setup, the commands |
| [`SECURITY.md`](SECURITY.md) | Disclosure, and what v0.1.0 does not protect you from |
| [`AGENTS.md`](AGENTS.md) | Conventions for agents working in this repo |

## Upstream dependencies

Four changes to `pkg/kit` would remove workarounds here. See
[`docs/UPSTREAM.md`](docs/UPSTREAM.md) for the evidence behind each.

1. **Batch append on `SessionManager`** — without it, no external session
   manager can write a tool-calling step atomically, so every one of them needs
   BONNIE's torn-write repair.
2. **`PrepareStepResult.Tools []Tool`** — the doc comment already advertises
   "dynamic tool filtering per step" but the struct cannot express it.
3. **Stability promise on `Halt` + `FinalValue`** — it currently reads as a
   stop-early convenience. BONNIE's suspension protocol depends on it.
4. **A stability policy for `kit.SessionManager`** — adding a method is a
   breaking change for every external implementer, BONNIE included. The
   compile-time assertion in `session.go` is the tripwire.

## License

MIT
