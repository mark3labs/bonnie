# BONNIE Tasks

Work items. Read `docs/SPEC.md` first — it holds the verified facts about Kit,
the known risks, and the invariants every task must preserve.

## How to use this file

- **Open work is at the top.** Shipped work is archived at the bottom, with
  one line each and a pointer to the code.
- Each task lists acceptance criteria that are testable. A task is done when
  every box is checked and `go test -race ./...` passes.
- Update the status table as you go. Leave a short note when you learn
  something that contradicts `docs/SPEC.md`, and correct the spec.
- New work gets the next free ID. Do not renumber.

## Status

### Open

| ID | Title | Priority | Size | Blocks |
|---|---|---|---|---|
| T-011 | Tag and release `v0.1.0` | P2 | S | — |
| T-012 | Journal the sandbox lifecycle | P1 | M | — |
| T-013 | Verify the microsandbox adapter on real hardware | P1 | S | — |
| T-014 | Cross-process run ownership | P2 | M | — |
| T-015 | `channel` package has no tests | P2 | S | — |

### Resolved by upstream

**T-009 — file the four upstream Kit issues — is moot.** Kit `v0.106.0`
(PR `mark3labs/kit#135`) answered all four asks before anything was filed.
BONNIE adopted the batch-append seam the same day: `Session.AppendStep`
plus the `runtime.StepJournal` optional interface. `docs/UPSTREAM.md` records
what each ask became, and `docs/SPEC.md` §3 was re-verified against
`v0.106.0`. T-011 no longer depends on it.

### Shipped in `v0.1.0`

