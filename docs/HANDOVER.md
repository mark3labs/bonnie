# Handover

**For:** the next agent or developer to work on BONNIE
**State:** `v0.1.0` tagged and released, 2026-09-12
**Read first:** this file, then `docs/SPEC.md`, then `AGENTS.md`

---

## 1. What BONNIE is, in one paragraph

Kit (`github.com/mark3labs/kit`) is an agent kernel. Its unit of work is one
in-process turn that lives and dies with the process. BONNIE is the layer above
it: it turns that turn into a **durable run** that survives process death, can
park indefinitely awaiting human input without holding compute, and is
reachable over HTTP. It adds a sandbox so the tool calls a model chooses do not
run as the host process.

## 2. Where things stand

```
master @ b3ec7b2 (+ this session's work)  →  github.com/mark3labs/bonnie
7 packages · 40 Go files · 133 test functions · all green
```

| Package | State |
|---|---|
| `runtime/` | Complete. Journal, session, runner, torn-write repair, events. Verified against a live model. |
| `channel/` | Interfaces only, no tests (T-015). |
| `channel/http/` | Complete. Six routes, NDJSON stream, journalled address map. |
| `sandbox/` | `local` and `docker` verified. **`microsandbox` has never run** (T-013). |
| `cmd/bonnie/` | `serve`, `runs list`, `runs show`. |
| `examples/` | `minimal`, `hitl-restart`. |

**Not done, one human decision:** tag `v0.1.0` (T-011). T-009 resolved
itself — Kit `v0.106.0` answered all four upstream asks, and BONNIE adopted
the seams the same day.

## 3. Start here

```bash
cd ~/Workspace/bonnie

go build ./...
go test -race ./...          # hermetic, no credentials needed
golangci-lint run            # must be 0 issues

# Live model. Skips cleanly with no key.
export ANTHROPIC_API_KEY=sk-ant-...
go test -race -tags integration ./runtime ./sandbox
```

`docs/TASKS.md` has the open work at the top, shipped work archived at the
bottom. Highest value first: **T-013** (verify microsandbox — needs hardware)
and **T-012** (journal the sandbox lifecycle).

## 4. The one rule that matters

**BONNIE uses the public Kit SDK only: `github.com/mark3labs/kit/pkg/kit`.**

Never import `kit/internal/...`. Never import `charm.land/fantasy` either —
Kit re-exports every model type as a `kit.LLM*` alias, and naming fantasy
directly pins BONNIE to Kit's transitive dependency. It must stay `// indirect`
in `go.mod`.

Enforced three ways: the Go module boundary, a `depguard` rule, and the
`boundary` CI job. All three fire.

I broke the fantasy half of this rule during development and it was caught in
review. If you need something Kit does not export, add it to
`docs/UPSTREAM.md` and raise it upstream — do not reach around.

## 5. Things that will bite you

These are the non-obvious decisions. Each exists because something went wrong.

### The journal is the source of truth, and `Record.Text` is not

`Record.Payload` carries the JSON-encoded `kit.LLMMessage` with its typed
parts. `Record.Text` is a **display projection**. An earlier version stored
only text, so a resumed run had no memory of which tools it had called and
would repeat a side effect it had already performed. If you add a record kind,
put the lossless form in `Payload`.

### A tool-calling step used to be two journal records

Kit called `AppendMessage` once per message, so an assistant message carrying
a `tool_use` and the tool message carrying its `tool_result` were separate
writes. Crash between them and you got an orphaned tool call, which **every
provider rejects** — the run became permanently unresumable.

**Kit `v0.106.0` closed the main window.** It added `kit.StepAppender`; BONNIE
implements it (`Session.AppendStep`) and mirrors the pattern on its own
journal seam (`runtime.StepJournal`). A step now reaches a `FileJournal` as
one buffered write and one fsync, and a cancelled context cannot drop a
completed step, because the write runs under `context.WithoutCancel` per
Kit's documented contract.

`runtime/repair.go` stays anyway, because three things still produce the torn
shape: journals written before the upgrade, journals whose implementation
does not provide `StepJournal` (the per-record fallback), and a short write of
the batch buffer. It drops that incomplete trailing step on restore and
journals the repair. A mismatch anywhere but the tail is real corruption and
returns `ErrCorruptConversation` rather than silently rewriting history.

### A sandbox must never cache a tool call

Measured against Dagger during evaluation:

```
call 1: ledger="CHARGE CUSTOMER 1789226793"
call 2: ledger="CHARGE CUSTOMER 1789226793"   ← ran once, agent told twice
```

The agent believes it charged the customer twice. It charged once. No error, no
warning. `TestEveryCallExecutes` enforces this for every backend. If you add an
adapter over a memoizing runtime, defeat its cache **by default**, not as an
option.

### A backend that cannot enforce a security control must refuse it

BONNIE broke this rule in its own code. `sandbox/microsandbox.go` accepted
`deny-all`, stored it in a field, and never read that field again — so a caller
asking for no egress got `nil` and full network access. It now refuses. This
was found while writing the handover, which is an argument for writing
handovers.

### Exit codes from CLI backends are ambiguous

`docker exec` and `msb exec` both exit with the **guest command's** code. So
exit 42 means either "your command failed" or "the daemon is down". Report the
second as the first and the model goes off fixing code that was never broken.

BONNIE has the guest print its own code on a nonce-marked line, with the argv
passed *after* the script so the shell handles quoting. No marker means the CLI
failed before the guest ran, which is a Go error, not a `Result`.

### A non-zero exit is a result, not an error

`Exec` returns `*Result` with an `ExitCode`. Only a failure to run the command
at all is a Go error. "The build failed" is information the model must act on.

