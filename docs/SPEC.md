# BONNIE MVP Specification

**Status:** released through `v0.4.0` · **Audience:** implementing agent

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
L4  CLI, evals, traces                 CLI implemented, incl. the built-in TUI
                                       (bonnie dev / bonnie chat); evals planned
L3  channel/    inbound transports     channel/http implemented
L2  discovery   agent/ tree + codegen  default layout + init + codegen;
                                       configuration is code (T-024)
L1  runtime/    durable run executor   implemented, memory + SQLite journals
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
| `depguard` | `.golangci.yml` denies `kit/internal` and `charm.land/fantasy` | configured |
| CI | `.github/workflows/ci.yml` job `boundary` | catches a planted violation |

The `charm.land/fantasy` denial is about **model types**, not the terminal
stack. The CLI's built-in TUI (`cmd/bonnie/tui`) imports
`charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, and `charm.land/lipgloss/v2`
directly — the charm v2 TUI libraries — which is allowed because a terminal
surface that renders Kit's model types to the screen is a separate concern
from naming the model types. It still never names fantasy or Kit internals;
see §8 invariant 1.

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
(`v0.106.0`). **Re-verify with the cited file:line before relying on any of
them**, and update this section if a Kit upgrade changes them.

`v0.106.0` is the release that answered every ask in `docs/UPSTREAM.md`
(PR `mark3labs/kit#135`, "durability seams for external SessionManager
implementations"). All four changes are additive; the upgrade from
`v0.105.0` broke nothing.

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

`pkg/kit/tools.go:212-214` defines `recordHalt`; the call sites at
`pkg/kit/tools.go:295` and `:312` (one per tool constructor) run it **before**
`toolOutputToResponse(result)` returns. The tool still produces a normal
`ToolResponse`, so the assistant message and its tool result are both emitted.
The loop stops after the step completes.

The suspension protocol is therefore sound: a halted turn leaves a
well-formed conversation that a provider will accept on resume.

`v0.106.0` states this as contract: `ToolOutput`'s godoc now says `Halt` plus
`FinalValue` is a supported suspension mechanism and that a halted tool call
still emits a well-formed tool result. BONNIE's suspension protocol no
longer rests on an implementation detail.

**Verified live.** `TestLiveSuspendAndResume` parks a real model on
`ask_human`, restores the journal in a second process, and asserts that the
replayed conversation holds an equal number of tool calls and tool results.
The provider then accepts that conversation on resume.

### 3.3 Step messages are persisted incrementally — and, since v0.106.0, atomically when the session allows it

`internal/agent/agent.go:946-961` — `OnStepFinish` appends `step.Messages` to
the accumulator, calls `cb.OnStepMessages(step.Messages)`, and only **then**
checks `ctx.Err()`. So a cancelled turn keeps its completed steps, and the
context handed to persistence may already be cancelled.

`pkg/kit/kit.go` — every site that persists more than one message now routes
through `appendMessages`, which type-asserts for [kit.StepAppender] and falls
back to a per-message loop only when the session manager does not implement
it:

- `kit.go:2996` — per-step persistence during generation
- `kit.go:3141` — `persistGenerationRemainder`, the tail of a finished or
  overflow-retried turn, which can carry a complete assistant + tool pair
- `kit.go:3205` — pre-generation messages

BONNIE's `Session` implements `kit.StepAppender` (`runtime/session.go`), so a
tool-calling step reaches the journal as **one call** and `SQLiteJournal`
commits it as one transaction. §4.2 records what this closes.

`appendMessages` deliberately ignores errors, matching the historical
behaviour of the call sites it replaced: a persistence failure must not abort
a turn that has already produced model output. The consequence for BONNIE is
that a journal write failure is silent to Kit — the runner's own error
reporting is the only place it can surface.

### 3.4 The `SessionManager` contract

`pkg/kit/session.go:36` — **20 methods**, not 21 as this section previously
claimed; the count was wrong at `v0.105.0` too and was corrected when
`v0.106.0` was verified. The doc comment on `AppendMessage` promises that for
tool-calling steps the assistant and tool messages "are appended together as
a pair", so "the session never contains an orphaned tool call without its
result, which would break subsequent LLM requests" — and, since `v0.106.0`, it
also states plainly that this ordering is not atomicity, and points to
`StepAppender` for that.

`v0.106.0` settled the stability question (UPSTREAM ask 4): the interface is
**frozen for `v0.x`**, and new capability arrives through optional interfaces
that Kit type-asserts for. `StepAppender` is the first instance and sets the
precedent. An implementer can now tell which world it lives in.

`runtime/session.go` carries `var _ kit.SessionManager = (*Session)(nil)` as a
deliberate tripwire: if Kit ever breaks the freeze and widens the interface,
BONNIE's build fails there first. A second assertion,
`var _ kit.StepAppender = (*Session)(nil)`, proves BONNIE takes the atomic
step path.

### 3.5 `*kit.Kit` satisfies `runtime.Agent`

Verified by compilation. `runtime/runner.go` carries
`var _ Agent = (*kit.Kit)(nil)`. The seam is three methods: `PromptResult`,
`InjectSteer`, `Close`.

### 3.6 `AppendExtensionData` / `GetExtensionData` is a durable KV store

Part of `kit.SessionManager`, branch-aware, already public. This is BONNIE's
equivalent of eve's [`defineState`](https://eve.dev/docs/concepts/state) and
needs no upstream change.

