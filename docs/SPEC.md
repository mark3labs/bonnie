# BONNIE MVP Specification

**Status:** `v0.1.0` scope complete, not yet tagged · **Audience:** implementing agent

This document is the authoritative specification for BONNIE's first release. It
records what is built, what must be built, the facts about Kit that the design
depends on, and the risks that are known but not yet handled.

Read this file before `docs/TASKS.md`.

---

## 1. What BONNIE is

Kit (`github.com/mark3labs/kit`) is an agent **kernel**: models, tools, hooks,
events, MCP, and a branching session tree. Its unit of work is one in-process
turn, `Kit.PromptResult(ctx, msg)`, which lives and dies with the process.

BONNIE is the layer above it. It turns that turn into a **durable run** that

- survives process death,
- can park indefinitely awaiting human input without holding compute,
- is reachable from outside the process.

The closest published analogue is [eve](https://eve.dev), a TypeScript agent
framework. eve is a useful reference because it solves the same problems with a
different substrate, and its documentation is unusually precise. BONNIE is not
an eve clone: see §7 for what is deliberately different.

### Layers

```
L4  CLI, evals, traces                 CLI implemented; evals planned
L3  channel/    inbound transports     channel/http implemented
L2  discovery   agent/ tree + codegen  planned, NOT in MVP
L1  runtime/    durable run executor   implemented, memory + file journals
L0  kit/pkg/kit                        upstream, unmodified

    sandbox/    isolated tool calls    implemented; local + docker verified,
                                       microsandbox verified on Linux/KVM
```

`sandbox/` sits beside L1 rather than inside it. It depends on `runtime` for
`AgentFactory` and nothing depends on it, so a host that wants durable runs
without isolation never compiles it.

Each layer must stay usable alone. That property is what makes BONNIE a
framework rather than a monolith. Do not introduce an L1 → L3 dependency.

---

## 2. The hard rule

**BONNIE uses the public Kit SDK only: `github.com/mark3labs/kit/pkg/kit`.**

Never import `github.com/mark3labs/kit/internal/...`. Never import
`charm.land/fantasy` either: Kit re-exports every model type BONNIE needs as an
alias (`kit.LLMMessage`, `kit.LLMToolCallPart`,
`kit.LLMToolResultOutputContentText`), and naming fantasy directly pins BONNIE
to Kit's own transitive dependency. It must stay `// indirect` in `go.mod`.
When Kit aliases a type but not a helper that works on it, write the helper in
BONNIE — see `toolResultText` in `runtime/util.go`.

This is not a style preference; it is enforced three ways, and all three are
verified to fire:

| Layer | Mechanism | Verified |
|---|---|---|
| Go compiler | BONNIE's module path is not a prefix of Kit's, so the `internal` rule applies | `use of internal package ... not allowed` |
| `depguard` | `.golangci.yml` denies `kit/internal`, `charm.land/fantasy`, and the TUI stack | configured |
| CI | `.github/workflows/ci.yml` job `boundary` | catches a planted violation |

The CI job inspects **direct** imports, not `go list -deps`. The transitive
graph always contains 18 `kit/internal/*` packages because `pkg/kit` imports
its own internals — that is Kit's business, not a BONNIE violation. The job
also needs `go list -e`, because without it `go list` aborts on the very
package it is meant to report.

If Kit does not export something BONNIE needs:

1. Check the public API again. It usually can.
2. File an issue on Kit to export it from `pkg/kit`.
3. Only then add a local workaround, marked `// TODO(kit):`.

BONNIE is Kit's first serious external consumer. Every gap it hits is a gap
real SDK users hit. That pressure is the point.

---

## 3. Verified facts about Kit

These were confirmed by reading Kit at the commit pinned in `go.mod`
(`v0.105.0`). **Re-verify with the cited file:line before relying on any of
them**, and update this section if a Kit upgrade changes them.

### 3.1 `Options.SessionManager` must be set at construction

`pkg/kit/kit.go:2038-2051` — `kit.New` calls `InitTreeSession(opts)` when
`Options.SessionManager` is nil, which creates a session file on disk.

Consequence: calling `kit.NewAgent(...)` and then `SetSessionManager(s)` leaks
a stray Kit session on every run. `runtime.KitAgent` therefore applies the
options to a `kit.Options` value and calls `kit.New` with `SessionManager`
already set. `kit.Option` is `func(*Options)` (`pkg/kit/options.go:9`), so
applying them by hand is supported.

This bug existed in the first scaffold and is fixed. Do not reintroduce it.

**Verified live.** `TestLiveSuspendAndResume` counts `kit.ListSessions("")`
before and after a real two-process run. The count does not change.

### 3.2 `Halt` does not orphan a tool call

`pkg/kit/tools.go:265-275` — `recordHalt(ctx, name, result)` runs **before**
`toolOutputToResponse(result)` returns. The tool still produces a normal
`ToolResponse`, so the assistant message and its tool result are both emitted.
The loop stops after the step completes.

The suspension protocol is therefore sound: a halted turn leaves a
well-formed conversation that a provider will accept on resume.

**Verified live.** `TestLiveSuspendAndResume` parks a real model on
`ask_human`, restores the journal in a second process, and asserts that the
replayed conversation holds an equal number of tool calls and tool results.
The provider then accepts that conversation on resume.

### 3.3 Step messages are persisted incrementally — but one at a time

`internal/agent/agent.go:946-958` — `OnStepFinish` appends `step.Messages` to
the accumulator, calls `cb.OnStepMessages(step.Messages)`, and only **then**
checks `ctx.Err()`. So a cancelled turn keeps its completed steps.

`pkg/kit/kit.go:2990-2994` — Kit's handler is:

```go
OnStepMessages: func(stepMessages []fantasy.Message) {
    for _, msg := range stepMessages {
        _, _ = m.session.AppendMessage(msg)
    }
},
```

It loops. The assistant message (carrying `tool_use`) and the tool-role message
(carrying `tool_result`) arrive as **two separate `AppendMessage` calls**.

This matters — see §4.1.

### 3.4 The `SessionManager` contract

`pkg/kit/session.go:26-110` — 21 methods. The doc comment on `AppendMessage`
(lines 30-35) promises that for tool-calling steps the assistant and tool
messages "are appended together as a pair", so "the session never contains an
orphaned tool call without its result, which would break subsequent LLM
requests."

Kit honours this **within a process**. It does not, and cannot, honour it
across a crash — see §4.1.

`runtime/session.go` carries `var _ kit.SessionManager = (*Session)(nil)` as a
deliberate tripwire: if Kit widens the interface, BONNIE's build breaks there
first.

### 3.5 `*kit.Kit` satisfies `runtime.Agent`

Verified by compilation. `runtime/runner.go` carries
`var _ Agent = (*kit.Kit)(nil)`. The seam is three methods: `PromptResult`,
`InjectSteer`, `Close`.

### 3.6 `AppendExtensionData` / `GetExtensionData` is a durable KV store

Part of `kit.SessionManager`, branch-aware, already public. This is BONNIE's
equivalent of eve's [`defineState`](https://eve.dev/docs/concepts/state) and
needs no upstream change.

---

## 4. Known risks

### 4.1 RESOLVED — replay fidelity

**Fixed. Regression test: `runtime/replay_fidelity_test.go` (active, not
skipped). Keep it passing.**

An earlier `Restore` rebuilt every journalled message as a single
`kit.LLMTextPart`, because `Record` carried only flattened text. A probe
recovered **zero** tool calls from a message that had one. A resumed run
therefore had no memory of what it called or what came back, and could repeat a
side effect it had already performed. This fired on every resume of a
tool-using run, not only after a crash.

`Record.Payload` now carries the JSON-encoded `kit.LLMMessage`, and `Restore`
decodes from it, falling back to `Text` for records written before the change.
`Record.Text` is now explicitly a display-only projection — see the doc comment
on `Record` in `runtime/journal.go`.

The same change fixed a latent ID-collision bug: `Restore` now sets
`s.ids.n = maxSeq`, so a session that keeps appending after a resume cannot
reissue a replayed entry ID.

### 4.2 RESOLVED — torn write orphans a tool call

**Fixed by T-003. Regression tests: `runtime/repair_test.go` and
`TestFileJournalCrashResumeIsProviderValid`. Keep them passing.**

Kit calls `AppendMessage` once per message (§3.3). BONNIE's
`Session.AppendMessage` performs one journal `Append` per call. So a
tool-calling step produces two journal records:

```
record N    : assistant message containing tool_use   ← crash here
record N+1  : tool message containing tool_result
```

If the process dies between them, a replay rebuilds a conversation whose last
assistant message has an unanswered `tool_use`. Providers reject that. The run
would become permanently unresumable — the exact failure Kit's own docstring
warns about, reintroduced at the journal layer.

The in-process pairing guarantee does not survive a crash, so BONNIE repairs.

**The mitigation.** `Restore` calls `repairTrailingOrphan`
(`runtime/repair.go`). It walks the replayed messages; when a tool call has no
matching result **and every later message is a tool result**, it drops that
whole step and journals a `RecordRepair`. Dropping is correct: the step never
finished, so re-running it is the intended semantics. A partially answered
multi-call step is dropped whole, because keeping the answered calls would
leave the same orphan.

A mismatch anywhere but the tail is not a torn write. It means the journal is
damaged, and `Restore` returns `ErrCorruptConversation` rather than silently
rewriting history.

The journal is append-only, so the orphan records stay on disk and every later
`Restore` finds them again. The repair is therefore deterministic, and it is
journalled only once.

**Still worth filing upstream.** A batch-append hook on `SessionManager` would
let an implementation write a step atomically and remove the need for the
repair. See `docs/UPSTREAM.md` ask 1; it is the strongest of them.

### 4.3 RESOLVED — nothing has run against a live model

**Closed by T-001.** `runtime/integration_test.go`, behind the `integration`
build tag, runs the whole claim against a real provider: a run parks on
`ask_human`, the journal is closed and reopened, a second `Runner` resumes it,
and the run completes. It also asserts that Kit opened no session of its own.

Run it with:

```bash
go test -race -tags integration ./runtime
```

It skips, and does not fail, when no provider credential is present.

**Findings.** No divergence from §3. Every claim there held against
`anthropic/claude-sonnet-4-5`. §3.1 and §3.2 now carry a "Verified live" note.

### 4.4 RESOLVED — `Runner` cannot cancel

**Closed by T-005.** `Runner.Cancel(runID)` stops the turn a run is executing
and checkpoints `RunCancelled`. `Runner.Steer(runID, msg)` injects a message
into a turn that is already running. Both return `ErrRunNotActive` when this
Runner is not executing the run.

`RunCancelled` is a separate state from `RunFailed`, because a cancelled run
is resumable in principle: §3.3 shows Kit persists a step's messages before it
checks `ctx.Err()`, so a cancelled turn keeps its finished steps and restores
to a provider-valid conversation. `Runner.Start` continues it.

Terminal bookkeeping uses `context.WithoutCancel`, or a cancel would leave the
run in `running` for ever.

### 4.5 RESOLVED — `IsPersisted()` is a type assertion

**Closed by T-004.** `Persisted() bool` is now part of the `Journal`
interface. `Session.IsPersisted` delegates to it and asserts nothing.

### 4.6 RESOLVED — `MemoryJournal.Checkpoint` writes two things

**Closed by T-004.** Both journals now write the state move and the record
that documents it under one lock, so a reader never sees a state with no
record behind it.

### 4.7 OPEN — one process must own a run

`FileJournal` serialises writes inside a process with a per-run mutex. It
takes no cross-process lock. Two processes that append to the same run
interleave records, and the sequence numbers they assign collide.

This is a deployment constraint for `v0.1.0`, stated in `README.md` and
`SECURITY.md`. A lock file, or a journal backed by a database, removes it.

### 4.8 OPEN — events are not durable

`EventBus` keeps a bounded in-memory backlog per run (`DefaultEventBuffer`,
1024 events) so a client that drops its connection can reconnect with a cursor
and lose nothing. A client that reconnects after more than that many events
sees a gap.

This is the right split — the journal is the durable record, events are a live
view — but a consumer that needs every event must read the journal, not the
stream.

### 4.9 PARTLY RESOLVED — no sandbox, observed in practice

**The `sandbox` package closes this. It is opt-in, so the risk returns for any
host that does not switch it on.**

BONNIE runs the tool calls a model chooses. It is worth recording how fast that
bites.

The first version of the T-001 live test built the agent with Kit's default
core tools — shell, write, edit — in the repository working directory, and
told the model to "deploy the app". The model answered the question, and then
wrote a `Dockerfile`, a `terraform/` directory, `deploy.sh`, and three
deployment documents into the checkout. Nothing failed. Nothing warned. The
test passed.

Two mitigations now exist.

**For tests.** Any live test must give the model no tool that can touch the
disk. `noCoreTools()` sets `Options.DisableCoreTools`, and
`isolatedWorkspace(t)` runs the test in an empty temporary directory and fails
if anything appears in it.

**For hosts.** `sandbox.Agent(provider, ...)` replaces Kit's core tools with a
set that proxies into a container or a microVM. See `docs/SANDBOX.md`.
`TestLiveAgentCannotReachTheHost` is the regression test: it puts a marker file
on the host, asks a real model to read it, and fails if the model succeeds.

The residual risk is that sandboxing is opt-in. A host that does not pass
`--sandbox`, or does not call `sandbox.Agent`, runs tool calls as its own
process. `README.md` and `SECURITY.md` both say so.

### 4.10 OPEN — sandbox lifecycle is not journalled

A run's conversation is durable; its sandbox workspace is not recorded
anywhere. Nothing writes a journal record when a sandbox is created, stopped,
or deleted.

Consequences:

- `bonnie runs show` does not say which backend a run used, or whether its
  workspace still exists.
- A run whose container was pruned resumes with an empty workspace and no
  explanation. The model sees its files vanish between turns.
- Nothing cleans up the container of a completed run, so a long-lived server
  accumulates them.

The fix is a `RecordSandbox` entry carrying the backend name and the sandbox
ID, written when a sandbox opens, plus a reconciler that deletes the sandboxes
of terminal runs. That is T-012.

### 4.11 PARTLY RESOLVED — the microsandbox adapter now runs

`sandbox/microsandbox.go` was written and compiled but had never executed:
`msb` was not installed on the development machine, so every microsandbox
conformance case skipped rather than passed.

Adding `msb` to the Nix flake (`44116e3`) put the binary on the PATH, so the
cases stopped skipping and started running. **Three of the 18 failed at once.**
The adapter had shipped with two defects that only execution could find, both
in the assumptions this section told the reader to confirm:

- **`msb` reports an absent path as `error: stat <path>`.** The shared
  `isMissingPath` looks for `no such file` or `not found`, so `ReadFile` never
  returned `ErrNotFound` for a missing file. Fixed by `msbMissingPath`, which
  anchors the match on the path. Anchoring matters: `error: sandbox not
  found: <name>` *does* satisfy the generic helper, so a **vanished workspace**
  would have been reported to the model as an ordinary missing file. Guard
  test: `TestMsbMissingPathSeparatesAbsentFileFromAbsentSandbox`.
- **`msb ps` lists only running sandboxes.** `exists()` therefore answered
  false for a sandbox that `Stop` had released, `Open` tried to create it
  again, and `msb` refused with `sandbox already exists`. A run that did
  nothing worse than park between turns was stranded. Fixed with `--all`, and
  the listing is now JSON-decoded on the `name` field instead of substring
  matched, so a value in the image, command, or status field cannot produce a
  false positive.

A third defect surfaced in the same run: `msb rm` refuses to remove a running
sandbox, and `Delete` failures are usually ignored, so the conformance suite
leaked **18 running microVMs, about 9 GiB, per run**, in silence. `Delete` now
passes `--force`. After the fix three consecutive suite runs leave zero.

Verified on Linux with KVM, `msb` 0.6.18, on 2026-09-12: all 18 conformance
cases pass for the microsandbox backend, none skipped. `msb cp` carries binary
content unaltered (`TestBinaryFileRoundTrip`), which settles the file I/O
question this section raised.

Still unverified, so T-013 stays open:

- The live sandbox suite (`-tags integration`) has not been run against
  microsandbox.
- `SetNetworkPolicy` still refuses every policy except allow-all. Wiring it to
  `msb` for real is the remaining work.
- Verified on Linux/KVM only, not macOS on Apple Silicon.

A related bug was found and fixed while writing this section originally: the
adapter accepted `deny-all` and an allow-list, stored the policy in a field,
and never read that field again. A caller asking for no egress received a nil
error and full network access — the exact failure `ErrPolicyUnsupported` exists
to prevent. It now refuses any policy it cannot apply.

The general lesson is worth keeping: an adapter that compiles and skips is not
an adapter that works. Three real defects sat behind a green suite because the
only thing proving them absent was a skip.

---

## 5. MVP scope

### The claim `v0.1.0` makes

> Durable agent runs on Kit. A run survives process death and can park
> indefinitely for human input. Reachable over HTTP.

If the release cannot do those three things, it is not worth tagging.

### In scope

| Capability | Task |
|---|---|
| Live-model verification of L1 | T-001 ✅ |
| Lossless replay of tool calls | T-002 ✅ |
| Crash-safe replay (torn-write repair) | T-003 ✅ |
| `FileJournal` — JSONL persistence | T-004 ✅ |
| `Runner.Cancel` | T-005 ✅ |
| `channel/http` — start, send, respond, NDJSON stream | T-006 ✅ |
| `bonnie serve` / `bonnie runs` | T-007 ✅ |
| Runnable examples | T-008 ✅ |
| Isolated tool execution (`sandbox/`) | ✅ added after the original scope |
| Upstream Kit issues | T-009 — drafted, not filed |
| Release mechanics | T-010 ✅ · T-011 — config ready, not tagged |

### Out of scope — deliberately

L2 discovery and codegen · evals · OpenTelemetry · Slack/GitHub/Discord/
Telegram channels · scheduler · memory providers · multi-tenancy ·
subagent orchestration · structured output · web client SDK.

All are valuable. None is load-bearing for the claim. A narrow true v0.1 beats
a broad shaky one.

**Sandboxing was originally on this list and came off it.** The live-model
test in T-001 gave a real model Kit's core tools in the repository working
directory, and it wrote Terraform into the checkout (§4.9). A framework that
executes model-chosen tool calls and ships no way to contain them is not
narrow, it is incomplete. `sandbox/` adds no dependency, so the cost of
including it was close to zero.

---

## 6. Upstream asks for Kit

File these as issues on `mark3labs/kit` **before** tagging `v0.1.0`, so the
release documents its own assumptions.

The full text of each, with the evidence and the file:line citations, is in
[`docs/UPSTREAM.md`](UPSTREAM.md). They are drafted and ready to paste; put the
issue links next to the titles there and here when you file them.

1. **Batch append on `SessionManager`** (§4.2) — without it, no external
   session manager can be crash-safe across a tool-calling step. Highest
   value. BONNIE's `runtime/repair.go` is the evidence.
2. **`PrepareStepResult.Tools []Tool`** — the doc comment on `PrepareStepHook`
   in `pkg/kit/hooks.go` already advertises "dynamic tool filtering per step",
   but the result struct cannot express it. Needed later for eve-style
   [dynamic capabilities](https://eve.dev/docs/guides/dynamic-capabilities).
3. **Stability promise on `ToolOutput.Halt` + `FinalValue`** — currently reads
   as a stop-early convenience. BONNIE's entire suspension protocol depends on
   it being stable.
4. **Stability policy for `kit.SessionManager`** — adding a method breaks every
   external implementer. BONNIE's compile-time assertion is the tripwire.

---

## 7. eve analogues

eve solves the same problems on a different substrate (TypeScript, Vercel
Workflow). Its docs are a good source of prior art for naming, semantics, and
edge cases. Consult them for **design questions**, not for implementation.

| BONNIE | eve | eve doc |
|---|---|---|
| `runtime.Run`, `RunState` | session + turn, durable checkpoints | [Execution Model and Durability](https://eve.dev/docs/concepts/execution-model-and-durability) |
| `runtime.Journal` | Workflow SDK world (`@workflow/world-postgres`) | [Execution Model and Durability](https://eve.dev/docs/concepts/execution-model-and-durability) |
| `RunWaiting` + `SuspendRequest` | `session.waiting`, park and resume | [Human-in-the-Loop](https://eve.dev/docs/human-in-the-loop) |
| `AskTool` | `ask_question` built-in tool | [Built-in Tools](https://eve.dev/docs/concepts/built-in-tools) |
| `ApprovalTool` | tool `approval` policies | [Human-in-the-Loop](https://eve.dev/docs/human-in-the-loop) |
| `channel.Channel`, `Inbound` | `defineChannel`, routes | [Custom Channels](https://eve.dev/docs/channels/custom) |
| `channel.SessionRef.From` / `Attach` | `from(address)` vs `attachSession(id)` | [Custom Channels](https://eve.dev/docs/channels/custom) |
| `channel.TurnPolicy` | `turnPolicy: "steer" \| "queue"` | [Custom Channels](https://eve.dev/docs/channels/custom) |
| `channel/http` NDJSON stream | `GET /eve/v1/session/:id/stream` | [Sessions, Runs & Streaming](https://eve.dev/docs/concepts/sessions-runs-and-streaming) |
| `channel.Principal` | `SessionAuthContext` | [Authentication](https://eve.dev/docs/channels/eve) |
| `AppendExtensionData` | `defineState` | [State](https://eve.dev/docs/concepts/state) |
| Kit compaction (inherited) | `compaction.thresholdPercent` | [Default Harness](https://eve.dev/docs/concepts/default-harness) |
| *deferred* | `defineEval`, `eve eval` | [Evals](https://eve.dev/docs/evals/overview) |
| *deferred* | `instrumentation.ts` | [Observability](https://eve.dev/docs/guides/instrumentation) |
| *deferred* | `defineDynamic` | [Dynamic Capabilities](https://eve.dev/docs/guides/dynamic-capabilities) |
| *deferred* | `defineRemoteAgent` | [Remote Agents](https://eve.dev/docs/guides/remote-agents) |

Full text for offline reading: `https://eve.dev/llms-full.txt` (~1.1 MB).
Page index: `https://eve.dev/sitemap.md`.

### Where BONNIE deliberately differs

- **Single static binary.** No Node, no bundler, no Workflow service, no
  `dist/`. Build and copy. eve structurally cannot offer this.
- **Branching sessions.** BONNIE inherits Kit's conversation tree — fork,
  collapse, navigate. eve has no branching at all.
- **Pluggable durability.** `Journal` is six methods. eve is coupled to the
  Workflow SDK.
- **Hooks can mutate.** Kit's hooks can block a tool call, rewrite a result, or
  replace the context window. eve's `defineHook` is observation-only.

Do not copy eve's filesystem-convention layer into the MVP. It is L2 and
explicitly deferred.

---

## 8. Invariants

Any change must preserve these. Each has, or must gain, a test.

1. **Public API only.** No direct import of `kit/internal/...`, and no direct
   import of `charm.land/fantasy`.
2. **Headless.** No bubbletea, lipgloss, or huh anywhere in BONNIE.
3. **Append-only journal.** `Replay` is deterministic for a given run.
4. **Restore yields a provider-valid conversation.** No orphaned `tool_use`
   (§4.2), and no lost tool calls (§4.1).
5. **Resume works across processes.** A run suspended by one `Runner` completes
   in a second `Runner` sharing only the journal.
6. **`Agent` stays an interface.** Never narrow it to `*kit.Kit`; it is what
   makes L1 testable without credentials. Optional capability, such as event
   streaming, goes through a type assertion for a narrow optional interface
   (`eventSource` in `runtime/events.go`), never by widening `Agent`.
7. **L1 does not import L3.** `runtime/` must not depend on `channel/`.
8. **Kit opens no session of its own.** `Options.SessionManager` is always set
   before `kit.New` (§3.1).
9. **Reserved runs stay out of operator listings.** A transport that keeps
   bookkeeping in the journal uses [`runtime.ReservedRunPrefix`], and anything
   operator-facing filters with `runtime.IsReservedRun`.
10. **A sandbox never caches a tool call.** Every `Exec` must really run the
    command. A memoizing backend makes a repeated side effect invisible to the
    model. `TestEveryCallExecutes` enforces this for every adapter.
11. **A backend that cannot enforce a security control refuses it.** Returning
    success for a network policy nothing applies tells an operator they are
    protected when they are not. BONNIE broke this rule once, in
    `sandbox/microsandbox.go`, and §4.11 records it.

---

## 9. Conventions

- Godoc on every exported symbol. BONNIE is a public framework; the doc comment
  is part of the API.
- Errors wrapped `fmt.Errorf("bonnie: context: %w", err)`.
- Sentinel errors at package level, tested with `errors.Is`.
- `t.Parallel()` by default.
- Imports grouped stdlib → third-party → local.
- Commits: conventional prefixes (`feat:`, `fix:`, `docs:`, `test:`).
- Every durability claim needs a test that crosses a process boundary in
  spirit — build a second `Runner` sharing only the journal.

Full guidance in `AGENTS.md`.