### Live tests must give the model no tool that touches the disk

The first version of the live test handed a real model Kit's core tools in the
repository working directory and said "deploy the app". It wrote a
`Dockerfile`, a `terraform/` directory, `deploy.sh`, and three deployment
documents into the checkout. Nothing failed. Nothing warned. The test passed.

Live tests now use `noCoreTools()` and `isolatedWorkspace(t)`, which fails if
anything appears in the temp directory. `docs/SPEC.md` §4.9 has the full
story.

## 6. Architecture in 60 seconds

```
L4  cmd/bonnie        serve, runs
L3  channel/          transport interfaces → channel/http
L1  runtime/          durable executor          sandbox/  isolated tools
L0  kit/pkg/kit       upstream, unmodified
```

Durability comes from four **public** Kit extension points:

| Need | Kit API | BONNIE file |
|---|---|---|
| Journal every message | `Options.SessionManager` | `runtime/session.go` |
| Commit a step atomically | `kit.StepAppender` (v0.106.0) | `runtime/session.go`, `journal.go`, `filejournal.go` |
| Checkpoint each step | `Kit.OnStepFinish` | `runtime/runner.go` |
| Inject replayed context | `Kit.OnContextPrepare` | `runtime/runner.go` |
| Park for a human | `ToolOutput{Halt, FinalValue}` | `runtime/suspend.go` |

Two seams are worth understanding before changing anything:

- **`Agent` is an interface**, not `*kit.Kit`. That is what makes L1 testable
  without credentials. Optional capability (event streaming) goes through a
  narrow type assertion — `eventSource` in `runtime/events.go` — never by
  widening `Agent`.
- **`AgentFactory` is the extension point.** `sandbox.Agent` is just another
  factory. That is why sandboxing needed no change to `runtime/`.

`var _ kit.SessionManager = (*Session)(nil)` in `session.go` is a deliberate
tripwire: if Kit widens the interface, the build breaks there first.

## 7. Conformance suites

Three exist. They are the cheapest way to keep a future implementation honest,
and a new implementation should join one rather than write its own tests:

| Suite | File | Covers |
|---|---|---|
| Journal | `runtime/journal_conformance_test.go` | memory + file |
| Sandbox | `sandbox/conformance_test.go` | local + docker + microsandbox, 18 cases |
| Channel | *does not exist* | T-015 |

A backend that cannot run on the test machine must **skip**, not fail.

## 8. What I would do next, in order

1. **T-013 — verify microsandbox.** It is written, compiles, and has never
   executed. Needs macOS on Apple Silicon or Linux with KVM. Until this is
   done, do not let the docs imply that path is proven.
2. **T-012 — journal the sandbox lifecycle.** Today a pruned container gives a
   resumed run an empty workspace with no explanation, and finished runs leak
   containers.
3. **T-011 — tag the release.** T-009 resolved itself: Kit `v0.106.0`
   answered the asks, BONNIE adopted `kit.StepAppender`, and the torn-write
   window is now one write, not two.
4. **T-015 — `channel` tests**, before a second adapter makes the interface
   hard to change.

## 9. Verification before you commit

```bash
gofmt -l $(git ls-files '*.go')              # must be empty; never `gofmt -l .`,
                                             # it walks .direnv/ and reports vendored files
go build ./...
go vet ./... && go vet -tags integration ./...
go test -race ./...
golangci-lint run                            # 0 issues
GOWORK=off go build ./...                    # the CI build
```

`GOWORK=off` matters. Locally a `go.work` in the **parent** directory points
BONNIE at a Kit checkout; CI builds against the version pinned in `go.mod`.
Never put a `replace` directive in the published `go.mod`.

A durability claim needs a test that crosses a process boundary in spirit:
build a second `Runner` sharing only the journal. `TestResumeAcrossProcessBoundary`
is the pattern.

## 10. Documents

| File | What it is for |
|---|---|
| `docs/TASKS.md` | Open work first, shipped work archived. Start here. |
| `docs/SPEC.md` | Verified Kit facts with file:line, every risk, the invariants. |
| `docs/SANDBOX.md` | Backends, adapter contracts, why the CLI and not the SDKs. |
| `docs/UPSTREAM.md` | Four Kit issues, written and ready to file. |
| `AGENTS.md` | Conventions. Short. |
| `README.md` | User-facing. Every snippet was compiled and the quickstart run. |

`docs/SPEC.md` §3 is the one to re-read after a Kit upgrade: it records what
was verified about Kit and where, so a changed assumption is findable.

## 11. Known limits, stated plainly

- Sandboxing is opt-in. Without it, tool calls run as the host process.
- Docker is namespaces, not a guest kernel.
- **microsandbox is verified on Linux/KVM only, and its network policy is
  fixed at create time; a reattach under a different policy fails loudly.**
  The live suites run against `opencode/kimi-k2.5` by default, after the
  Anthropic workspace quota blocked every agent-sized request for a day.
- The HTTP channel carries a `Principal` and does not verify it.
- Run ownership is enforced per host with a `flock` per run; a shared
  network filesystem or a second writer still needs one owner in front.
- Events are anchored to the journal: a reconnect whose cursor has fallen
  off the in-memory backlog is served by replaying the records, and the
  stream survives a process restart. Live-only deltas (Kit's mid-turn
  progress) are the exception, and they are marked as ephemeral.
- Sandbox lifecycle is journalled; reclaiming is a `bonnie sandbox prune`
  command, not a background sweep.

All of these are in `README.md` and `SECURITY.md` too. Keep them there. A
framework that hides its limits gets deployed into situations it cannot
handle.