BONNIE writes two entries of its own: `bonnie.origin` (the channel and the
kind of surface a run's conversation lives on) and `bonnie.title`. Both are
written once, on the first turn that names them, by `Runner.Start`
(`runtime/context.go`). `runs list` shows the title.

### 3.7 `OnContextPrepare` runs once per turn and its result is not persisted

`kit.go:3216` (v0.106.0): after `BuildContext`, before `generate`, the hook
runs once with the assembled window, and a non-nil result replaces the
window for that call only. Nothing the hook adds reaches `AppendMessage`.

That is the property per-turn context needs. `runtime.Input.Context` is a
list of strings a channel hands the runner with one message — the event that
fired, the diff a comment refers to, who is speaking. `Runner.Start` journals
it as a `RecordContext` (no entry ID: run metadata, not a tree entry) and
sets it on the session; the hook registered in `attachCheckpoints` puts it in
front of the last user message as user-role messages with a `[context] `
prefix, and the run's origin ahead of that. `Restore` skips the record, so a
resumed run replays the conversation without the context, and a later turn
never sees an earlier turn's. Guard tests: `runtime/context_test.go`,
`TestReplayKeepsContextOutOfTheConversation`.

The placement rule is a pure function, `prepareContext`, so it is tested
without Kit. `Runner.Resume` carries no context: an answer to a question is
the answer.

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

**Fixed three times, and the third closed it.** First by T-003's repair
(`runtime/repair.go`), which stays. Then by the upstream fix BONNIE asked
for: Kit `v0.106.0` added `kit.StepAppender` and BONNIE's `Session`
implements it, so a tool-calling step arrives as one call. Then by T-025,
which made the journal SQLite: that one call is now one **transaction**, and
a transaction has no prefix. Regression tests: `runtime/repair_test.go`,
`TestSQLiteJournalCrashResumeIsProviderValid`,
`TestSQLiteJournalAppendStepIsAllOrNothing`, and the step tests in
`runtime/step_append_test.go`. Keep them all passing.

**The hazard, for the record.** A tool-calling step produces two journal
records — an assistant message containing `tool_use`, then a tool message
containing `tool_result`. Written separately, a crash between them leaves a
replay whose last assistant message has an unanswered `tool_use`. Providers
reject that conversation. The run becomes permanently unresumable — the exact
failure Kit's own docstring warns about, reintroduced at the journal layer.

**What each step closed.** The JSONL journal narrowed the window from "any
crash between two fsyncs" to "a torn single `Write`", which is as small as an
append-only file can make it — but a short write could still land a prefix of
the step on disk. SQLite removes that residue: the step commits or it does
not, and a crash before the commit leaves the write-ahead log unclaimed. A
cancelled context never dropped a completed step either: Kit persists before
it checks `ctx.Err()`, and `Session.AppendStep` writes under
`context.WithoutCancel`, per the contract `v0.106.0` documents on
`kit.StepAppender`.

**Why the repair stays.** Two sources still produce the torn shape, and
neither is hypothetical:

1. Journals written before the upgrade — every run created by an older
   BONNIE, on disk today, and carried into the database by
   `importJSONLRuns` exactly as it was found. The import does not repair;
   `Restore` still does, at the layer that understands conversations.
2. Journals whose implementation does not provide BONNIE's `StepJournal` —
   `Session` falls back to per-record writes for them, with the old window.

`Restore` therefore still calls `repairTrailingOrphan`
(`runtime/repair.go`). It walks the replayed messages; when a tool call has
no matching result **and every later message is a tool result**, it drops
that whole step and journals a `RecordRepair`. Dropping is correct: the step
never finished, so re-running it is the intended semantics. A partially
answered multi-call step is dropped whole, because keeping the answered
calls would leave the same orphan.

A mismatch anywhere but the tail is not a torn write. It means the journal is
damaged, and `Restore` returns `ErrCorruptConversation` rather than silently
rewriting history.

The journal is append-only, so the orphan records stay in the store and every
later `Restore` finds them again. The repair is therefore deterministic, and
it is journalled only once.

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

### 4.7 RESOLVED — one process must own a run

**Resolved twice. T-014 mitigated it with a lock file; T-025 removed it by
making the journal a database — the option T-014 weighed and deferred.**

The original problem: `FileJournal` serialised writes inside one process with
a per-run mutex and took no cross-process lock, so two processes appending to
one run interleaved records and gave the same sequence number to different
ones. T-014's answer was an exclusive `flock` on
`<root>/runs/<run-id>.lock`, held from a run's first write until close or
process death, with a second owner's write refused as
`ErrRunOwnedElsewhere`.

`SQLiteJournal` does not need it. SQLite serialises write transactions across
processes, and the journal's `(run_id, seq)` primary key makes a reused
sequence number a **constraint violation** rather than silent corruption. The
sequence number is read and taken inside the same `BEGIN IMMEDIATE`
transaction that writes the record, so two writers cannot both see the same
next value. Concurrent writers are now *safe*, not *forbidden*.

What that changed, stated plainly:

- The lock file, `ownLocked`, and the `runFile` handle map are gone, together
  with the whole class of defects they carried (see §4.12: a read used to
  create a handle that was never released).
- `ErrRunOwnedElsewhere` stays **exported and documented** on the `Journal`
  seam, because it is still the right answer for a journal backed by a store
  that admits one writer. `channel/http` still maps it to 409.
  `SQLiteJournal` never returns it.
- `TestFileJournalOwnershipIsCrossProcess` and
  `TestFileJournalLockFileIsNotARun` were deleted with the mechanism they
  guarded. `TestSQLiteJournalAdmitsConcurrentWriters` replaces them, and it
  asserts the stronger property: two journals over one store, both writing
  one run, produce a dense unbroken sequence and lose no record.

Limits, stated plainly, because two of the old ones survive:

- **SQLite's locking is per host.** It needs working POSIX advisory locks; on
  a network filesystem without them the database can be corrupted, which is
  worse than the old failure, not better. Keep the journal on local storage,
  or write a `Journal` backed by a networked database.
- **Journal integrity is not turn coordination.** Two `Runner` instances that
  both execute a turn for one run now write a well-formed journal holding an
  interleaved conversation. That used to be refused at the first write; it is
  no longer. One owner per run is still a deployment decision, and
  `README.md` and `SECURITY.md` say so.

### 4.8 RESOLVED — events survive a reconnect past the backlog

**Resolved by T-016 with journal-backed catch-up.** The design that made the
old gap a fact of life was a split: the journal was the durable record, the
event stream a live view over a bounded in-memory backlog, and a client whose
cursor had fallen off the backlog edge saw the gap with nothing to do about
it. eve closes the same problem by making the stream itself the durable
record — "every event is recorded before a step completes" — with stable
`evt_` ids, an absolute `startIndex` cursor, and replay that returns the same
id for the same event.

BONNIE adopted the contract, not the substrate. Every event now carries the
**journal position it is anchored to**: a state event the Seq of its
`RecordState`, a suspend or resume event the Seq of its record, and a
response the Seq of the message record the turn's last text belongs to. Live
events forwarded from Kit mid-turn anchor to the journal position at publish
time; they are ephemeral by nature and are never replayed. The seqs are
therefore not dense — messages sit between the state records — but they rise,
never repeat, and mean the same thing on every path.

On reconnect, [Runner.StreamEvents] — the only path `channel/http`'s stream
uses — first subscribes to the bus, then replays the run's journal when the
cursor has fallen behind, and joins the live stream by dropping events whose
Seq the replay already covered. Because both paths derive from the same
records, the handoff is a filter, not a negotiation. A **process restart** is
covered too: a fresh Runner's bus remembers nothing, and the replay comes
entirely from the journal.

What replay cannot revive, it says so: Kit's mid-turn deltas are live-only,
and the response text of a turn whose agent journaled no assistant message
has no anchor to replay from. Both are documented on [Event], and neither
hides a gap in the durable events.

Tests: `runtime/events_test.go` (replay past a shrunken backlog, the restart
replay, the handoff without gap or duplicate), the stream tests in
`channel/http` (anchors on the wire, reconnect past the backlog at 18 events),
and the bus tests updated to the anchored contract.

### 4.8.1 RESOLVED — live events at one journal anchor were dropped

**Fixed in the event-stream join and covered by
`TestStreamEventsKeepsLiveEventsAtOneAnchor`.** Live Kit events use the current
journal position as an anchor. A tool-call start, parsed call, execution, and
result can all occur before the next journal write, so all can have the same
`Seq`. The old live-stream filter advanced its cursor after the first event and
dropped all later events with that `Seq`. This made the TUI lose tool calls even
though Kit emitted them and the event bus held them.

When no journal replay is in progress, `Runner.StreamEvents` now preserves all
live bus events in their original order, including events that share one
anchor. The cursor still selects the first anchor (`Seq > after`); it does not
make events at that anchor unique. During journal catch-up, events at or before
the replay end stay filtered because ephemeral Kit events cannot be inserted
reliably into replayed history.

The TUI folds a tool lifecycle into one compact entry: a spinner while the tool
works, a check mark when it completes, and an indented arrow with a one-line,
Unicode-safe truncated result. Tool call IDs correlate start, parsed-call, and
result events, so one call does not render as duplicate transcript lines.

**TUI restart.** A second defect had the same symptom: a TUI that reopened an
existing address learned its run ID only after the first turn, so it opened the
event stream too late and missed every tool and reasoning event of that turn.
The channel now exposes `GET /bonnie/v1/addresses/{address}` — a read-only lookup that
returns the bound run ID and its current journal cursor, and creates nothing on
a miss (the From/Attach rule of the HTTP adapter, applied to addresses). The
TUI resolves the address at startup and opens the stream from the served
cursor. Guard tests: `TestStartupLookupResumesToolStream`,
`TestAddressLookupDoesNotCreate`.

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

### 4.9.1 RESOLVED — the agent's root is the workspace, in both modes

**The same incident had a second cause, fixed separately.** Even with the
right intent, a tool call's *root* was wrong: Kit's file tools resolve a
relative path against `WorkDir`, falling back to `os.Getwd()`
(`internal/core/read.go:122-134`), and BONNIE never set it. So a model's
`write("notes.md")` landed in whatever directory the operator started the
server from — for a tree that is the tree itself, on top of `main.go`,
`instructions.md`, and `.bonnie/`, **the journal that makes a
run durable**. A sandboxed run rooted everything at `/workspace`
(`sandbox.Resolve`), so the two modes disagreed about what the agent's root
meant, and a prompt could not name a stable location.

The workspace is now the agent's root in both modes:

- **No sandbox** — `hostWorkspaceOptions` rebuilds Kit's core tool set with
  `kit.WithWorkDir(<root>/<workspace>)` and installs it through
  `kit.WithTools`. Kit's default core tools take no working directory:
  `WithWorkDir` is a `kit.ToolOption` and `kit.Options` has no field that
  forwards one, so rebuilding the set is the only public route.
  `kit.AllTools` is that same default set, so no tool is lost.
- **Sandbox** — the same directory is the seed: `sandbox.Seeded(provider,
  workspace)`. That call is what makes the workspace real with a sandbox;
  `Seeded` was written and tested but **never called** by non-test code, so
  the setting was accepted and ignored — an invariant 13 violation that had
  been recorded in `docs/TASKS.md` as working.

**The two must never be mixed, and that is a security property.** Kit honours
`Options.Tools` even when `DisableCoreTools` is set, and `sandbox.Agent`
applies the caller's options *after* its own (`sandbox/agent.go`) — so using
the host options on a sandboxed agent would hand the model real host tools
inside the sandbox. `hostWorkspaceOptions` is therefore called on the
no-sandbox branch only. Guard test: `TestSandboxedAgentGetsNoHostTools`.

Limits, stated plainly: `WithWorkDir` sets a base, **not a jail**. An absolute
path, or one with enough `../`, still reaches the wider filesystem. Rooting
the agent stops the accident — a model tidying up its own files does not
overwrite the source or the journal — it does not contain a determined one.
That is what the sandbox is for, and §4.9 still applies.

Serving without a tree (`bonnie serve`) has nothing to anchor to, so the
process's own directory stays the root, as before.

**The dev loop must not watch the workspace.** Rooting the agent there made a
model's write an ordinary event in a watched directory, so `bonnie dev`
rebuilt and SIGTERMed the child that was still serving the turn — the agent
restarted itself for doing its job. The workspace is the loop's output, like
`.bonnie` and `bonnie_gen.go`, and is skipped both by the watcher and by the
event filter (the second catches the directory's own create event, which the
child raises on first boot). Guard tests: `TestWorkspaceIsNotWatched`,
`TestWorkspaceDirIsTheRuntimeWorkspace`. Since T-024 the workspace path is
the constant `bonnie.DefaultWorkspace`, read by the scaffold, codegen, the
dev loop, and the runtime alike, so the four cannot disagree about which
directory must not be watched. Both guards were briefly lost with the
manifest and restored — see §4.13.

Verified live against `opencode/kimi-k2.5`: a `write` and a shell redirect
both landed in `workspace/` with the tree root untouched, `pwd` reported the
workspace, and under `--sandbox docker` the seed file arrived at `/workspace`.
Under `bonnie dev`, a model's write no longer restarts the child, while an
edit to `instructions.md` or a tool still does.
Tests: `cmd/bonnie/workspace_test.go`, `cmd/bonnie/dev_workspace_test.go`.

### 4.10 RESOLVED — sandbox lifecycle is journalled

**Resolved by T-012.** A `RecordSandbox` entry now carries the backend name,
the sandbox ID, and — when a loss was verified — a `gone` flag. Three
consumers read it:

- `bonnie runs show` prints the record in the timeline, so an operator can
  see which backend a run used and what held its compute. The raw payload is
  in `--json`.
- A resumed run whose workspace vanished gets **a note in the conversation**
  (`Session.NoteSandboxUnavailable`), written once per discovery, before the
  first step of the resumed turn. The decision: a vanished workspace is not a
  failure — a run whose container an operator pruned can still do useful
  work — but it must never be silent. The model watched its files vanish;
  the note tells it why, and tells it to re-create what it needs. The note is
  user-role with a `[bonnie]` prefix, because some providers reject a
  system-role message mid-conversation.
- `bonnie sandbox prune` walks the journal, finds runs in a terminal state,
  and deletes each one's sandbox through `sandbox.RunDeleter` — a delete that
  never opens, because opening would create. `--dry-run` reports without
  deleting.

Where the code sits, and why: the record kind and the note live in
`runtime/`, the code that writes them lives in `sandbox/` — the same layering
invariant 6 states for `channel/`. `sandbox.LazyOpener` takes the session now
and journals the open, retrying the record on every call until the journal
takes it; a bookkeeping failure fails the tool call that saw it, so the model
retries, and the record lands when the journal recovers. It is never silent
and never permanent.

Unverified losses are reported differently from verified ones. A backend that
says the sandbox is gone produces a gone record plus the note. A record that
points at a backend this host no longer uses — BONNIE cannot ask the other
backend — produces the note only, because nothing verified the loss.

The reconciler is a command, not a background sweep. A background sweep in
`serve` would be the complete answer for a long-lived server; **it does not
exist yet**, and `README.md` says so. The command is deliberately the first
step: it is the same code an operator can run from cron, and it is honest
about what it does not do.

Tests: `runtime/sandbox_record_test.go` (the record survives restore; the
note is written once; an unverified loss writes no gone record; the record
never joins the conversation tree), `sandbox/agent_test.go` (the opener
journals once, the retry, the four resume-check shapes), and
`cmd/bonnie/runs_test.go` (the timeline, and prune end to end).

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

**Correction, 2026-09-14: that pass is not reliable under load.** Running the
suite repeatedly on a loaded machine fails intermittently — measured at 2/6
runs on `master` and 4/6 on a branch whose only change was unrelated build
weight. The failure is always the same shape, on a different case each time:

```
create microsandbox from alpine:3.19: error: sandbox already exists:
sandbox 'bonnie-exec-codes' already exists
```

Nothing leaks: `msb ps --all` is empty before and after. So this is not the
reclaim defect fixed above — it is a **create/exists race inside one test**.
`Open` asks `exists()`, `msb ps --all` does not yet report a sandbox that is
mid-creation, `Open` creates, and `msb` refuses. The `--all` fix earlier in
this section closed the *stopped-sandbox* half of the question and left the
*being-created* half open. Load widens the window, which is why an unrelated
change can move the rate without touching the adapter.

This is invisible to CI, which has no `msb` and skips the backend — so a
green CI run says nothing about it, exactly as T-011's "Watch for" warns.
Recorded as **T-026**; not fixed here, because the fix belongs to the
microsandbox adapter and not to the change that exposed it.

Resolved, same day. The live sandbox suite ran against microsandbox and
**all four tests passed**:

- `TestLiveAgentWorksInsideTheSandbox` — a real model wrote a file, catted it,
  and reported its content, from inside the microVM, with nothing on the host.
- `TestLiveAgentCannotReachTheHost` — the §4.9 regression: the model could
  not read a host file.
- `TestLiveSandboxSurvivesSuspendAndResume` — park on a question, release the
  compute, resume in a second Runner sharing only the journal, files intact.
- `TestLiveParkedRunHoldsNoCompute` — no tool call, no microVM.

The model was `opencode/kimi-k2.5`, not Anthropic. The Anthropic account
returned 429 on every agent-sized request across five attempts (a single
small curl succeeded each time, so the limit sits on the shared workspace,
not the key), and the available OpenAI key is a ChatGPT/Codex account that
rejects `gpt-4.1`. The opencode gateway answered in seconds, and it is now
the live suites' first choice — a test model must not share a quota with a
busy workspace. Both live suites passed again with the new default and no
explicit override.

What is left for T-013 is hardware, not code — and T-013 is deferred: there is
no Apple Silicon machine to run it on, and everything code-side is verified
on Linux/KVM. Reopen the task when hardware appears.

- Verified on Linux/KVM only, not macOS on Apple Silicon.

**Update, same day: the network policy is wired and enforced.** `msb create`
carries `--no-net` and `--net-rule allow@<host>`, so `SetNetworkPolicy` maps
every mode onto create flags instead of refusing. Enforcement was verified
with real egress (`TestMicrosandboxEnforcesNetworkPolicy`): a control sandbox
reaches the internet, a deny-all sandbox does not, and an allow-list sandbox
reaches the listed host and nothing else.

One constraint the wiring exposed: `msb modify` cannot change network rules,
so policy is fixed at create time. A host that reconfigures its policy and
reattaches to an existing sandbox would silently run under the old rules —
the honesty trap again. `Open` therefore inspects the live policy
(`active_config.network.policy`) and returns the new `ErrPolicyMismatch`
when it differs from the configured one (`TestMicrosandboxRefusesReattachPolicyMismatch`).
The inspect shapes the comparison relies on are pinned as fixtures in
`TestPolicyMatches`, recorded from msb 0.6.18, so an msb upgrade that changes
them fails a test instead of breaking reattach quietly.

A related bug was found and fixed while writing this section originally: the
adapter accepted `deny-all` and an allow-list, stored the policy in a field,
and never read that field again. A caller asking for no egress received a nil
error and full network access — the exact failure `ErrPolicyUnsupported` exists
to prevent. It now refuses any policy it cannot apply.

The general lesson is worth keeping: an adapter that compiles and skips is not
an adapter that works. Three real defects sat behind a green suite because the
only thing proving them absent was a skip.

### 4.12 RESOLVED — what a read-only audit found

A full-repository audit on 2026-09-14 read every non-test file against the
invariants. It found no boundary violation and no journal-integrity hole:
`Record.Payload` is the only source a message is rebuilt from, nothing
rewrites or reorders records, and `repairTrailingOrphan` still touches the
tail alone. It found four defects worth recording, all fixed in the same
commit as this section.

**A stream leaked two goroutines per disconnect.** `Runner.StreamEvents`
forwarded events on an unbuffered channel. An HTTP client that went away
between two events left the forwarding goroutine parked on `out <- ev`, and
the bus subscriber's pump parked behind it: `unsubscribe` closed the
subscriber, but a goroutine blocked *in* a send never looks at the closed
flag, which is read before the send and never during it. Every reconnect
that raced an event cost a server two goroutines and their queued events,
for the life of the process — and the reconnect path is the one the TUI uses
most. The stop function now closes a `done` channel that every send selects
on, in the forwarder, in `replayEvents`, and in the pump. Tests:
`runtime/stream_leak_test.go`, which counts goroutines around 20 abandoned
streams, live and mid-replay. Without the fix the live case leaks 40.

**A read of an unknown run grew the journal for ever.** `FileJournal.run`
created the in-memory handle for any ID it was asked about, and nothing ever
deleted one. A server reachable from outside answers 404 to
`GET /bonnie/v1/runs/<invented-id>` and paid a permanent map entry for each one, so an
ID scan was unbounded memory growth with no run behind it. Reads went
through `readRun`, which threw its probe handle away when the run did not
exist and adopted it when it did. **T-025 deleted the whole mechanism**:
`SQLiteJournal` keeps no per-run handle, so the defect class is gone by
construction. `TestReadsOfUnknownRunsDoNotGrowTheJournal` and
`TestReadOfAKnownRunIsCached` went with the handle map;
`TestSQLiteJournalReadsOfUnknownRunsCostNothing` keeps the observable half —
a read of an unknown run answers `ErrRunNotFound` and creates nothing.

**Two settings were accepted and ignored** — the failure mode invariants 10
and 13 exist to prevent, found in two new places:

- `--sandbox-image` reached docker and microsandbox only. `--sandbox auto`
  built its candidates with no image, so an operator who named one got
  alpine, silently; `--sandbox local` took an image it cannot run. `auto`
  now passes the image to every candidate, and `local` refuses one. The old
  guard test asserted the provider's *name*, so it passed either way; it now
  asserts the image through the new `sandbox.Imaged` interface, which is
  also what lets the startup banner name the image in force.
- Reserved runs were addressable from a transport. `chat.Ref.RunID` accepted
  any ID with a state, and BONNIE's own address map lives in a run with a
  state, so a caller who knew `ReservedRunPrefix` could run a model turn
  inside the store every address binding lives in. `bonnie sandbox prune`
  listed the same run to operators as "kept". Both now filter. Tests:
  `TestReservedRunIsNotAddressable`, `TestSandboxPruneHidesReservedRuns`.

**`channel/http` buffered an unbounded request body.** The three webhook
adapters had always capped theirs at 1 MiB; the channel's own routes had no
cap. Added, with a 413 that names the limit, plus the missing
`ErrRunOwnedElsewhere` → 409 mapping: a second server hitting a locked run
answered 500, which reads as "retry" for a conflict no retry can fix.

The audit also removed `EventBus.Backlog`, which had no caller anywhere
(§4.8 states `StreamEvents` is the only path), and collapsed three copies of
the chat delivery text and three copies of the workspace-default rule into
`chat.DeliveryText` and a single workspace rule (`Manifest.WorkspaceDir`
then; `bonnie.DefaultWorkspace` since T-024). One claim had no test in the
shape §9 demands — a cancelled run continuing in a second process — and now
has one: `TestCancelledRunContinuesInASecondRunner`.

### 4.13 RESOLVED — deleting a feature deleted two guards with it

T-024 removed the manifest and the `serve --agent` path. A documentation
audit afterwards grepped every test name cited in the docs against the tests
that exist, and found citations with nothing behind them. Two were prose
debt; two were live invariants whose guard had been deleted along with the
test file that happened to contain it.

**The dev loop's workspace exclusion lost its only test.**
`cmd/bonnie/dev_workspace_test.go` held `TestWorkspaceIsNotWatched`, the
guard on §4.9.1 above, in the same file as a manifest-specific test. T-024
deleted the file for the manifest test and took the guard with it. The
behaviour never broke — `watchTree` and `watched` still skip the workspace —
but for two commits the repository asserted the fix in prose and tested
nothing, which is the state §4.9.1 exists to prevent: a model writing a file
would restart the child mid-turn, and only a live tmux session would say so.
Restored, and rewritten against `bonnie.DefaultWorkspace`. A second test,
`TestWorkspaceDirIsTheRuntimeWorkspace`, pins the loop's idea of the
workspace to the runtime's, which is invariant 14 pointed at the dev loop.

**The chat-channel wiring lost its only test.** The same commit deleted
`TestServeMountsChatChannels` and `TestChatChannelNeedsItsSecrets` with the
manifest-driven serve path that had carried them, and replaced them with a
test of the `require` helper alone. The helper is not the contract: the
contract is that `WithSlack`, `WithDiscord`, and `WithTelegram` each *call*
it before building a channel. An option that skipped the check would mount an
unverified webhook and no test would have failed.
`TestChatChannelOptionsMount` drives each option end to end — routes mounted
with the credentials present, refusal naming the variable with any one of
them empty.

**Writing that test found a real defect.** Each `With<Platform>` option
captured its `Config` by value and then filled the captured copy from the
environment:

```go
func WithSlack(cfg slack.Config) Option {
    return WithChannel(func(r *runtime.Runner) (Channel, error) {
        fill(&cfg.BotToken, "SLACK_BOT_TOKEN")   // writes into the closure
```

`fill` only writes when the field is empty, so the first call left the
credential *inside the closure* and every later call saw a non-empty field
and skipped the environment entirely. The option was therefore single-use:
reusing one across two agents, or calling `Agent.Run` twice, silently served
the credentials read at the first call rather than the ones set now — and a
credential that has since been removed from the environment would keep
working. Each option now copies its config per call and fills the copy.
Since `Option` values are ordinary Go values a user can hold in a variable
and pass twice, this was a public-API footgun, not a theoretical one.

The general lesson, recorded because it will recur: **a guard test's file
name is not its scope.** When a feature is deleted, grep the docs for the
test names in the files being removed before deleting them, and re-home any
guard whose invariant outlives the feature.

### 4.14 RESOLVED — the journal is SQLite

**T-025.** `FileJournal` — one append-only JSONL file per run, a lock file
beside it, a `runFile` handle in a map — is gone. `SQLiteJournal` replaces
it: one `<root>/journal.db`, opened with the **pure-Go** driver
`modernc.org/sqlite`.

**Why the pure-Go driver and not the usual one.** `github.com/mattn/go-sqlite3`
is CGO. `bonnie build` promises the user a single static binary and
`goreleaser` cross-compiles four targets from one machine; both stop working
the moment a C library enters the graph. The rule is enforced, not stated: a
`depguard` rule denies the CGO driver, and CI runs `CGO_ENABLED=0 go build
./...` as its own step.

**What the move bought.** Each of these was a section of this document:

| Was | Is |
|---|---|
| §4.2 — a torn single `Write` could land a prefix of a step | a step is one transaction; it commits or it does not |
| §4.7 — a second writer refused with `ErrRunOwnedElsewhere` | SQLite serialises writers; `(run_id, seq)` rejects a reused number |
| §4.12 — a read of an unknown run created a handle for ever | there is no per-run handle to create |

Decisions worth knowing:

- **The sequence number is taken inside the write transaction.** `SELECT
  COALESCE(MAX(seq),0)+1` runs in the same `BEGIN IMMEDIATE` that inserts the
  record, so no reader-then-writer race exists, and the primary key is the
  backstop if one ever did.
- **Pragmas are DSN parameters, not statements.** A `PRAGMA` executed after
  `sql.Open` applies to one pooled connection, so WAL, `synchronous`, and
  `busy_timeout` are set per connection through the DSN. Guard tests:
  `TestSQLiteJournalUsesWAL`, `TestSQLiteJournalFsyncPolicy`, which read the
  pragma back rather than trusting the struct field.
- **A payload that is not valid JSON is refused at write time.** The JSONL
  journal caught this for free, because encoding the record encoded the
  payload with it. A blob column takes anything, so `insertRecord` checks
  `json.Valid` — without it BONNIE could journal a record that is durable
  and unreplayable at once, which is §4.1 with extra steps.
- **`FsyncInterval` became `FsyncRelaxed`, and `WithFsyncInterval` is gone.**
  The old option promised "flushes at most once per interval". SQLite's
  `synchronous=NORMAL` fsyncs at WAL checkpoints, not on a clock, so keeping
  the name and ignoring the duration would have been invariant 13 in the
  public API.
- **Every legacy run is imported, never deleted.** `importJSONLRuns` runs on
  open when `<root>/runs/*.jsonl` exists: records keep their sequence
  numbers, the source file is renamed to `.jsonl.imported`, the lock file is
  removed, and a run already in the database is skipped — so a crash between
  the insert and the rename costs a second read. A file that cannot be
  parsed fails the open with the path named, because presenting an operator
  with a journal that silently lost a run is the one outcome a durability
  layer may not produce. Tests: `runtime/legacy_journal_test.go`.

**Verified live, not only in tests.** Against `opencode/kimi-k2.5` on
2026-09-14, through `bonnie dev`'s TUI in tmux — the surface a developer
actually uses, and the one that reads the journal through the event cursor:

- A turn with a real tool call renders and completes; the step lands in the
  journal as the assistant + tool pair.
- **A hot reload restarts the child mid-conversation and the run continues.**
  One run, `seq` 1–18, dense and unbroken across **three** processes: the
  first child, the child `dev` rebuilt after an `instructions.md` edit, and a
  separate `bonnie chat` process attached to the same address. The model
  recalled a tool result produced two process-generations earlier, which is
  §4.1's lossless replay proven end to end.
- `PRAGMA integrity_check` returns `ok`, and no run has a gap or a duplicate
  sequence number.
- The live suites pass: `TestLiveSuspendAndResume` and
  `TestLiveToolCallsSurviveOneProcess`.

One thing the tmux run clarified rather than found: a resumed run did not
adopt an edited `instructions.md` rule in its next reply, while a **fresh**
run did. That is in-context pressure from the replayed transcript, not a
stale prompt — worth knowing before someone reports it as a hot-reload bug.

**The cost, measured, not guessed.** Two dimensions, both paid once and
neither hidden:

- **Binary.** `modernc.org/sqlite` and `modernc.org/libc` add about
  **3.8 MB** to a stripped `bonnie` binary (77.4 MB → 81.1 MB,
  `CGO_ENABLED=0 -ldflags="-s -w"`, linux/amd64).
- **Cold build.** The driver is transpiled C, so it brings about **1.6
  million lines** of generated Go into the dependency graph. A cold-cache
  `go build ./...` goes from **98 s to 110 s** (+12%), and a cold-cache
  `go fix ./...` — which type-checks the whole graph — takes about **80 s**.
  Warm, both are unchanged: `go fix ./...` is 2 s. This is worth knowing
  because a 20-second tool timeout on a cold cache reports as a failure with
  no defect behind it. CI caches the module and build cache, so it pays this
  on a cache miss only.

The driver lives in `runtime`, so every host that imports the package links
it, even one that supplies its own `Journal`. That was accepted rather than
hidden behind a sub-package: splitting it would have moved the shared
conformance suite out of `runtime` and away from the helpers it tests
against.

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
| `FileJournal` — JSONL persistence (replaced by `SQLiteJournal`, T-025) | T-004 ✅ |
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

The L2 spec is now drafted: [`docs/L2.md`](L2.md) covers the agent tree, the
default layout, `init`/`dev`/`build`, and the codegen contract.

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

**All four answered by Kit `v0.106.0`** — PR `mark3labs/kit#135`, "durability
seams for external SessionManager implementations", released before BONNIE
filed anything. The asks were drafted in [`docs/UPSTREAM.md`](UPSTREAM.md),
whose record of what landed and where is archived in
`docs/archive/UPSTREAM.md`; nothing needs filing.

1. **Batch append on `SessionManager`** (§4.2) — landed as the optional
   `kit.StepAppender` interface, routed through `appendMessages` at all three
   persistence sites. BONNIE adopted it: `Session.AppendStep` plus the
   `StepJournal` seam on BONNIE's own `Journal` interface. Highest value, and
   the one the torn-write repair existed to mitigate.
2. **`PrepareStepResult.Tools []Tool`** — landed, with nil-versus-empty
   semantics documented (nil keeps the live tool set; empty offers none).
   Not yet used by BONNIE; L2 discovery will want it.
3. **Stability promise on `ToolOutput.Halt` + `FinalValue`** — landed in the
   godoc. BONNIE's suspension protocol is now contract, not coincidence.
4. **Stability policy for `kit.SessionManager`** — landed: the interface is
   frozen for `v0.x`, new capability arrives through optional interfaces.
   `StepAppender` is the precedent. The compile-time tripwire in
   `runtime/session.go` stays as enforcement.

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
| `channel/chat` dispatch, steering, delivery | Chat SDK `send`, turn policies, default handlers | [Chat SDK](https://eve.dev/docs/channels/chat-sdk) |
| `AppendExtensionData` | `defineState` | [State](https://eve.dev/docs/concepts/state) |
| `bonnie init` / `dev` / `build` | `eve init`, `npm run dev`, deploy | [Getting Started](https://eve.dev/docs/getting-started) |
| `main.go` options on `bonnie.New` | `agent/agent.ts` (`defineAgent`) — configuration is code in both. BONNIE shipped a declarative `agent.yaml` in `v0.2` and removed it in `v0.3` (T-024): two places for one setting is worse than requiring Go on the desk | [Getting Started](https://eve.dev/docs/getting-started) |
| Kit compaction (inherited) | `compaction.thresholdPercent` | [Default Harness](https://eve.dev/docs/concepts/default-harness) |
| *deferred* | `defineEval`, `eve eval` | [Evals](https://eve.dev/docs/evals/overview) |
| `channel/http` at `/runs` (root) — *T-030 moves it under `/bonnie/v1`* | `/eve/v1/*`, reserved for the framework | [eve channel](https://eve.dev/docs/channels/eve) |
| `runtime.Input{Text, Files}` — *no context slot; T-028* | `message` + per-turn `context` / `clientContext` | [Custom Channels](https://eve.dev/docs/channels/custom) |
| *T-031* | `operationId` create-once, `code` on errors | [eve channel](https://eve.dev/docs/channels/eve) |
| *T-032* | `reset`, `clear`, `compact`; channel-name prefix on every token | [Custom Channels](https://eve.dev/docs/channels/custom) |
| *T-033* | `to(channel, target).send`, `receive` (proactive) | [Custom Channels](https://eve.dev/docs/channels/custom) |
| *T-029* | `githubChannel`, `<github_context>`, PR diff in context | [GitHub](https://eve.dev/docs/channels/github) |
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
- **Pluggable durability.** `Journal` is seven methods. eve is coupled to the
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
2. **Append-only journal.** `Replay` is deterministic for a given run.
3. **Restore yields a provider-valid conversation.** No orphaned `tool_use`
   (§4.2), and no lost tool calls (§4.1).
4. **Resume works across processes.** A run suspended by one `Runner` completes
   in a second `Runner` sharing only the journal.
5. **`Agent` stays an interface.** Never narrow it to `*kit.Kit`; it is what
   makes L1 testable without credentials. Optional capability, such as event
   streaming, goes through a type assertion for a narrow optional interface
   (`eventSource` in `runtime/events.go`), never by widening `Agent`.
6. **L1 does not import L3.** `runtime/` must not depend on `channel/`.
7. **Kit opens no session of its own.** `Options.SessionManager` is always set
   before `kit.New` (§3.1).
8. **Reserved runs stay out of operator listings, and off the wire.** A
   transport that keeps bookkeeping in the journal uses
   [`runtime.ReservedRunPrefix`], anything operator-facing filters with
   `runtime.IsReservedRun` (`bonnie runs list`, `bonnie sandbox prune`), and
   no transport may attach to one: `chat.Ref.RunID` answers a reserved ID
   with `ErrRunNotFound`. Guard tests: `TestReservedRunIsNotAddressable`,
   `TestSandboxPruneHidesReservedRuns`.
9. **A sandbox never caches a tool call.** Every `Exec` must really run the
   command. A memoizing backend makes a repeated side effect invisible to the
   model. `TestEveryCallExecutes` enforces this for every adapter.
10. **A backend that cannot enforce a security control refuses it.** Returning
    success for a network policy nothing applies tells an operator they are
    protected when they are not. BONNIE broke this rule once, in
    `sandbox/microsandbox.go`, and §4.11 records it.

L2 — [`docs/L2.md`](L2.md) — drafts four more (11–14: generated-code
allowlist, disposable-generated versus sacred-authored files, refusal of
partial discovery, one place per setting). All four are enforced and tested
as of T-018 and T-024, and join the list here.

13. **Discovery refuses what it cannot fully honor.** A tool directory
    without `Tool()`, or two tools declaring one runtime name, is an error
    naming the directories, not a partial generation. A setting the chosen
    backend cannot apply is refused too: `sandbox.image` on the local
    backend, which runs no image; a network policy with no sandbox to
    enforce it; any agent option beside `WithAgentFactory`, which owns the
    agent outright. Guard tests: `TestCodegenRejectsDuplicateToolName`,
    `TestCodegenRejectsToolWithoutToolFunc`,
    `TestSandboxImageReachesEveryBackendThatRunsOne`,
    `TestLocalSandboxRefusesAnImage`, `TestDenyNetworkNeedsASandbox`,
    `TestAgentFactoryRefusesConflictingOptions`.
14. **There is one place to configure a setting.** A setting is a file at a
    fixed path or a Go option — never both, and never a third place that can
    disagree with the other two. The default paths are constants in the root
    `bonnie` package, read by the scaffold, codegen, the dev loop, and the
    runtime. This invariant replaced "the manifest is strict" when T-024
    removed the manifest: strictness was the mitigation for a second source
    of truth, and deleting the source beat policing it. Guard tests:
    `TestDefaultsAreTheScaffoldedLayout`, `TestScaffoldIsTheDefaultLayout`,
    `TestCodegenEmbedsTheDefaultLayout`.

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
