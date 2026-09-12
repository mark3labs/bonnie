# BONNIE MVP Tasks

Work items for `v0.1.0`. Read `docs/SPEC.md` first — it holds the verified
facts about Kit, the known risks, and the invariants every task must preserve.

## How to use this file

- Tasks are ordered by dependency, not by size. **Do T-001 first** even though
  T-002 is more severe; a surprise in T-001 can change T-002's design.
- Each task lists acceptance criteria that are testable. A task is done when
  every box is checked and `go test -race ./...` passes.
- Update the status table as you go. Leave a short note when you learn
  something that contradicts `docs/SPEC.md`, and correct the spec.

## Status

| ID | Title | Priority | Size | Status |
|---|---|---|---|---|
| T-001 | Live-model integration test | P0 | S | **done** |
| T-002 | Lossless replay: journal typed message parts | P0 | M | **done** |
| T-003 | Torn-write repair on Restore | P0 | M | **done** |
| T-004 | FileJournal (JSONL) | P0 | M | **done** |
| T-005 | Runner.Cancel | P0 | S | **done** |
| T-006 | channel/http adapter | P1 | L | **done** |
| T-007 | CLI: serve and runs | P1 | M | **done** |
| T-008 | Runnable examples | P1 | S | **done** |
| T-009 | Upstream Kit issues | P2 | S | drafted, not filed |
| T-010 | Repo hygiene files | P2 | S | **done** |
| T-011 | Release v0.1.0 | P2 | S | config ready, not tagged |

Sizes: S ≈ half a day · M ≈ 1–2 days · L ≈ 3–4 days.

---

## T-001 — Live-model integration test

**Priority** P0 · **Size** S · **Depends on** nothing · **Blocks** everything

### Why

Nothing in BONNIE has ever run against a real provider. `fakeAgent` proves the
executor's control flow, not that Kit behaves as `docs/SPEC.md` §3 claims. This
is where unknown-unknowns live. Doing it first means a surprise costs an
afternoon instead of a rewrite.

### Files

- `runtime/integration_test.go` (new, build tag `integration`)
- `.github/workflows/ci.yml` (leave hermetic — do **not** add a live job)
- `README.md` — document how to run it

### Do

1. Guard the file with `//go:build integration`.
2. Skip with `t.Skip` when the provider credential env var is absent, so
   `go test -tags integration ./...` is safe on a machine with no key.
3. Build a `Runner` with `runtime.KitAgent(kit.WithModel(...))` and a
   `MemoryJournal`.
4. Drive a prompt that forces `ask_human` — instruct the model plainly, for
   example "Before doing anything, use ask_human to ask which region to deploy
   to."
5. Assert `RunWaiting` and a non-nil `Suspend`.
6. Build a **second** `Runner` over the same journal. Resume it. Assert
   `RunCompleted`.
7. Assert the replayed conversation is intact and contains no orphaned
   `tool_use`. Replay is lossless as of T-002, so tool calls must be present
   and matched to their results.

### Acceptance criteria

- [x] `go test -race ./...` unchanged and green without the tag
- [x] `go test -race -tags integration ./runtime` passes with a real key
- [x] Test skips cleanly, not fails, when the key is absent
- [x] Resume happens through a second `Runner` instance
- [x] `README.md` documents the tag and the env var
- [x] Any divergence from `docs/SPEC.md` §3 is corrected in the spec

### Watch for

- `KitAgent` sets `Options.SessionManager` before `kit.New` (spec §3.1). If a
  stray session file appears under the Kit session directory, that regressed.
  The test asserts this with `kit.ListSessions("")`.
- Whether the model reliably calls the tool. If not, consider
  `PrepareStepResult.ToolChoice` via `LLMToolChoiceRequired` on step 0 — but
  note the doc warning in `pkg/kit/hooks.go` that forcing a choice on every
  step prevents the turn from ending. It was not needed: a plain system prompt
  worked on every run.