T-001 … T-008 and T-010, plus the `sandbox` package. See
[Archive](#archive-shipped) for what each one delivered.

Sizes: S ≈ half a day · M ≈ 1–2 days · L ≈ 3–4 days.

## Where the code is

| Area | Package | Notes |
|---|---|---|
| Durable executor | `runtime/` | journal, session, runner, repair, events |
| Transports | `channel/`, `channel/http/` | interfaces, then the HTTP adapter |
| Isolated tools | `sandbox/` | `local`, `docker`, `microsandbox` |
| CLI | `cmd/bonnie/` | `serve`, `runs list`, `runs show` |
| Examples | `examples/` | `minimal`, `hitl-restart` |

---

## T-009 — File the four upstream Kit issues

**RESOLVED WITHOUT FILING.** Kit `v0.106.0` (PR `mark3labs/kit#135`,
"durability seams for external SessionManager implementations") answered all
four asks before anything was filed. See [Resolved by upstream](#resolved-by-upstream)
at the top of this file and [`docs/UPSTREAM.md`](UPSTREAM.md) for what each
ask became. The section below is the task as it was written, kept for the
record.

**Priority** P2 · **Size** S · **Blocks** T-011

### Why

`v0.1.0` should document its own assumptions about Kit. The batch-append ask
is the strongest of the four, and it now has evidence behind it: BONNIE's
torn-write repair (`runtime/repair.go`) exists only because no external
`SessionManager` can write a tool-calling step atomically.

### Do

The four issues are **already written in full**, with verified file:line
citations, in [`docs/UPSTREAM.md`](UPSTREAM.md). Paste each into an issue on
`mark3labs/kit`:

1. **Batch append on `SessionManager`** — highest value.
2. **`PrepareStepResult.Tools []Tool`** — the doc comment already promises it.
3. **Stability promise on `ToolOutput.Halt` + `FinalValue`**.
4. **Stability policy for `kit.SessionManager`**.

Then put the issue links next to each title in `docs/UPSTREAM.md` and in
`docs/SPEC.md` §6.

### Acceptance criteria

- [x] Four issues filed with reproductions or citations — **moot**: the
      release answered them; nothing to file
- [x] `docs/UPSTREAM.md` carries the outcome of each ask, with PR and
      file:line references into `v0.106.0`
- [x] `docs/SPEC.md` §6 carries the outcome, and §3 was re-verified against
      `v0.106.0`
- [x] BONNIE adopts the batch-append seam (`Session.AppendStep`,
      `runtime.StepJournal`, tests in `runtime/step_append_test.go`)

### Watch for

Nothing. The release landed the seams before the filing, and BONNIE adopted
them. If a future Kit release changes the shapes `docs/SPEC.md` §3 cites,
re-verify that section and update it in the same commit.

---

## T-011 — Tag and release `v0.1.0`

**Priority** P2 · **Size** S

T-009 is resolved by upstream, so nothing blocks this task but the decision
to ship.

### Why

The configuration is ready and unverified. Tagging is a human decision, so it
was deliberately left undone.

### Do

1. Run `goreleaser check` and `goreleaser build --snapshot --clean`.
   `goreleaser` was not installed on the development machine, so
   `.goreleaser.yaml` has never been executed.
2. Confirm every CI job is green on `master`, including `boundary`.
3. Write release notes. State the three claims — survives process death,
   parks indefinitely, reachable over HTTP — and state the limits from
   `README.md` just as plainly.
4. Tag `v0.1.0` and push the tag. `release.yml` fires on `v*`.
5. Check the published artifacts run: `bonnie version` must print the
   injected version, not `dev`.

### Acceptance criteria

- [x] `.goreleaser.yaml` and `.github/workflows/release.yml` exist and parse
- [x] `go.work` is not committed
- [ ] `goreleaser build --snapshot --clean` produces working binaries
- [ ] All CI jobs green on `master`, including `boundary`
      (green; the old golangci-lint pin was the only failure)
- [ ] Release notes state both the claims and the limits
- [ ] Tag pushed and artifacts published
- [ ] A downloaded binary prints the injected version

### Watch for

- **CI was broken from the first commit and nobody had looked.** Every run on
  `master` failed. The `lint` job pinned golangci-lint `v2.1.0`, which is
  built with go1.24, against a `go.mod` that asks for 1.27; golangci-lint
  refuses to load its config when its own toolchain is older than the target
  and exits 3. `test` and `boundary` were green throughout, so only `lint` was
  ever failing. Fixed by pinning `v2.13.2` (built with go1.27.0). Raise that
  pin whenever the `go` line in `go.mod` moves.
- The sandbox conformance suite **skips** Docker and microsandbox on a bare
  runner. CI green does not mean those adapters were exercised. The
  microsandbox defects fixed in T-013 were invisible to CI for exactly this
  reason.

---

## T-012 — Journal the sandbox lifecycle

**Priority** P1 · **Size** M · **Spec** §4.10

### Why

A run's conversation is durable; its sandbox workspace is recorded nowhere.
Today that produces three distinct failures:

- `bonnie runs show` cannot say which backend a run used, or whether its
  workspace still exists.
- A run whose container was pruned resumes with an **empty workspace and no
  explanation**. The model watches its files vanish between turns and has no
  way to know why.
- Nothing deletes the container of a finished run, so a long-lived server
  accumulates them until the disk fills.

### Files

- `runtime/journal.go` — add `RecordSandbox`
- `sandbox/agent.go` — write the record when a sandbox opens
- `cmd/bonnie/runs.go` — show the backend and sandbox ID in the timeline
- A reconciler, location to be decided

### Do

1. Add a `RecordSandbox` kind carrying the backend name and the sandbox ID.
2. Write it when `LazyOpener` opens a sandbox for the first time.
3. On resume, compare the recorded sandbox against what the backend reports.
   When the workspace is gone, say so in the run rather than silently handing
   the model an empty directory. Decide and **document** whether that is a
   failure or a fresh workspace with a note in the conversation.
4. Add a reconciler that deletes the sandboxes of terminal runs. Decide where
   it lives: a `bonnie sandbox prune` command is the cheap answer, a
   background sweep in `serve` is the complete one.

### Acceptance criteria

- [ ] `RecordSandbox` is written when a sandbox opens
- [ ] `bonnie runs show` displays the backend and sandbox ID
- [ ] A resumed run whose workspace is gone reports that, and does not
      silently present an empty one
- [ ] Terminal runs can have their sandboxes reclaimed
- [ ] A test covers the missing-workspace path

### Watch for

Do not let `runtime/` import `sandbox/` (invariant 7 is about `channel/`, but
the same layering logic applies). The record kind belongs in `runtime`; the
code that writes it belongs in `sandbox`.

---

## T-013 — Verify the microsandbox adapter on real hardware

**Priority** P1 · **Size** S

### Why

`sandbox/microsandbox.go` was **written but never executed**. `msb` was not
installed on the development machine, so every microsandbox conformance
subtest skipped. It is the strongest isolation BONNIE offers and the only
backend that can enforce a domain allow-list, so shipping it unverified is a
promise BONNIE has not tested.

**Update 2026-09-12.** The Nix flake put `msb` on the PATH, the cases stopped
skipping, and three of the 18 failed immediately. All three are fixed and all
18 now pass on Linux with KVM (`msb` 0.6.18). See `docs/SPEC.md` §4.11 for the
defects. What remains is the network policy, the live suite, and macOS.

### Do

1. Install `msb` on a machine that supports it: macOS on Apple Silicon, or
   Linux with KVM.
2. Run `go test -race ./sandbox`. All 18 conformance cases must pass for the
   `microsandbox` backend, not skip.
3. Run `go test -race -tags integration ./sandbox` with a provider key.
4. Check the two decisions made without being able to test them:
   - File I/O uses `msb cp` rather than exec-with-stdin, because `cp` is
     documented and stdin byte handling is not. Confirm `cp` handles binary
     content and a missing path the way the adapter assumes.
   - `exists()` matches a name in `msb ps --format json`. Confirm the field
     shape, and that a name match cannot produce a false positive.
5. Implement `SetNetworkPolicy` for real. **Done.** The provider maps every
   mode onto `msb create` flags (`--no-net`, `--net-rule allow@<host>`) and
   verifies on reattach that an existing sandbox still carries the configured
   policy, returning `ErrPolicyMismatch` otherwise, because `msb modify`
   cannot change network rules. Enforcement is proven with real egress in
   `TestMicrosandboxEnforcesNetworkPolicy`.

### Acceptance criteria

- [x] All conformance cases pass for `microsandbox`, none skipped
      (18/18, Linux + KVM, `msb` 0.6.18, 2026-09-12)
- [ ] The live sandbox suite passes against microsandbox
      (selectable with `BONNIE_TEST_SANDBOX=microsandbox`; attempted twice,
      blocked by provider quota — Anthropic 429 on the shared workspace, and
      the OpenAI key is a ChatGPT/Codex account that rejects `gpt-4.1`)
- [x] `msb cp` confirmed for binary content and missing paths
      (`TestBinaryFileRoundTrip`, `TestReadMissingFile`)
- [x] `SetNetworkPolicy` either enforces the policy or refuses it
      (it enforces every mode; the refusal test was replaced by
      `TestMicrosandboxEnforcesNetworkPolicy`, per its own instruction)
- [x] `docs/SANDBOX.md` records what was verified and on what hardware
- [ ] Verified on macOS with Apple Silicon
- [x] `SetNetworkPolicy` actually enforces an allow-list
      (real egress: the listed host answers, an unlisted one does not)

### Watch for

What remains is the live suite against microsandbox and macOS on Apple
Silicon. Neither has run, so do not let the docs imply either is proven.

---

## T-014 — Cross-process run ownership

**Priority** P2 · **Size** M · **Spec** §4.7

### Why

`FileJournal` serialises writes inside one process with a per-run mutex. It
takes no cross-process lock. Two processes appending to the same run
interleave records and assign the same sequence number to different records.
The file stays readable, the replay is wrong, and the run can repeat work it
already did.

`README.md` and `SECURITY.md` both state this as a deployment constraint, so
it is documented rather than hidden. It still limits BONNIE to one server per
journal.

### Do

Pick one, and write down why:

- **Lock file per run.** Cheap, works on one host, does nothing for a shared
  network filesystem.
- **A journal backed by a database.** Removes the problem completely and
  gives multi-host deployment, at the cost of a dependency and a second
  `Journal` implementation.

Either way, the conformance suite in `runtime/journal_conformance_test.go` is
where the new implementation proves itself.

### Acceptance criteria

- [ ] Two processes cannot corrupt one run
- [ ] The failure is a clear error, not silent interleaving
- [ ] The chosen approach and its limits are documented in `docs/SPEC.md` §4.7
- [ ] Any new journal passes the shared conformance suite

---

## T-015 — `channel` package has no tests

**Priority** P2 · **Size** S

### Why

`channel/channel.go` defines the transport interfaces that `channel/http`
implements. It has no test file. The interfaces are exercised indirectly
through the HTTP adapter, which is real coverage, but nothing pins the
contract itself — so a change to `SessionRef` or `Inbound` that breaks a
future adapter fails only in that adapter.

### Do

1. Add a compile-time assertion that the HTTP channel satisfies every
   interface, in `channel`, not only in `channel/http`.
2. Add a table-driven test for `TurnPolicy` semantics, so "steer" and "queue"
   mean the same thing to every future adapter.
3. Consider a `channeltest` sub-package holding a conformance suite a new
   adapter can run, in the same spirit as the journal and sandbox suites.

### Acceptance criteria

- [x] `channel` has a test file — compile-time assertions for every adapter
      in the repo, plus a wire-format pin on the `TurnPolicy` values
- [x] A new adapter has a documented way to prove it satisfies the contract —
      `channeltest.RunConformance`, which the HTTP adapter joins in
      `channel/channel_test.go`. It covers address resolution, attach-never-
      creates, both turn policies against a held-open turn, refusal of an
      unknown policy, suspension and respond, and cancellation
- [x] An unknown `TurnPolicy` is refused (`channel.ErrUnknownTurnPolicy`),
      never guessed — it used to fall through and silently queue

---

## Archive: shipped

Each of these is complete and covered by tests. The detail that mattered is
now in the code and in `docs/SPEC.md`; this is a pointer, not a spec.

| ID | Delivered | Where |
|---|---|---|
| T-001 | Live-model integration test behind the `integration` tag; skips without a key | `runtime/integration_test.go` |
| T-002 | Lossless replay — `Record.Payload` carries the typed `kit.LLMMessage` | `runtime/session.go`, `runtime/replay_fidelity_test.go` |
| T-003 | Torn-write repair drops an incomplete trailing tool-calling step | `runtime/repair.go`, `runtime/repair_test.go` |
| T-004 | `FileJournal` — JSONL, one file per run, fsync policy, `Persisted()` on the interface | `runtime/filejournal.go` |
| T-005 | `Runner.Cancel`, `Runner.Steer`, `RunCancelled`, and the event bus | `runtime/runner.go`, `runtime/events.go` |
| T-006 | `channel/http` — six routes, NDJSON stream with a reconnect cursor, journalled address map | `channel/http/` |
| T-007 | `bonnie serve`, `runs list`, `runs show --json`, graceful SIGINT | `cmd/bonnie/` |
| T-008 | `examples/minimal` and `examples/hitl-restart`, which really does exit between phases | `examples/` |
| T-010 | `CONTRIBUTING.md`, `SECURITY.md`, issue and PR templates, `CHANGELOG.md` | repo root |
| — | `sandbox` package: `local`, `docker`, `microsandbox`, 18-case conformance suite, no new dependencies | `sandbox/` |

### What the archive is missing on purpose

The original task file carried the full rationale for each item. That
rationale now lives where it is enforced:

- Verified Kit facts and their file:line citations → `docs/SPEC.md` §3
- Every known risk, resolved or open → `docs/SPEC.md` §4
- The invariants a change must preserve → `docs/SPEC.md` §8
- Sandbox adapter contracts → `docs/SANDBOX.md`

---

## Definition of done for `v0.1.0`

The release ships when a user can:

1. Start a durable run from Go or over HTTP.
2. Have it park on `ask_human`, **kill the process**, restart, and resume to
   completion.
3. Inspect what happened with `bonnie runs show`.

All three are verified, against a live model, in this order:

1. `examples/minimal`, and `POST /runs` on a running `bonnie serve`.
2. `examples/hitl-restart`, whose first phase calls `os.Exit`, plus
   `TestLiveSuspendAndResume`.
3. `bonnie runs show` renders the timeline and valid `--json`.

What remains before tagging is T-011. T-009 resolved itself: Kit `v0.106.0`
answered the asks, and BONNIE adopted the seams.