- **Give the live agent no tool that can touch the disk.** The first version of
  this test did, and the model wrote Terraform files into the checkout. See
  `docs/SPEC.md` §4.9. Use `noCoreTools()` and `isolatedWorkspace(t)`.

### eve analogue

[Human-in-the-Loop](https://eve.dev/docs/human-in-the-loop) — eve's park and
resume is the same shape, and its `ask_question` tool is the model for
`AskTool`.

---

## T-002 — Lossless replay: journal typed message parts ✅ DONE

**Priority** P0 · **Size** M · **Depends on** T-001 · **Blocks** T-003, T-004

> **Status: implemented.** `Record.Payload` carries the JSON-encoded
> `kit.LLMMessage`; `Restore` decodes from it with a text fallback for older
> records. `runtime/replay_fidelity_test.go` is active and passing. A latent
> ID-collision bug was fixed alongside (`s.ids.n = maxSeq`).
>
> Remaining acceptance criteria below are **unverified** — confirm reasoning,
> file, and tool-result round trips, and tighten
> `TestResumeAcrossProcessBoundary`, before closing.

### Why

**Most severe open issue. Verified by experiment — see `docs/SPEC.md` §4.1.**

`Restore` rebuilds every journalled message as a single `kit.LLMTextPart`,
because `Record` stores only flattened text. A probe that appended an assistant
message carrying one `LLMToolCallPart` recovered **zero** tool calls after
restore.

A resumed run therefore has no memory of what it called or what came back. The
model re-derives state it should have remembered, and may repeat a side effect
it already performed. This fires on **every** resume of a tool-using run, not
only after a crash — which makes it worse than T-003.

### Files

- `runtime/journal.go` — widen `Record`
- `runtime/session.go` — `AppendMessage` and `Restore`
- `runtime/util.go` — correct the stale comment on `messageText`
- `runtime/replay_fidelity_test.go` — **exists, skipped**; remove the `t.Skip`

### Do

1. Add a typed-parts field to `Record`. Prefer storing the marshalled
   `kit.LLMMessage` in `Payload` over inventing a parallel encoding — fantasy's
   `Message` has a custom `UnmarshalJSON`
   (`charm.land/fantasy/content_json.go`) that reconstructs the concrete part
   types, so a round trip through it is faithful and cheap to maintain.
2. Keep `Text` populated as a human-readable projection. `bonnie runs show`
   (T-007) and `jq` inspection depend on it. It becomes derived data, not the
   source of truth.
3. `Restore` rebuilds from the typed payload, falling back to the text
   projection when the payload is absent, so journals written before this task
   still load.
4. Verify round-trip fidelity for every part type BONNIE can encounter:
   text, reasoning, tool call, tool result, file.

### Acceptance criteria

- [x] `t.Skip` removed from `runtime/replay_fidelity_test.go` and it passes
- [x] Round trip preserves `LLMToolCallPart` including `ToolCallID`,
      `ToolName`, and `Input`
- [x] Round trip preserves `LLMToolResultPart` and matches it to its call
- [x] Round trip preserves reasoning and file parts
- [x] A record written before this change still restores (text fallback)
- [x] `messageText`'s comment in `runtime/util.go` no longer claims the typed
      message survives
- [x] `TestResumeAcrossProcessBoundary` tightened to assert tool-call fidelity

### Watch for

- `kit.LLMMessage` is an alias for `fantasy.Message`. Marshal and unmarshal
  through that type; do not hand-roll part decoding.
- Part field names are not what you might guess: it is
  `LLMToolCallPart{ToolCallID, ToolName, Input}`, not `{ID, Name}`.

### eve analogue

[Execution Model and Durability](https://eve.dev/docs/concepts/execution-model-and-durability)
— eve checkpoints the whole step payload, so replay fidelity is not a question
it has to answer. BONNIE must answer it explicitly.

---

## T-003 — Torn-write repair on Restore

**Priority** P0 · **Size** M · **Depends on** T-002 · **Blocks** T-004

### Why

**This is a severe latent defect.** See `docs/SPEC.md` §4.2 for the full
analysis. In short: Kit calls `AppendMessage` once per message
(`pkg/kit/kit.go:2990-2994`), so a tool-calling step becomes two journal
records. A crash between them leaves an assistant message whose `tool_use` has
no `tool_result`. Providers reject that conversation, and the run becomes
permanently unresumable.

Kit's in-process pairing guarantee cannot survive a crash. BONNIE must repair.

### Files

- `runtime/repair.go` (new)
- `runtime/repair_test.go` (new)
- `runtime/session.go` — call the repair from `Restore`

### Do

1. Write `func repairTrailingOrphan(msgs []kit.LLMMessage) []kit.LLMMessage`.
2. Inspect the **trailing** assistant message. Collect the IDs of its
   `kit.LLMToolCallPart` entries. Collect the tool-result IDs from any
   following tool-role messages.
3. If a tool call has no matching result, drop the incomplete trailing step.
   Dropping is correct: the step never finished, so re-running it is the
   intended semantics.
4. Only repair the **tail**. A mismatch in the middle of a conversation means
   the journal is corrupt, not torn — return an error rather than silently
   rewriting history.
5. Call it from `Restore` before the session is returned.
6. Record a `RecordKind` for the repair so the event is auditable.

### Acceptance criteria

- [x] Trailing assistant with unmatched `tool_use` is dropped on `Restore`
- [x] A well-formed conversation is returned unchanged (byte-identical)
- [x] A mid-conversation mismatch returns an error, not a silent rewrite
- [x] Multi-call steps: partial results drop the whole step, not just the
      unmatched call
- [x] A repair is recorded in the journal
- [x] Test simulates a torn write by appending only the assistant record
- [x] `Restore` after repair yields a conversation a provider accepts
      (assert structurally; the live check belongs in T-001)

### Watch for

- Kit's tool-call part types are `kit.LLMToolCallPart` and
  `kit.LLMToolResultPart` (`pkg/kit/types.go:196-199`). Match on the call ID.
- This task assumes T-002 landed. Before that, replay has no tool calls to
  orphan, so the repair is untestable — which is exactly why T-002 comes first.

### eve analogue

eve gets this free from the Workflow SDK, which checkpoints a whole step
atomically — see
[Execution Model and Durability](https://eve.dev/docs/concepts/execution-model-and-durability).
BONNIE has no such engine, so it repairs instead. Worth reading to understand
what "checkpoint at steps" buys.

---

## T-004 — FileJournal (JSONL)

**Priority** P0 · **Size** M · **Depends on** T-003 · **Blocks** T-006

### Why

`MemoryJournal` makes the durability claim false in production. Everything in
the release rests on a journal that outlives the process. JSONL matches Kit's
own session format, so records stay inspectable with `jq`, and appends are
cheap.

### Files

- `runtime/filejournal.go` (new)
- `runtime/filejournal_test.go` (new)
- `runtime/journal.go` — add `Persisted() bool` to the interface
- `runtime/session.go` — `IsPersisted` delegates instead of type-asserting

### Do

1. Layout: `<root>/runs/<run-id>.jsonl`, one JSON object per line. Default root
   `.bonnie` (already gitignored).
2. `Append` opens with `O_APPEND|O_CREATE|O_WRONLY`, writes one line, and
   fsyncs. Make the fsync policy configurable — `always` for correctness,
   `interval` for throughput — and default to `always` for the MVP.
3. Keep run state in a sidecar or derive it from the last `RecordState` record.
   Prefer deriving: fewer files, no divergence.
4. `Replay` streams with `bufio.Scanner` and a raised buffer limit; tool
   arguments can exceed the 64 KB default.
5. Tolerate a truncated final line — that is a torn write, and it must not fail
   the whole replay. Drop it and let T-003's repair handle the semantics.
6. Serialise writes per run with a mutex. Document that one process owns a run
   at a time; cross-process locking is out of scope for the MVP.
7. Confirm `Record`'s typed payload from T-002 survives the JSONL round trip.

### Acceptance criteria

- [x] `FileJournal` satisfies `runtime.Journal`
- [x] Round-trip: `Append` → new `FileJournal` on the same dir → `Replay`
      returns identical records
- [x] A truncated final line is dropped without failing `Replay`
- [x] A 200 KB tool argument survives the round trip
- [x] Concurrent `Append` on different runs is race-clean under `-race`
- [x] `Persisted()` reports true; `MemoryJournal` reports false
- [x] `Session.IsPersisted` no longer type-asserts
- [x] Crash test: write records, reopen, `Restore`, assert conversation intact
- [x] All `MemoryJournal` tests pass against `FileJournal` via a shared suite

### Watch for

- Write a table-driven conformance suite both journals run. It is the cheapest
  way to keep a third implementation honest later.
- `Checkpoint` should be one atomic append (spec §4.6).

### eve analogue

[Execution Model and Durability](https://eve.dev/docs/concepts/execution-model-and-durability)
— eve's local dev world persists under `.eve/.workflow-data`. Same idea,
different encoding.

---

## T-005 — Runner.Cancel

**Priority** P0 · **Size** S · **Depends on** T-004

### Why

There is no way to stop a running turn. `channel.PolicySteer` is declared in
`channel/channel.go` but cannot be implemented without it, so T-006 is blocked
on this.

Persistence is already safe: `internal/agent/agent.go:946-958` persists a
step's messages **before** checking `ctx.Err()`, so a cancelled turn keeps its
completed work (spec §3.3). This is a missing feature, not a correctness bug.

### Files

- `runtime/runner.go`
- `runtime/runner_test.go`

### Do

1. Store a `context.CancelFunc` per active run alongside the existing `active`
   map, under the same mutex.
2. Add `func (r *Runner) Cancel(runID string) error`. Return a sentinel when
   the run is not active.
3. Derive the turn context from the caller's with `context.WithCancel`.
4. On cancellation, checkpoint a terminal state. Decide and **document**
   whether that is `RunFailed` or a new `RunCancelled`. Recommendation: add
   `RunCancelled`, because a cancelled run is resumable in principle and a
   failed one is not.
5. Verify the repair from T-003 runs on the next `Restore` of a cancelled run.

### Acceptance criteria

- [x] `Cancel` stops an in-flight turn
- [x] Cancelling an unknown or idle run returns a sentinel error, tested with
      `errors.Is`
- [x] Completed steps survive cancellation in the journal
- [x] A cancelled run restores to a provider-valid conversation
- [x] State transition is documented in `docs/SPEC.md`
- [x] `-race` clean with concurrent `Start` and `Cancel`

### Watch for

- `fakeAgent` ignores its context. Give it a blocking mode so cancellation is
  actually observable in tests.

---

## T-006 — channel/http adapter

**Priority** P1 · **Size** L · **Depends on** T-005

### Why

Makes BONNIE reachable from outside the process, which is the third of the
three claims in `docs/SPEC.md` §5. It is also the first real test of whether
`pkg/kit` exposes enough for concurrent multi-session work — a gap that is
still unverified.

### Files

- `channel/http/http.go` (new)
- `channel/http/http_test.go` (new)
- `channel/channel.go` — adjust the interfaces if the concrete adapter proves
  them wrong. They are drafts; the adapter is the authority.

### Do

1. Routes:
   - `POST /runs` → start a run, return `{run_id, state, ...}`
   - `POST /runs/{id}` → send a message to an existing run
   - `POST /runs/{id}/respond` → answer a suspension
   - `POST /runs/{id}/cancel` → cancel
   - `GET  /runs/{id}/stream` → NDJSON event stream
2. Implement `Inbound`: `From(address)` resolves an address to whichever run
   owns it now, creating one if new; `Attach(runID)` targets exactly that run
   and never creates. Keep the distinction sharp — it is the part most often
   got wrong.
3. Address→run mapping needs to be durable. Store it in the journal rather than
   a process-local map, or a restart loses every Slack thread.
4. NDJSON stream: subscribe via `kit.Kit.Subscribe`, serialise each event as
   one JSON line, flush per line. Support a cursor query parameter so a
   dropped client can reconnect without losing events.
5. Implement `PolicySteer` with `InjectSteer` plus `Cancel`; implement
   `PolicyQueue` with a per-run mailbox.
6. Respect `SendOptions.Auth`. Carrying the `Principal` is enough for the MVP;
   verification is deferred.

### Acceptance criteria

- [x] All five routes work end to end against `httptest.Server`
- [x] `From` on a new address creates a run; on a known address resolves the
      existing one
- [x] `Attach` on an unknown run returns 404 and never creates
- [x] Address mapping survives a journal reopen
- [x] NDJSON stream emits one JSON object per line and flushes incrementally
- [x] Reconnect with a cursor resumes without gaps or duplicates
- [x] A suspended run is answered through `/respond` and completes
- [x] Concurrent runs on one server are `-race` clean
- [x] `PolicySteer` interrupts an active turn; `PolicyQueue` serialises

### Watch for

- **The real risk:** one `*kit.Kit` per run, or one shared? `KitAgent` builds a
  fresh instance per turn today, which is simple but may be costly under load.
  Kit's README says each instance owns an isolated viper store, so concurrent
  instances are safe. Measure before optimising, and record the finding.
- Do not let `runtime/` import `channel/` (invariant 7).

### eve analogue

- [Custom Channels](https://eve.dev/docs/channels/custom) — `defineChannel`,
  routes, `from` vs `attachSession`, `turnPolicy`, continuation tokens. Read
  this before designing the address mapping; eve's re-keying semantics are
  worth copying.
- [Sessions, Runs & Streaming](https://eve.dev/docs/concepts/sessions-runs-and-streaming)
  — the NDJSON contract and reconnect cursor.
- [eve HTTP channel](https://eve.dev/docs/channels/eve) — route shapes for
  cancel, compact, clear, reset.

---

## T-007 — CLI: serve and runs

**Priority** P1 · **Size** M · **Depends on** T-006

### Why

`cmd/bonnie/main.go` is a skeleton that only prints a version. `runs` is the
debugging story for a durable system and costs almost nothing once the journal
exists.

### Files

- `cmd/bonnie/main.go`
- `cmd/bonnie/serve.go`, `cmd/bonnie/runs.go` (new)

### Do

1. `bonnie serve [--addr :8080] [--journal .bonnie]` — mount `channel/http`.
2. `bonnie runs list [--state waiting]` — read the journal directly, no server.
3. `bonnie runs show <id>` — print the record timeline; `--json` for machines.
4. Graceful shutdown on SIGINT: stop accepting, let in-flight turns checkpoint.
5. Keep using stdlib `flag`. Cobra is a dependency the MVP does not need, and
   BONNIE is headless by invariant 2.

### Acceptance criteria

- [x] `bonnie serve` starts and answers the T-006 routes
- [x] `bonnie runs list` shows runs, filterable by state
- [x] `bonnie runs show` prints a readable timeline and valid `--json`
- [x] SIGINT shuts down without losing a checkpointed step
- [x] `bonnie help` documents every implemented command
- [x] No new third-party dependency

---

## T-008 — Runnable examples

**Priority** P1 · **Size** S · **Depends on** T-006

### Why

The `README.md` example has never been executed — it is aspirational. Examples
that compile in CI cannot rot.

### Files

- `examples/minimal/main.go` (new)
- `examples/hitl-restart/main.go` (new)
- `examples/README.md` (new)
- `README.md` — align the snippet with the real example

### Do

1. `minimal` — start a run, print the response, using `FileJournal`.
2. `hitl-restart` — two phases in one binary behind a flag: phase one suspends
   and exits the process; phase two is a fresh run that resumes from disk.
   This is the headline demo, and it must genuinely exit between phases.
3. `go vet ./...` must cover `examples/`, so they stay compiling.

### Acceptance criteria

- [x] Both build under `go build ./...`
- [x] `hitl-restart` resumes after a real process exit
- [x] `examples/README.md` gives copy-pasteable commands and the env vars
- [x] The `README.md` snippet matches `examples/minimal`

---

## T-009 — Upstream Kit issues

**Priority** P2 · **Size** S · **Depends on** T-001, T-003

### Why

`v0.1.0` should document its own assumptions about Kit. File after T-003,
because the batch-append ask needs the torn-write evidence to be persuasive.

### Do

File on `mark3labs/kit`, each citing file:line from `docs/SPEC.md` §3:

1. **Batch append on `SessionManager`** — cite §4.2 and the T-003 repair as
   proof that no external session manager can be crash-safe without it.
2. **`PrepareStepResult.Tools []Tool`** — the doc comment already promises it.
3. **Stability promise on `ToolOutput.Halt` + `FinalValue`**.
4. **Stability policy for `kit.SessionManager`**.

### Acceptance criteria

- [ ] Four issues filed with reproductions or citations
- [ ] `docs/SPEC.md` §6 updated with the issue links

> **Status: drafted, not filed.** The four issues are written in full, with
> verified file:line citations and the evidence from T-003, in
> [`docs/UPSTREAM.md`](UPSTREAM.md). Filing needs a GitHub account with access
> to `mark3labs/kit`, so a human must do the last step. Paste the issue links
> into `docs/UPSTREAM.md` and `docs/SPEC.md` §6 when you file them.

---

## T-010 — Repo hygiene files

**Priority** P2 · **Size** S · **Depends on** nothing

### Files

`CONTRIBUTING.md`, `SECURITY.md`, `.github/ISSUE_TEMPLATE/`,
`.github/pull_request_template.md`, `CHANGELOG.md`

### Do

- `CONTRIBUTING.md` — the §2 boundary rule stated first and plainly, the
  `go.work`-in-parent-directory setup, and the `GOWORK=off` CI rule.
- `SECURITY.md` — disclosure contact. Note that BONNIE executes model-chosen
  tool calls and ships no sandbox in `v0.1.0`; that is a deployment
  responsibility and must be stated.
- `CHANGELOG.md` — Keep a Changelog format.

### Acceptance criteria

- [x] All files present
- [x] `CONTRIBUTING.md` explains the public-API-only rule and its enforcement
- [x] `SECURITY.md` states the no-sandbox caveat

---

## T-011 — Release v0.1.0

**Priority** P2 · **Size** S · **Depends on** all

### Do

1. `.goreleaser.yaml` building `./cmd/bonnie` for linux/darwin × amd64/arm64,
   with `-X main.version={{.Version}}`.
2. `.github/workflows/release.yml` on tag push.
3. Verify CI is green with `GOWORK=off` against the published Kit version.
4. Confirm `go.work` is absent from the repo (it belongs in the parent dir).
5. Write release notes: state the three claims, and state the limits plainly —
   no sandbox, no auth verification, single-process run ownership.
6. Tag `v0.1.0`.

### Acceptance criteria

- [x] `.goreleaser.yaml` and `.github/workflows/release.yml` exist and parse
- [ ] `goreleaser build --snapshot --clean` produces working binaries
- [x] `bonnie version` prints the injected version
- [ ] All CI jobs green, including `boundary`
- [x] `go.work` not committed
- [x] Release notes state both the claims and the limits
- [ ] Tag pushed and artifacts published

> **Status: configuration ready, not released.** `goreleaser` is not installed
> on the development machine, so the snapshot build is unverified. CI runs only
> after a push. Tagging is a human decision, so it is left undone on purpose.
> The limits are stated in `README.md` under "Limits of v0.1.0" and in
> `SECURITY.md`.

---

## Definition of done for v0.1.0

The release ships when a user can:

1. Start a durable run from Go or over HTTP.
2. Have it park on `ask_human`, **kill the process**, restart, and resume to
   completion.
3. Inspect what happened with `bonnie runs show`.

If any of those three fails, do not tag.
