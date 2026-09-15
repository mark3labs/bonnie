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
| T-022 | TUI transcript replay on reopen | P2 | M | — |
| T-019 | Evals against a discovered agent | P2 | L | — |

### Deferred

| ID | Title | Why |
|---|---|---|
| T-013 | Verify the microsandbox adapter on real hardware (macOS box only) | no access to an Apple Silicon machine; everything code-side is verified on Linux/KVM. Reopen when hardware is available. |

### Shipped after `v0.1.0`

| ID | Delivered | Where |
|---|---|---|
| T-027 | `goreleaser` published a commit list, so T-011's "the notes state the claims and the limits" was met only by a person editing the body after every tag — done by hand at `v0.1.0` and `v0.4.0`. `release.yml` now slices the tag's section out of `CHANGELOG.md` and passes it to `--release-notes`; a tag whose version has no section fails the workflow, naming the missing heading, instead of publishing an empty body. The extractor matches the bracketed version exactly (`0.5` does not match `0.5.0`, `0.1.0` does not match `0.10.0`) and stops at the next `## [`. Proven on the `v0.5.0` tag: 170 lines of notes, no human edit | `scripts/release-notes.sh`, `scripts/release_notes_test.go`, `.github/workflows/release.yml`, `Taskfile.yml` |
| T-026 | The microsandbox conformance flake, which was two defects. **The harness**: `backends()` built a provider per test case, so 18 parallel cases issued ~20 concurrent `msb create` calls and locked msb's own SQLite store — a load a real host never produces, because `serve.go` shares one provider whose mutex serialises every create. One shared provider per backend took ~0/10 to 8/10. **The adapter**: `msb ps --all` intermittently returns an empty list with exit 0 while sandboxes run, so `exists()` reported absent and the create was refused; `ensureRunning` now adopts an "already exists" refusal, with `checkPolicy` on every adopt path. 20/20 runs pass. `msbError` keeps msb's `→` cause lines that `firstLine` dropped — the truncation that made the whole thing misdiagnosed | `sandbox/microsandbox.go`, `sandbox/conformance_test.go`, `sandbox/sandbox_test.go`, `docs/SPEC.md` §4.11 |
| T-034 | The public-API boundary moved from a CI job to a Kit extension: `.kit/extensions/kit-boundary.go` blocks a `write`/`edit` that adds `kit/internal/...` or `charm.land/fantasy` to a `.go` file **in this repository**, naming the import and the way out. The `boundary` CI job is deleted — `depguard` in the `lint` job already denies both paths by prefix, whatever the module layout, so nothing was lost. Two defects that unit tests missed were found by driving it live and fixed: it read a composite-literal element as an import, and it applied to other checkouts including Kit's own. BONNIE cannot test it (Kit's harness signs its API with `internal/extensions` types — open ask in `docs/UPSTREAM.md`) | `.kit/extensions/kit-boundary.go`, `.github/workflows/ci.yml`, `docs/SPEC.md` §2, `docs/UPSTREAM.md` |
| T-033 | Cross-channel hand-offs and proactive sessions: `channel.Outbound` in every route handler, `channel.Receiver` on all four platform adapters, `chat.Core.Proactive` (bind before dispatch so a mid-turn reply continues the run), the initiating principal carried to the destination run | `channel/channel.go`, `channel/chat/chat.go`, all four adapters, `run.go`, `handoff_test.go` |
| T-029 | The GitHub App channel: comment mentions and bound-thread replies become turns; issue, PR, and review-thread addresses; the PR diff as per-turn context; `eyes` reactions; signature verification and delivery dedup; per-event installation tokens that never reach the journal; `OnIssue`/`OnPullRequest`/`OnCheckSuite` hooks; `bonnie.WithGitHub` | `channel/github/`, `options.go`, `docs/CHANNELS.md` |
| T-031 | Idempotent start (`operation_id`, namespaced per authenticated principal, refused anonymous) and a stable `code` on every error body, listed in `docs/CHANNELS.md` | `channel/http/http.go`, `channel/http/errors_test.go` |
| T-032 | Session controls: `Reset`/`Clear`/`Compact` on `SessionRef` and the HTTP channel, `RunRetired` (the only non-revivable terminal state), `RecordClear` (an append-only forget), `/new` in every chat surface, the core owns the address prefix with the channel's name | `runtime/controls.go`, `runtime/session.go`, `channel/channel.go`, `channel/chat/chat.go`, `channeltest/` |
| T-028 | Normalised turn: `runtime.Input{Context, Title, Origin}`, `RecordContext` journalled and shown to the model through `OnContextPrepare` for one turn only, `chat.Turn` with kinds, adapters set kind/title/context, HTTP accepts `context`/`kind`, `runs list` shows the title | `runtime/context.go`, `channel/chat/chat.go`, `channel/{slack,discord,telegram}`, `channeltest/`, `docs/SPEC.md` §3.7 |
| T-030 | The HTTP channel serves under `/bonnie/v1`; `/bonnie/` is reserved and a channel that mounts there is refused at startup by name; `GET /bonnie/v1/health` and `GET /bonnie/v1/info`; `bonnie.WithName` | `channel/channel.go`, `channel/http/http.go`, `run.go`, `namespace_test.go`, `cmd/bonnie/tui/client.go` |
| T-011 | Applied again for `v0.4.0`, tagged at `9ccb959` and published 2026-09-14. Verified post-publish: the downloaded `linux_amd64` artifact prints `bonnie 0.4.0` (the injected version, not `dev`), its checksum matches, it is statically linked, and it serves. The auto-generated notes were again only a commit list, so the body was replaced with the `CHANGELOG.md` section — claims and limits both stated | `CHANGELOG.md`, [release v0.4.0](https://github.com/mark3labs/bonnie/releases/tag/v0.4.0) |
| T-025 | The journal is SQLite, pure Go (no CGO): `SQLiteJournal` over one `<root>/journal.db`, WAL, one transaction per step, concurrent writers safe instead of refused, legacy `runs/*.jsonl` imported on open. `FileJournal`, the per-run lock file, and the handle map are gone | `runtime/sqlitejournal.go`, `runtime/legacy_journal.go`, `runtime/journal.go`, `docs/SPEC.md` §4.14 |
| T-024 | The manifest is gone: configuration is code. The root `bonnie` package owns the serving path (`Main`, `Run`, options) and the default layout as constants; codegen registers the tree through `bonnie.Register` from `init`; `bonnie init` scaffolds a one-call `main.go`; `serve` is the flag-only generic host; `yaml` and `toml` return to indirect | `bonnie.go`, `run.go`, `options.go`, `agent/scaffold.go`, `agent/generate.go`, `cmd/bonnie/`, `internal/treetest/`, `docs/L2.md` |
| T-023 | Audit fixes: the event stream releases a parked send on disconnect (goroutine leak), reads of unknown runs no longer grow the file journal, `--sandbox-image` reaches `auto` and is refused by `local`, reserved runs are off the wire and out of `sandbox prune`, `channel/http` caps a body and maps `ErrRunOwnedElsewhere` to 409; `chat.DeliveryText` and the workspace-default rule replace three copies each; `EventBus.Backlog` removed | `runtime/events.go`, `runtime/runner.go`, `runtime/filejournal.go`, `channel/chat/chat.go`, `channel/http/http.go`, `cmd/bonnie/sandbox.go`, `docs/SPEC.md` §4.12 |
| T-021 | Built-in terminal TUI: `bonnie dev` opens a scrollback chat; `bonnie chat` connects to an HTTP channel; cursor reconnect and cancel; assistant messages render as markdown through herald-md (Kit's typography patterns) | `cmd/bonnie/tui/`, `cmd/bonnie/chat.go`, `cmd/bonnie/dev.go` |
| T-018 | L2 codegen: tool discovery (`agent/gen`), `bonnie dev` (fsnotify loop), `bonnie build` (go:embed + static binary), `--dry-run`; import allowlist; duplicate-name refusal; idempotent codegen | `agent/generate.go`, `agent/generate_test.go`, `cmd/bonnie/build.go`, `cmd/bonnie/dev.go`, `cmd/bonnie/l2_test.go` |
| T-020 | Chat channels: Slack, Discord, Telegram adapters with verified webhooks, dispatch (a reply to a parked run resumes it), threaded delivery; `channel/chat` shared plumbing; mounted by `bonnie.WithSlack`/`WithDiscord`/`WithTelegram` with env-only credentials (manifest keys until T-024) | `channel/slack/`, `channel/discord/`, `channel/telegram/`, `channel/chat/`, `options.go`, `docs/CHANNELS.md` |
| T-017 | L2 core: `bonnie init` (always a Go module; `--tools` adds a sample tool), workspace seeding, and the strict manifest loader — the loader and `serve --agent` were removed by T-024 | `agent/scaffold.go`, `cmd/bonnie/init.go`, `sandbox/seed.go` |
| T-012 | The sandbox lifecycle is journalled: `RecordSandbox`, a resume note when a workspace is gone, `runs show` timeline, `bonnie sandbox prune` | `runtime/journal.go`, `runtime/session.go`, `sandbox/lifecycle.go`, `cmd/bonnie/sandbox.go` |
| T-014 | Cross-process run ownership: `flock` per run, `ErrRunOwnedElsewhere` on a second writer, reads unlocked, per-host limit stated | `runtime/filejournal.go`, `filejournal_test.go` |
| T-015 | `channeltest` conformance suite; compile-time assertions in `channel`; unknown `TurnPolicy` refused, not guessed | `channeltest/`, `channel/channel_test.go` |
| T-016 | Events survive a reconnect past the backlog: every event anchored to a journal record, `Runner.StreamEvents` replays the journal, the stream survives a restart | `runtime/events.go`, `runtime/runner.go`, `runtime/events_test.go` |

## T-016 — Journal-backed event catch-up

**RESOLVED.** eve's contract — reconnect, rewind, and replay all return the
event at the same position — adopted on BONNIE's substrate: the journal is
the durable stream. The design, the anchors, and the live-only exceptions are
in `docs/SPEC.md` §4.8.

### Resolved by upstream

**T-009 — file the four upstream Kit issues — is moot.** Kit `v0.106.0`
(PR `mark3labs/kit#135`) answered all four asks before anything was filed.
BONNIE adopted the batch-append seam the same day: `Session.AppendStep`
plus the `runtime.StepJournal` optional interface. `docs/archive/UPSTREAM.md`
records what each ask became, and `docs/SPEC.md` §3 was re-verified against
`v0.106.0`. T-011 no longer depends on it.

### Shipped in `v0.1.0`

T-001 … T-008 and T-010, plus the `sandbox` package — and T-011, which
tagged and published the release itself. See
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
at the top of this file and [`docs/archive/UPSTREAM.md`](archive/UPSTREAM.md)
for what each ask became. The section below is the task as it was written,
kept for the record.

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

## T-011 — Tag and release

**RESOLVED.** `v0.1.0` was tagged at `15e1727` and published on 2026-09-12;
`release.yml` completed successfully. Verified post-publish, not assumed:
a downloaded `linux_amd64` artifact prints `bonnie 0.1.0` (the injected
version, not `dev`), and the GitHub release notes lead with the three claims
and the limits — the auto-generated notes had only the commit list, so the
notes were edited to the required form.

**This task is the release checklist.** It stays here as the procedure every
subsequent tag follows, not as open work. Applications:

| Version | Commit | Date |
|---|---|---|
| `v0.1.0` | `15e1727` | 2026-09-12 |
| `v0.2.0` | `b1fff6d` | 2026-09-13 |
| `v0.3.0` | `38a511d` | 2026-09-13 |
| `v0.4.0` | `9ccb959` | 2026-09-14 |
| `v0.5.0` | `f9794d1` | 2026-09-15 |

### `v0.5.0`, 2026-09-15 — every box confirmed

- [x] `task release-check` — goreleaser 2.17.1, 1 config validated
- [x] `task release-snapshot` — four targets, archives and checksums
- [x] All CI jobs green on `master` at the tagged commit `f9794d1`:
      `test` and `lint` (run `34968133708`). There is no `boundary` job to
      confirm any more — see the note above
- [x] `CHANGELOG.md` carries a `[0.5.0]` section in Keep-a-Changelog shape
- [x] Release notes state the three claims **and** the limits — in the tag
      annotation and, **for the first time with no human edit**, on the
      GitHub release, because T-027 landed in this release
- [x] Tag pushed; `release.yml` run `34968699731` succeeded
- [x] Five artifacts published; the downloaded `linux_amd64` binary prints
      `bonnie 0.5.0` (the injected version, not `dev`), its checksum
      verifies, and it is statically linked

**Re-checking the notes at tag time caught three defects**, exactly as the
`v0.4.0` entry below warns. The section written across the increment had two
separate `### Changed` headings, omitted `bonnie.WithName` from *Added*
though it is a new exported option, and carried neither the claims nor the
limits. The limits were then taken from the current `README.md` rather than
copied from the `0.4.0` section, which still describes the `flock` ownership
model that T-025 replaced with SQLite. **Copying the previous release's
limits forward is how a stale limit gets published.**

### `v0.4.0`, 2026-09-14 — every box confirmed

- [x] `task release-check` — goreleaser 2.17.1, 1 config validated
- [x] `task release-snapshot` — four targets, archives and checksums, 2m16s
- [x] All CI jobs green on `master` at the tagged commit, including
      `boundary` (run `34862857167`)
- [x] `CHANGELOG.md` carries a `[0.4.0]` section in Keep-a-Changelog shape
- [x] Release notes state the three claims **and** the limits — in the tag
      annotation and, after an edit, on the GitHub release
- [x] Tag pushed; `release.yml` run `34863448503` succeeded in 5m20s
- [x] Five artifacts published; a downloaded binary prints `bonnie 0.4.0`,
      its checksum verifies, and it serves

**`0.4.0` had been written into `CHANGELOG.md` and never tagged.** Four more
commits then accumulated under `[Unreleased]`, so the two sections were merged
into one `0.4.0` rather than tagging `v0.5.0` and leaving a changelog heading
no tag would ever match. Merging found three claims that intra-release churn
had made false — `Manifest.WorkspaceDir` under *Added* after T-024 deleted the
manifest, the seeding fix described through a manifest key that no longer
exists, and two new exported options missing from the list. **Notes written
mid-release must be re-checked against the code at tag time**; a later commit
in the same release can invalidate an earlier entry, and nothing in CI
notices.

**`goreleaser` does not read `CHANGELOG.md` — fixed in `v0.5.0`.**
`.goreleaser.yaml` builds the body from commit subjects, so the published
notes were a commit list until someone replaced them. This cost an edit at
`v0.1.0` and again at `v0.4.0`. T-027 closed it: `release.yml` now slices the
tag's section out of `CHANGELOG.md` with `scripts/release-notes.sh` and passes
it to `goreleaser --release-notes`, and a tag with no matching section fails
the workflow instead of publishing an empty body. **Step 3 below is no longer
manual** — but the section must exist before the tag is pushed.

**The `boundary` CI job no longer exists.** T-034 deleted it; `depguard` in
the `lint` job is the authority and denies both forbidden paths by prefix.
"All CI jobs green" now means `test` and `lint`.

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
- [x] No `replace` directive is committed in `go.mod`
- [x] `goreleaser check` passes; `goreleaser build --snapshot --clean`
      builds working binaries on all four targets, and the binary prints the
      injected version
- [x] All CI jobs green on `master`, including `boundary`
      (green; the old golangci-lint pin was the only failure)
- [x] Release notes state both the claims and the limits
      (in the tag annotation and the GitHub release)
- [x] Tag pushed and artifacts published
- [x] A downloaded binary prints the injected version

### Watch for

- **CI was broken from the first commit and nobody had looked.** Every run on
  `master` failed. The `lint` job pinned golangci-lint `v2.1.0`, which is
  built with go1.24, against a `go.mod` that asks for 1.27; golangci-lint
  refuses to load its config when its own toolchain is older than the target
  and exits 3. `test` and `boundary` were green throughout, so only `lint` was
  ever failing. Fixed by pinning `v2.13.2` (built with go1.27.0). Raise that
  pin whenever the `go` line in `go.mod` moves.
- The sandbox conformance suite **skips** any backend whose runtime is not
  on the machine, and a bare CI runner has neither Docker nor microsandbox.
  CI green does not mean those adapters were exercised. The microsandbox
  defects fixed in T-013 were invisible to CI for exactly this reason, and
  the Docker backend went unexercised for as long because the daemon was
  simply not running locally — nothing said so louder than one `SKIP` line
  among many. When a backend matters, check that it *ran*, not that the
  suite was green.

---

## T-012 — Journal the sandbox lifecycle

**RESOLVED.** The design — a `RecordSandbox` kind, a resume note that is
never silent, a prune command rather than a sweep — is in `docs/SPEC.md`
§4.10. The text below is the task as it was written.

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

- [x] `RecordSandbox` is written when a sandbox opens
      (`LazyOpener`, retried until the journal takes it)
- [x] `bonnie runs show` displays the backend and sandbox ID
- [x] A resumed run whose workspace is gone reports that, and does not
      silently present an empty one — the decision was a note in the
      conversation, not a failure, and it is documented in §4.10
- [x] Terminal runs can have their sandboxes reclaimed
      (`bonnie sandbox prune`, with `--dry-run`; a `serve` sweep is future work)
- [x] A test covers the missing-workspace path
      (`TestCheckRecordedSandboxNotesTheLoss`, and the prune test end to end)

### Watch for

Do not let `runtime/` import `sandbox/` (invariant 6 is about `channel/`, but
the same layering logic applies). The record kind belongs in `runtime`; the
code that writes it belongs in `sandbox`. That held: `LazyOpener` takes the
`*runtime.Session` and calls its record methods, and the resume check lives
in `sandbox.Agent`'s factory, which is the only place that can ask a provider
whether its sandbox is still there.

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
- [x] The live sandbox suite passes against microsandbox
      (4/4, with `BONNIE_TEST_SANDBOX=microsandbox`; first blocked five
      attempts by Anthropic workspace quota, then passed against
      `opencode/kimi-k2.5`, which is the live suites' default now)
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

The only open box is hardware: macOS on Apple Silicon. Everything code-side
was verified on Linux with KVM (`msb` 0.6.18), including the live suite.

Also worth keeping: the live suites default to `opencode/kimi-k2.5` because a
test model must not share a quota with a busy workspace — the Anthropic
workspace behind us returned 429 for agent-sized requests all day while a
single small curl succeeded, which is exactly the trap a shared quota sets.

---

## T-014 — Cross-process run ownership

**RESOLVED.** A lock file per run, chosen over a database journal. The
design, decisions, and limits are in `docs/SPEC.md` §4.7. The text below is
the task as it was written.

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

- [x] Two processes cannot corrupt one run — the second owner's write is
      refused
- [x] The failure is a clear error, not silent interleaving
      (`ErrRunOwnedElsewhere`, all three write paths, per-run granularity,
      release on close)
- [x] The chosen approach and its limits are documented in `docs/SPEC.md` §4.7
      — lock file per run; per host only; refusing is not coordinating
- [x] Any new journal passes the shared conformance suite (the built-ins
      still do; ownership is `FileJournal`-specific and tested beside it)

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

## T-020 — Chat channels: Slack, Discord, Telegram

**RESOLVED.** eve's chat-channel devex, on BONNIE's contract: dispatch,
steering, delivery, and HITL-in-chat, with verification front and centre.
The full page is [`docs/CHANNELS.md`](CHANNELS.md); the reasoning below is
the record.

**Priority** P1 · **Size** L · **Spec** [`docs/CHANNELS.md`](CHANNELS.md)

### Why

A durable run is only reachable by the person who made the HTTP call. The
team already lives in a chat; the run should live there too. eve's chat
channels are the devex reference: dispatch rules decide which platform
events reach the agent, steering handles the message that arrives mid-turn,
and a parked run's question lands in the thread with the next reply
answering it.

### Decisions, and why

- **Zero new dependencies.** The SANDBOX.md precedent ("why the CLI and
  not the SDKs"), applied to transports: the platforms' webhooks are plain
  HTTPS + JSON, and every verification scheme is stdlib — Slack's v0
  HMAC-SHA256, Discord's Ed25519, Telegram's secret-token header. No SDK,
  no websocket dependency, no bigger binary.
- **Shared plumbing in `channel/chat`.** The journalled address map,
  per-run locks, the `SessionRef` implementation, and dispatch moved out of
  `channel/http` — four adapters must not keep four copies of the thing
  that makes a conversation durable. `channel/http` now uses it too, and
  its tests are the refactor's safety net.
- **Dispatch: a reply to a parked run resumes it.** A chat surface cannot
  say "this is a resume". `Start` on a waiting run would open a fresh turn
  and strand the suspension, so `chat.Route` checks the run's state and
  routes to `Resume`. The check and the entry run under the turn lock.
- **Verification is mandatory at the wiring.** A manifest chat key without
  its credentials in the environment is a startup error naming the
  variable — a webhook that does not verify its caller is a door with no
  lock. New invariant 15.
- **Deliveries are one message per turn.** eve edits the message as tokens
  arrive; BONNIE's chat channels post at boundaries. The NDJSON stream is
  the live-output surface.

### Acceptance criteria

- [x] Each adapter joins `channeltest.RunConformance` — the Inbound
      contract (addressing, policies, suspension, cancellation) is proven
      without any platform (three tests named `TestConformance`)
- [x] Each webhook verifies its scheme and refuses tampered bodies, stale
      timestamps, and missing secrets
- [x] Slack redelivers are dropped by `event_id`; a retried event cannot
      send the same message twice
- [x] A group message that is not for the bot creates no run (Telegram
      mention/command, Slack thread-binding, Discord slash-command-only)
- [x] A parked run's question is delivered into the chat; the next message
      there resumes it (`TestReplyToAParkedRunResumesIt`,
      `TestAskAnswersAParkedRun`)
- [x] `serve --agent` mounts enabled channels from the manifest, banner
      names them, and a missing credential is a named error
      (`TestServeMountsChatChannels`, `TestChatChannelNeedsItsSecrets` — both
      removed with the manifest by T-024; the same contract is now guarded
      at the option surface by `TestChatChannelOptionsMount`)
- [x] No new `go.sum` entries (verified: `git diff go.sum` is empty)

### Watch for

The adapters' live behaviour against the real platforms is verified only by
reading their API docs and testing against recorded shapes — there are no
credentials on the development machine, and the integration-tagged live
suites do not cover chat. A first real deployment should watch three
things: Slack's event subscription configuration (which events arrive at
all), Discord's command registration, and Telegram's `setWebhook` secret.
The fakes pin the wire shapes the adapters assume; if a platform changes
them, the tests fail instead of the conversation.

---

## T-017 — L2 core: the manifest, `bonnie init`, `serve --agent`

**RESOLVED.** The zero-Go path ships: scaffold, edit one file, serve. The
loader is one strict code path for all three formats; precedence is one
function for every setting; the banner names the source that won. What
differs from the plan below is recorded in the resolution notes.

**Priority** P1 · **Size** M · **Spec** [`docs/L2.md`](L2.md) §3, §4, §6

### Why

The runtime core is done and hardened (T-012, T-014, T-015, T-016). What
deletes the adoption gap against eve is a five-minute path that needs no Go:
scaffold, edit instructions, serve. eve's getting-started is that path. BONNIE
today requires a hand-written `main.go` for anything.

### Do

1. The manifest loader per `docs/L2.md` §4: `agent.yaml` primary,
   `agent.toml` and `agent.json` accepted; one loader, one schema; strict
   unknown-key rejection and `apiVersion` check by key-set diff, so all
   three formats get identical errors.
2. Discovery with the ambiguity refusal: two manifests in one root is an
   error naming both; `--config` names one.
3. `bonnie init` — scaffold `agent.yaml` (or `--format`), `instructions.md`,
   `skills/`, `workspace/`. Never overwrite. Default is the zero-Go path;
   `--tools` adds `go.mod`, `main.go`, and a sample tool.
4. `serve --agent DIR` — instructions from disk, model, sandbox, channel
   binding from the manifest; flags override and the banner names each
   setting's source.
5. The refusal: a tree with `tools/` or `go.mod` is refused with an error
   naming `bonnie build` (invariant 13). `kind: none` prints the
   no-isolation warning.

### Acceptance criteria

> **Historical.** T-024 removed the manifest and `serve --agent`, and the
> tests named below went with them. They are kept as the record of what was
> accepted at the time; do not grep for them. The criteria that outlived the
> manifest are re-guarded under T-024.

- [x] `bonnie init` in an empty directory produces a tree that
      `bonnie serve --agent .` serves; curl round-trips one run
      (`TestServeAgentRoundTrip` — scaffold, serve, POST, GET, 404; the
      smoke test against the real binary hit the live provider and the
      manifest's model reached it)
- [x] `--format toml` and `--format json` scaffolds parse to the same
      struct as YAML (`TestScaffoldParsesInEveryFormat`)
- [x] Unknown manifest key → error naming the key, all three formats
      (`TestUnknownKeyIsNamed`, including a nested `sandbox.netwrok`)
- [x] Unknown `apiVersion` → error (`TestAPIVersionIsChecked`)
- [x] Two manifests in one root → error naming both; `--config` resolves it
      (`TestTwoManifestsInOneRootIsRefused`)
- [x] `init` refuses to overwrite any existing file and changes nothing
      (`TestScaffoldNeverOverwrites` — the pre-flight names every blocker
      and writes nothing)
- [x] `serve --agent` refuses a tree it cannot fully honor, naming
      `bonnie build` (`TestResolveServeRefusesGoTree` — go.mod, a tools
      directory, and the `--tools` scaffold itself)
- [x] `go.sum` gains no new modules — yaml, toml, and fsnotify are already
      in the graph as Kit's transitive deps; promote, do not add
      (verified: `git diff go.mod go.sum` is empty)

### Resolution notes — what was learned

- **The mark3labs modules are publicly fetchable** (resolved 2026-09-13).
  `proxy.golang.org` serves `bonnie` and `kit`, and sum.golang.org has
  entries, so a scaffolded `go.mod` tidies and builds off the proxy with no
  workspace and no `GOPRIVATE`. `init --tools` writes a bare `go.mod`;
  `go mod tidy` fills it. The guard test builds the fresh scaffold against
  the local checkout (`TestScaffoldToolsModuleBuilds`) to stay hermetic, but
  a standalone `go build` off the proxy is the verified public path.
- **`skills:` is reserved, not wired.** Seeding skill files into a sandbox
  nothing reads would be a dead key — a control nothing applied. The key
  is refused with the same message class as `mcp`, until a skill-loading
  story exists. `workspace:` is wired as of the workspace-root fix: with a
  sandbox, `sandbox.Seeded` mirrors the seed directory over the `Sandbox`
  interface, skip-if-exists, so a resumed run never has its edits reverted
  (`sandbox/seed_test.go`); without one, the same directory is the working
  directory of Kit's file tools. It was written and tested but **never
  called** until then — accepted and ignored, which invariant 13 forbids.
  The wrapper forwards `Networked`, `ExistenceChecker`, and `RunDeleter`, so
  wrapping never widens what a caller can request.
- **`init --tools` writes a `bonnie_gen.go` stub** so the fresh module
  compiles before codegen exists. main.go (authored, never rewritten)
  calls `discoveredTools()` — the symbol the real generator will define.
  The stub carries the DO-NOT-EDIT banner and is disposable per the L2
  contract.
- **A manifest with no instructions file is refused** at serve time. The
  default path (`instructions.md`) is a promise the tree must keep; eve's
  "one required file" rule is adopted with it.

### Watch for

Do not add an `auth` key to the manifest. The channel carries a `Principal`
and verifies nothing; a config key that promises authentication it cannot
deliver is the SECURITY.md failure in manifest form. Also do not let the
loader grow three bespoke decoders — the strictness must be one code path,
or the formats will drift.

---

## T-018 — L2 codegen: tool discovery, `bonnie dev`, `bonnie build`

**RESOLVED.** The graduation path closes: a tree's tools are discovered at
build time by codegen, hot-reloaded by `bonnie dev`, and shipped as the user's
own static binary by `bonnie build`. The recording below is the record of what
shipped and the decisions that were made while building it.

**Priority** P1 · **Size** M · **Blocks on** T-017 · **Spec**
[`docs/L2.md`](L2.md) §5, §6

### Why

The manifest covers data. Custom tools are Go, and Go is compiled — so tools
are wired by codegen, watched by `bonnie dev`, and shipped as the user's own
binary by `bonnie build`. This is the graduation path: zero Go to start, a
single static binary at the end.

### What shipped

1. **Codegen (`agent/generate.go`).** `Discover` walks `tools/<name>/` and
   requires each directory to export `func Tool() kit.Tool`; `Plan.Render`
   produces `bonnie_gen.go`, byte-identical across runs. The generated file
   defines `discoveredTools()` and the embed accessors
   (`embeddedInstructions`, `embeddedSkills`, `embeddedWorkspace`). It imports
   only the tree's tool packages, `embed`, and `kit/pkg/kit` — it cannot emit a
   boundary violation. Duplicate declared names (two directories passing the
   same name to `kit.NewTool`) are refused with both paths named, before a
   provider silently keeps one.
2. **`bonnie build`.** Generates the wiring, embeds instructions (and
   skills/workspace when they hold real files), and `go build`s one static
   binary `./<title>`. `--dry-run` prints the discovery plan.
3. **`bonnie dev`.** The fsnotify loop: watch the tree (manifest,
   instructions, `skills/`, `workspace/`, `tools/**`, `go.mod`, `go.sum`),
   debounce, regenerate, rebuild, and gracefully restart the child with
   SIGTERM. A parked run keeps no compute and lives in the journal, so the
   restarted child resumes it. `--dry-run` prints the plan.

### Decisions, and why

- **The generator registers nothing; it calls `Tool()`.** The runtime tool
  name is whatever the code passes to `kit.NewTool`. So the generated file
  imports each tool package and calls `.Tool()`, and the *only* thing codegen
  can validate about names is the declared `NewTool` name. That is what
  duplicate detection is on: two directories that both declare "echo" would
  collapse to one tool at run time, so codegen refuses before it can happen.
  Directory-name-is-tool-name (§3) is the authoring convention; the duplicate
  check is the guard on the real runtime name.
- **The embed slots are always declared.** A `//go:embed` directive is emitted
  only for the paths discovery found, but `_instructions`/`_skills`/
  `_workspace` are always declared, so the accessors and the `embed` import
  compile whether or not a slot is embedded. A bare `.gitkeep` is never real
  content, so it does not trigger an embed.
- **`main.go` falls back to the embedded instructions.** The scaffold's
  authored `main.go` reads `instructions.md` from disk first (the dev path);
  a `bonnie build` binary has no such file, so it falls back to
  `embeddedInstructions()`. That is what makes the build output serve with
  no agent tree beside it.
- **A bad save never takes the child down.** A change that does not build is
  reported and the loop keeps the running child serving. Only the initial
  build failure, or a non-build error, stops `dev`.
- **`go.mod` promotes `fsnotify`, adds nothing.** `fsnotify` was already in
  the graph as a transitive dependency; it is now a direct import. No new
  `go.sum` entry.

### Acceptance criteria

- [x] Codegen twice over the same tree is byte-identical
      (`TestCodegenIsIdempotent`)
- [x] A duplicate tool name fails with both paths named
      (`TestCodegenRejectsDuplicateToolName`)
- [x] Generated imports match the allowlist (guard test greps the file;
      `TestGeneratedImportsMatchAllowlist`, plus the boundary job)
- [x] Authored files are never rewritten; deleting `bonnie_gen.go` and
      regenerating restores an equivalent build
      (`TestGeneratorRewritesOnlyGeneratedFile`, `TestGeneratedFileCompiles`)
- [x] **The dev-restart test:** a run parks in the child, `dev` restarts, a
      respond in the new child completes it — crosses a process boundary in
      spirit (`TestDevRestartCompletesParkedRun`)
- [x] `bonnie build` output serves a full run on a host with no Go and no
      BONNIE install, with instructions served from the embedded copy
      (`TestBuildOutputServesEmbeddedInstructions`)
- [x] `--dry-run` prints files found, tools generated, embed set
      (`TestRunBuildDryRun`, `TestPlanStringNamesDiscovery`)

### Watch for

- **`go:embed` patterns cannot climb** with `..`. The generated file is at the
  module root and embeds `instructions.md`, `skills`, and `workspace` relative
  to it. Do not move authored files under a nested directory without moving
  the generator with them.
- `runtime/` stays free of L2 imports — the generator is an L4 tool that
  emits L0/L1 calls. It lives in `agent/`, not `runtime/`.

---

## T-021 — Built-in terminal TUI

**RESOLVED.** `bonnie dev` now opens a minimal scrollback TUI, and
`bonnie chat` connects the same TUI to any running HTTP channel. Typing,
completed turns, and a hot-reload reconnect were verified live in tmux.
**Shipped in `v0.3.0`**, which also fixed the two event-stream defects the
first live uses exposed (§4.8.1 of the spec) and added the colored tool-call
rendering and the startup address lookup.

**Priority** P2 · **Size** M · **Reference**
[eve Dev TUI](https://eve.dev/docs/guides/dev-tui)

### Why

eve's `eve dev` opens a terminal UI beside the dev server: you type a prompt,
watch tool calls and the answer stream in, answer a parked run. BONNIE's own
`dev` had no such surface — it built and served, and a developer reached the
agent only over HTTP with `curl`. A single command that brings up a minimal TUI
is what deletes that gap.

### What shipped

1. **`cmd/bonnie/tui`** — a bubbletea (charm v2: bubbletea, bubbles, lipgloss)
   model that is an **HTTP client of the channel**. It holds one conversation
   (one run, resolved from an address), renders a scrollable transcript, and
   streams the run's events live from the journal-cursor. Because it speaks
   the wire, it works against any running channel, not only the one `dev`
   started.
2. **`bonnie chat`** — connect the TUI to a running channel
   (`--addr`, default `127.0.0.1:8080`), one durable conversation per `--run`
   address.
3. **`bonnie dev`** — the hot-reload serve loop now also opens the TUI against
   its own child. The child binds a free port (or `--addr`); the TUI connects
   to the real bound address, so a hot reload restarts the child and the run
   survives in the journal.
4. **Assistant markdown (added after the release).** Assistant entries render
   as markdown through `herald-md` (`cmd/bonnie/tui/markdown.go`), matching
   upstream Kit's own TUI, which uses the same pair of libraries. The patterns
   are Kit's: one cached `herald.Typography` (construction is expensive and
   must never run per frame), a palette plus per-element overrides instead of
   a `herald.Theme` literal (a literal zeroes the glyph fields), no paragraph
   margin, and `lipgloss.Wrap` because herald wraps nothing. The old
   `MaxWidth(100)` style truncated every assistant line past 100 cells —
   wrapping is the fix, verified live. User messages stay unrendered: typed
   text is not markdown. No syntax highlighting yet — that is a chroma
   dependency, pluggable later via `WithCodeFormatter`.

### Decisions, and why

- **HTTP client, not in-process runner.** The TUI never imports the runtime
  beyond the wire types. That keeps it usable against `serve`, `dev`, or a
  remote server, and keeps the framework packages terminal-free.
- **Walk ports 8080, 8081, 8082, ...** The first free loopback port is used
  and then kept for each hot reload. `--addr` overrides it with a fixed
  address. The listen-close-bind step has a short race, but a child bind
  failure is reported and never falls back to the manifest address.
- **Streaming is cursor-based and durable.** The transcript is rebuilt from
  the journal on reconnect, so a `dev` hot reload does not lose the
  conversation; live Kit deltas (message text, tool calls, reasoning) overlay
  it and never survive a restart, exactly as the event contract specifies.

### Acceptance criteria

- [x] `bonnie dev` opens the TUI against its serving child (scrollback, not
      alt-screen; port walks 8080, 8081, …)
- [x] `bonnie chat --addr` talks to any running channel
- [x] A first message starts a run; a question parks it; the next send resumes
      it (`TestModelFirstTurnParks`, `TestModelResumeAnswersAQuestion`)
- [x] Live events render into the transcript (tool calls, streamed text)
- [x] The HTTP client decodes the channel's wire shapes (`TestHTTPClientWire`)
- [x] A free port is chosen automatically, and `--addr` binds a fixed one
- [x] `go test -race ./cmd/bonnie/...` passes; no `charm.land/fantasy` import
- [x] tmux typing reaches the textarea and submits a live turn
      (`TestModelAcceptsTypedKeys`; verified live in tmux)
- [x] Stream re-opens from the last cursor after a `dev` child restart
      (`TestModelReconnectsFromLastCursor`; verified live across hot reload)
- [x] `ctrl+w` calls the channel cancel route (`TestModelCancelCallsChannel`,
      `TestHTTPClientWire`)

### Watch for

- The TUI is an L4 CLI surface. The layered packages (`runtime`, `channel`)
  stay terminal-free; do not move bubbletea into them.
- Bubble Tea v2 always requests basic Kitty key disambiguation. The original
  typing defect was not Kitty mode: `Model.Init` focused a copied textarea.
  `New` now focuses the textarea stored in the model.
- **Assistant markdown (T-021 extension, verified live 2026-09-14).**
  Assistant entries render through `herald-md` (`cmd/bonnie/tui/markdown.go`),
  with Kit's own patterns: cached `herald.Typography`, palette-plus-overrides
  theming, no paragraph margin, `lipgloss.Wrap` (herald wraps nothing — and
  the old `MaxWidth(100)` style **truncated**, losing every assistant line
  past 100 cells). Verified in tmux against `opencode/kimi-k2.5`: headings,
  lists, tables, code fences, wrapped prose, live streaming render, and the
  hot-reload reconnect all render correctly. The same run exposed two things:
  a fresh `bonnie chat` on an existing address shows an empty transcript —
  it opens at the served cursor by design, and no wire endpoint returns the
  past conversation (now T-022) — and a dev child built from a scaffold links
  the **published** release pinned in the tree's `go.mod`, not the working
  tree; testing a local change through `bonnie dev` needs a `replace` in the
  tree's `go.mod`, which is what `internal/treetest` writes for the tests.
- **The inline cursor is a screen coordinate, not a frame coordinate.**
  bubbletea v2's inline renderer moves the terminal cursor to the exact row
  the view reports. A scrollback transcript grows past the terminal height,
  so `View` must convert the frame row to a screen row (subtract the rows
  the screen scrolled past) — or the terminal clamps the move to its bottom
  row and the cursor leaves the input. Found in tmux; pinned by
  `TestViewCursorStaysOnTheInputRowWhenTheFrameExceedsTheScreen`.

## T-022 — TUI transcript replay on reopen

**Priority** P2 · **Size** M · **Found by** the T-021 tmux verification

### Why

`bonnie chat`'s help promises that re-opening the TUI with the same `--run`
address "continues the conversation from the journal". The run does continue —
the next send appends to the same durable run, verified live — but the
**transcript does not come back**. `GET /addresses/{address}` returns the run's
current journal cursor by design (SPEC §4.8.1), the TUI opens its stream
there, and nothing replays. A developer who closes the terminal reopens to an
empty screen and no way to see what the agent said before.

The journal holds everything a transcript needs — user messages, assistant
messages, tool calls, questions — so this is a wire gap, not a data gap.

### Do

1. A read-only wire endpoint that returns the past conversation in wire-event
   form, e.g. `GET /runs/{id}/transcript`, built from the journal records.
2. The TUI folds it into `entries` at startup, before the live stream opens.
   The existing dedupe paths (`finishAssistant`, `commit`) already tolerate a
   replayed confirmation arriving after the fetched history.
3. Decide the resume-note rendering: `[bonnie]` notes are user-role records
   (SPEC §4.10); decide whether they show as status entries or stay invisible.

### Acceptance criteria

- [ ] A reopened TUI shows the full past transcript for the bound run
- [ ] Live events after reopen dedupe against the fetched history, never
      double-render
- [ ] `chat`'s help text is true again: history **and** continuation
- [ ] A test crosses a process boundary in spirit: run, close, reopen, the
      transcript is there

### Watch for

Do not reuse the event stream with cursor 0 for this. Journal replay covers
durable events only — tool calls and live Kit deltas are live-only by design
(SPEC §4.8), so a cursor-0 stream would render a half transcript (responses
and questions, no user lines, no tool calls) and look like a fix while it is
not.

---

## T-024 — Remove the manifest: configuration is code

**RESOLVED.** `agent.yaml` is gone. A tree's data lives at fixed paths and
everything else is a Go option on `bonnie.New`. The scaffolded `main.go` is
one call.

**Priority** P1 · **Size** L · **Spec** [`docs/L2.md`](L2.md) §2, §4, §5

### Why

The scaffolded `main.go` had grown to 170 lines. It was authored, so BONNIE
could never rewrite it, which meant it carried a **copy** of the CLI's serve
wiring into user space — manifest loading, precedence, workspace rooting,
journal, mux, signal handling — and the two copies had already drifted (the
scaffold had no sandbox branch, no chat channels, no stream close on
shutdown). Every one of those pieces was `package main` in `cmd/bonnie`, so
no user could call them.

Underneath that was the real defect: **two places to configure one setting.**
The workspace path was written five times (`serve.go`, the scaffold's
`main.go`, `generate.go`, `dev.go`, `manifest.go`), and only one of the five
had been collapsed — after a defect where a renamed workspace was honoured by
one caller and missed by another (SPEC §4.9.1). The manifest's strictness was
the mitigation for having a second source of truth. Deleting the source beats
policing it.

eve reached the same place from the other direction: its config is
`defineAgent` in `agent/agent.ts`, the file is optional, and framework
defaults occupy the slot when it is absent. `docs/L2.md` §7 had recorded the
declarative manifest as BONNIE's own invention, not an eve adoption. It was
the wrong invention.

### What shipped

1. **The root `bonnie` package.** `Main` (flags, signals, exit) and `Run`
   (the serving path) with the wiring lifted out of `cmd/bonnie/serve.go`:
   the agent factory, the sandbox branch, `hostWorkspaceOptions`, the mux,
   `closeStreamsOnShutdown`, the drain. The CLI now calls the same code a
   user calls, so the two cannot drift again.
2. **The default layout as constants.** `DefaultInstructions`,
   `DefaultWorkspace`, `DefaultSkills`, `DefaultJournal`, `DefaultAddr`. The
   scaffold, codegen, the dev loop's watch set, and the runtime all read
   them.
3. **`Register` / `Registered` / `Tree`.** The generated `bonnie_gen.go`
   calls `Register` from `init` with the discovered tools and the embedded
   instructions, skills, and workspace. `main.go` never names a tool or an
   embed, so adding a tool changes only the generated file.
4. **Options for everything else**, including `WithAgentFactory` for a host
   that brings its own agent, and `WithSlack`/`WithDiscord`/`WithTelegram`
   which take their credentials from the environment.
5. **Deleted:** `agent/manifest.go` and its tests, `serve --agent`,
   `--config`, `--format`, `--title`, `resolveServe`'s precedence and
   banner, `refuseGoTree`, `embeddedManifest`, the `_manifest` embed slot.
   `yaml.v3` and `go-toml/v2` are indirect again; `go.sum` is unchanged.
6. **`internal/treetest`** replaced the workspace file the build tests used
   with a `replace` written into the temporary tree's own `go.mod`.

### Decisions, and why

- **The zero-Go path is gone, deliberately.** A host with no Go toolchain can
  no longer serve a tree from data. This was the manifest's one real
  capability, and it cost a second source of truth for every setting. eve
  requires Node on the desk and nothing on the host; BONNIE now requires Go
  on the desk and nothing on the host. The end of the arc — a static binary
  the user owns — is unchanged, and that is the end that matters.
- **`Register` rather than generated symbols `main.go` must call.** The old
  generated file defined `discoveredTools()` and `embeddedInstructions()`,
  which the authored `main.go` had to know about and call. Registration
  inverts it, which is what lets the minimum `main.go` be one line.
- **`New` returns an agent; `Serve` runs it.** A single `Main(opts...)` was
  tried first and named the caller rather than the thing. The constructor and
  the verb are separate because they do different jobs: `New` cannot fail, so
  it returns no error, and `Serve` owns the process — flags, signals, exit —
  while `Run(ctx)` is the same work for a host that owns its own. A `New` that
  blocked, or a `Serve` that built, would lie about one of the two.
- **`Registered` is a run-time read, and the doc says so.** Go initialises
  package-level variables before any `init`, so `var p = bonnie.Registered()`
  silently reads the empty tree. Found by the hermetic build test, which
  served an empty prompt. This is an API trap, so it is documented on the
  function rather than left for the next person to rediscover.
- **The banner reports the bound address, not the configured one.** `Run`
  binds before it prints, so `-addr :0` reports the real port. The old code
  printed the request back.
- **`WithAgentFactory` refuses the options it would shadow.** A host factory
  owns the agent, so `WithModel` beside it would be a model setting that
  silently does nothing — invariant 13, applied to the option surface.
- **`serve` kept, narrowed.** It is the flag-only generic host for trying the
  runtime with no tree. It cannot serve a tree, and says so, naming
  `bonnie dev` and `bonnie build`.

### Acceptance criteria

- [x] The scaffolded `main.go` is one call and wires no server by hand
      (`TestScaffoldedMainIsOneCall`)
- [x] The scaffold writes no manifest in any format
      (`TestScaffoldIsTheDefaultLayout`)
- [x] A fresh scaffold builds and the generated file compiles against this
      checkout (`TestScaffoldToolsModuleBuilds`, `TestGeneratedFileCompiles`)
- [x] The defaults are the scaffolded layout
      (`TestDefaultsAreTheScaffoldedLayout`)
- [x] The generated file carries no manifest and registers the tree
      (`TestCodegenEmbedsTheDefaultLayout`, `TestCodegenSkipsEmptySlots`)
- [x] The prompt falls back to the embedded copy, and a tree with neither is
      refused (`TestSystemPromptFallsBackToTheEmbeddedCopy`)
- [x] The security guards survived the move
      (`TestSandboxedAgentGetsNoHostTools`, `TestSandboxedWorkspaceBecomesASeed`)
- [x] An option that cannot apply is refused, naming it
      (`TestAgentFactoryRefusesConflictingOptions`, `TestDenyNetworkNeedsASandbox`)
- [x] The dev-restart and graduation tests still cross a process boundary,
      and now **run** rather than skip (`TestDevRestartCompletesParkedRun`,
      `TestBuildOutputServesEmbeddedInstructions`)
- [x] `go.sum` gains no entries; yaml and toml return to indirect
- [x] Verified live: `bonnie init`, `bonnie build`, run the binary from a
      directory with no tree — it answered as its own agent from the embedded
      instructions; `bonnie dev` hot-reloaded and the codegen-wired `echo`
      tool was called by the model

### Follow-up: what the documentation audit found

The doc pass after the two commits grepped every test name cited in the docs
against the tests that exist. Four citations had nothing behind them. Two
were prose debt in T-017's and T-020's historical acceptance criteria, now
annotated. Two were live invariants whose guard had been deleted because it
shared a file with a manifest test:

- **`TestWorkspaceIsNotWatched` was restored** and rewritten against
  `bonnie.DefaultWorkspace`, with `TestWorkspaceDirIsTheRuntimeWorkspace`
  added beside it. `docs/SPEC.md` §4.9.1 had gone two commits asserting a fix
  in prose with nothing testing it.
- **`TestChatChannelOptionsMount` replaces the deleted serve-path tests.**
  The secrets test that shipped with T-024 exercised the `require` helper
  directly, which is not the contract — the contract is that each
  `With<Platform>` option calls it before building a channel.

Writing the second test found a real defect: each option captured its
`Config` and let `fill` write the environment *into the captured copy*, so
`fill`'s empty-field guard made every later call reuse the first call's
credentials. A reused `Option`, or a second `Agent.Run`, would serve
credentials read at the first call rather than the environment as it stands
now. Each option now copies per call. Recorded in `docs/SPEC.md` §4.13 with
the general lesson: a guard test's file name is not its scope.

### Watch for

Do not reintroduce a config file for "just one setting". The next pressure
will be a sandbox or a channel that feels too verbose in `main.go`; the
answer is slot discovery (`sandbox/sandbox.go`, `channels/<name>/`) wired by
the same generator through the same `Register` seam — code, discovered by
path, exactly like `tools/`. See `docs/L2.md` §12.

An `Option` is an ordinary Go value a user can hold in a variable and pass to
two agents. Anything an option's closure captures must be treated as
read-only — copy before mutating, or the option becomes single-use in a way
no compiler catches.

---

## T-019 — Evals against a discovered agent

**Priority** P2 · **Size** L · **Blocks on** T-018 · **Reference**
[eve Evals](https://eve.dev/docs/evals/overview)

### Why

L4's second half. eve puts evals beside the agent tree; T-017/T-018 give
BONNIE a tree to point at. Without a defined agent, `bonnie eval` has no
subject.

### Do

Write the spec first, in this file or a new `docs/EVALS.md`, before any
code: what a case is, where cases live (beside the tree, per eve's
convention), how a run is scored, and what CI runs without a provider key.
This task is a placeholder until that spec exists.

### Acceptance criteria

- [ ] Spec written and reviewed before implementation
- [ ] `bonnie eval` runs cases against a discovered agent
- [ ] Hermetic mode (no provider key) skips, never fails

---

## T-025 — The journal is SQLite, without CGO

**RESOLVED.** The design, the decisions, and the measured cost are in
`docs/SPEC.md` §4.14. The text below is the record.

**Priority** P1 · **Size** M · **Spec** §4.14, and the rewrites of §4.2,
§4.7, §4.12 it forced

### Why

The journal was already on the local filesystem, and the JSONL layout paid
for that choice three times over. Each of the following had its own section
in `docs/SPEC.md`, its own mechanism, and its own tests:

- a tool-calling step could be torn by a short write (§4.2),
- two writers had to be refused with a per-run `flock`, because nothing
  stopped them reusing a sequence number (§4.7),
- a read of an unknown run created an in-memory handle that was never freed
  (§4.12).

All three are properties a local database has for free. T-014 weighed "a
journal backed by a database" against the lock file and chose the lock file
for cost; T-025 is that decision revisited now that a pure-Go SQLite driver
makes the cost about 3.8 MB of binary and no build-system change at all.

### What shipped

1. **`runtime/sqlitejournal.go`** — `SQLiteJournal` and
   `OpenSQLiteJournal(dir, opts...)` over `<root>/journal.db`. Tables `runs`,
   `records`, `meta`, all `STRICT`; `records` keyed by `(run_id, seq)`. WAL,
   `synchronous=FULL` by default, `busy_timeout`, `_txlock=immediate`, every
   one a DSN parameter so it reaches each pooled connection.
2. **`runtime/legacy_journal.go`** — `importJSONLRuns`, which carries every
   `runs/*.jsonl` into the database on open, keeps sequence numbers, renames
   the source rather than deleting it, and is idempotent.
3. **Deleted** — `runtime/filejournal.go` and its two test files, the lock
   file, `ownLocked`, the `runFile` handle map, `WithFsyncInterval`, and
   `FsyncInterval` (now `FsyncRelaxed`).
4. **Guards for the no-CGO claim** — a `depguard` rule denying
   `github.com/mattn/go-sqlite3`, and a `CGO_ENABLED=0 go build ./...` step
   in CI and in `task ci`.

### Decisions, and why

See `docs/SPEC.md` §4.14. The two that were easiest to get wrong:

- **A blob column takes invalid JSON; the JSONL encoder did not.** Writing a
  `Record.Payload` no decoder can read would journal a record that is durable
  and unreplayable at once. `insertRecord` checks `json.Valid`.
- **`FsyncInterval`'s promise could not be kept.** SQLite fsyncs at WAL
  checkpoints, not on a clock. Renaming it to `FsyncRelaxed` and deleting
  `WithFsyncInterval` was the only way to avoid invariant 13 in the public
  API.

### Acceptance criteria

- [x] `SQLiteJournal` passes the shared conformance suite
      (`runtime/journal_conformance_test.go`, factory `sqlite`)
- [x] A step is all-or-nothing, and a failed batch leaves no trace of the run
      (`TestSQLiteJournalAppendStepIsAllOrNothing`)
- [x] Two journals over one store write one run concurrently without losing
      a record or reusing a sequence number
      (`TestSQLiteJournalAdmitsConcurrentWriters`)
- [x] A crash mid-step still restores to a provider-valid conversation
      (`TestSQLiteJournalCrashResumeIsProviderValid`)
- [x] Pragmas are read back from a real connection, not trusted as fields
      (`TestSQLiteJournalUsesWAL`, `TestSQLiteJournalFsyncPolicy`)
- [x] A store from a newer BONNIE is refused by name
      (`TestSQLiteJournalRefusesANewerSchema`)
- [x] Legacy JSONL runs are imported losslessly, idempotently, and without
      deleting anything; a corrupt file fails the open with its path named
      (`runtime/legacy_journal_test.go`, five cases)
- [x] `CGO_ENABLED=0 go build ./...` passes, and is enforced in CI
- [x] `go test -race ./...` passes
- [x] The live suites pass against a real model
      (`TestLiveSuspendAndResume`, `TestLiveToolCallsSurviveOneProcess`,
      `opencode/kimi-k2.5`)
- [x] **Verified in tmux through `bonnie dev`'s TUI**: a tool-calling turn
      completes, a hot reload restarts the child mid-conversation, and one
      run stays dense across three processes — see `docs/SPEC.md` §4.14

### Watch for

- **A cold-cache toolchain run is slow, and it is not a defect.** The driver
  is transpiled C — about 1.6 million lines of generated Go in the graph — so
  a cold `go fix ./...` takes ~80 s and a cold `go build ./...` ~110 s
  (was ~98 s). Warm, both are seconds. A tool with a short timeout will
  report a failure with nothing behind it; check `gopls` and
  `golangci-lint`, which are the ones that inspect code.
- **Do not execute a `PRAGMA` after `sql.Open` and assume it stuck.** It
  applies to one connection in the pool. Every setting belongs in the DSN.
- **Do not put the journal on a network filesystem.** The old `flock` merely
  failed to protect there; SQLite can be corrupted. `SECURITY.md` says so.
- **The repair in `runtime/repair.go` still has work to do.** `SQLiteJournal`
  cannot produce a torn step, but imported runs and third-party journals
  can. Deleting the repair because "the journal is transactional now" would
  strand exactly the runs the import exists to save.

---

## Archive: shipped

Each of these is complete and covered by tests. The detail that mattered is
now in the code and in `docs/SPEC.md`; this is a pointer, not a spec.

| ID | Delivered | Where |
|---|---|---|
| T-001 | Live-model integration test behind the `integration` tag; skips without a key | `runtime/integration_test.go` |
| T-002 | Lossless replay — `Record.Payload` carries the typed `kit.LLMMessage` | `runtime/session.go`, `runtime/replay_fidelity_test.go` |
| T-003 | Torn-write repair drops an incomplete trailing tool-calling step | `runtime/repair.go`, `runtime/repair_test.go` |
| T-004 | `FileJournal` — JSONL, one file per run, fsync policy, `Persisted()` on the interface — **replaced by `SQLiteJournal` in T-025** | (deleted; see `runtime/sqlitejournal.go`) |
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

## T-027 — goreleaser publishes a commit list, not the release notes

**RESOLVED** in `v0.5.0`. `release.yml` slices the tag's section out of
`CHANGELOG.md` with `scripts/release-notes.sh` and passes it to
`goreleaser --release-notes`; a tag with no matching section fails the
workflow, naming the heading. Verified on the real `v0.5.0` tag: the
published body is the notes, not a commit list, and nobody edited it. The
section below is the task as it was written, kept for the record.

**Priority** P2 · **Size** S

### Why

`.goreleaser.yaml` has a `changelog:` block with `groups:`, which builds the
GitHub release body from **commit subjects**. It never reads `CHANGELOG.md`.
So every release publishes a list of commit hashes, and T-011's requirement —
the notes state the three claims and the limits — is met only by a human
editing the body afterwards. That was done by hand at `v0.1.0` and again at
`v0.4.0`.

A manual step that must happen after every tag will eventually be skipped,
and the failure is silent: the release looks published and simply does not
say what BONNIE cannot do. The limits are the part a reader most needs.

### Do

1. Point the release body at the changelog instead of the commit log. Either
   `release.notes` with the extracted section, or a release job step that
   slices `CHANGELOG.md` between the version heading and the next `## [` and
   passes it to `--release-notes`.
2. Keep the commit grouping as a secondary section if it is wanted, below the
   notes rather than instead of them.
3. Make the release fail, not pass quietly, when the changelog has no section
   matching the tag being built.

### Acceptance criteria

- [x] A tag publishes a body containing the three claims and the limits with
      no human edit — `release.yml` runs `scripts/release-notes.sh` and passes
      the section to `goreleaser --release-notes`
- [x] A tag whose version has no `CHANGELOG.md` section fails the release
      workflow with a message naming the missing heading
- [x] Verified on a real tag, not only in `--snapshot` — `v0.5.0`
      (release run `34968699731`) published a 170-line body carrying the
      claims and the limits, with no human edit and no commit list

The extractor is pinned by `scripts/release_notes_test.go`, which `go test
./...` reaches: it covers the stop-at-next-release boundary, the optional
leading `v`, exact-not-prefix matching (`0.5` must not match `0.5.0`, `0.1.0`
must not match `0.10.0`), the named failure, and a check that the repository's
own notes for the version being cut state the claims and the limits.

### Watch for

The extraction must match the heading form actually in use, `## [0.4.0] —
DATE`, and must stop at the next `## [`. Nested `###` group headings are part
of the section and must be kept.

---

## T-026 — microsandbox conformance flake

**SHIPPED 2026-09-15.** Two defects, and the first one hid the second.

**The harness demanded a concurrency the product never produces.**
`backends()` built a fresh provider per test case, so 18 parallel cases meant
18 independent mutexes and ~20 concurrent `msb create` calls, which lock
msb's own SQLite store (`SQLITE_BUSY`). A real host shares one provider
(`cmd/bonnie/serve.go:110`) and `Open` holds `p.mu` across `ensureRunning`,
so BONNIE never issues two creates at once. One shared provider per backend
took the pass rate from ~0/10 to 8/10.

**Then the original diagnosis proved right.** The remaining ~20% was the
reported `sandbox already exists`. Instrumenting `exists()` found why, and it
is not a mid-creation window: `msb ps --all --format json` intermittently
returns an empty list with exit 0 while sandboxes are running — 3 of 24 calls
in the run that caught it. `ensureRunning` now adopts an "already exists"
refusal instead of failing, because no pre-flight check can be atomic against
that. `adopt` runs `checkPolicy` on every path that takes over an existing
sandbox, so `ErrPolicyMismatch` still fires. **20/20 consecutive runs pass.**

**The truncation was the expensive part.** `firstLine` kept msb's headline and
dropped the indented `→` lines carrying the cause, which is how this was
misdiagnosed in `docs/SPEC.md` §4.11 for a day. `msbError` keeps them.

### Acceptance criteria

- [x] 20 consecutive `go test -count=1 ./sandbox` runs pass with `msb`
      installed, on a loaded machine
- [x] A failing `msb` command surfaces its `→` cause
      (`TestMsbErrorKeepsTheCause`)
- [x] The adopt path matches msb's real wording and does not swallow a
      genuine failure (`TestMsbAlreadyExistsMatchesMsbWording`)
- [x] A reattach under a different network policy still returns
      `ErrPolicyMismatch` (`TestMicrosandboxRefusesReattachPolicyMismatch`)
- [x] §4.11's correction notes record the fix
- [ ] Report the empty-`ps --all` race upstream to `microsandbox`

### What this cost, and the lesson

The first diagnosis was wrong, and so was the correction to it. A one-line
error hid the cause; a harness that did not match the product invented a
second failure on top. Neither was visible to CI, which has no `msb`.

Do not serialise the conformance suite to make this go away. The suite is now
*less* parallel than before only in the sense that it shares a provider — the
cases still run in parallel, which is what a multi-run host does.
---

## T-028 — Normalised inbound turn with a per-turn context slot

**Priority** P1 · **Size** M · **Blocks** T-029 (shipped) · **Found by** comparing
`channel/chat` with eve's channel contract (2026-09-14)

**SHIPPED.** Two notes against the plan: step 4 records the origin as
extension data (`bonnie.origin`) *and* tells the model through the same
context hook ("This conversation is on channel slack (thread)"), because a
record nobody reads helps no one; and `Runner.Resume` carries no context — an
answer to a question is the answer.

### Why

eve turns every platform event into one shape before the agent sees it: a
`message` (the user-visible text with the invocation token removed), a
`context` (per-turn facts for the model that are **not** conversation
history), an `auth` principal, and an optional `title`. BONNIE does the
first and third parts, but only inside each adapter: `forUs` in
`channel/slack/slack.go:266` and `channel/telegram/telegram.go:225`,
`stripMention`, `stripCommand`, `commandText`. The shared layer takes
`(address, text, SendOptions)` and nothing more (`channel/chat/chat.go:422`).

Three things are missing:

1. **No normalised turn type.** There is no struct for "a platform event
   turned into a turn", so each adapter parses on its own and there is no
   common shape to test against.
2. **No per-turn context.** `runtime.Input` is `{Text, Files}`
   (`runtime/runner.go:106`). A GitHub comment is text, but the PR diff, the
   event type, the actor, and "was the bot mentioned" are context. Today they
   would go into `Text`, and the journal would record them as if the user
   typed them. That is a fidelity problem, not a cosmetic one: replay must
   stay lossless, and a replayed turn must not carry a diff as user prose.
3. **No conversation kind and no title from chat.** Nothing tells the agent
   whether the address is a DM, a thread, an issue, or a review thread.
   `SendOptions.Title` is set only by the HTTP channel
   (`channel/http/http.go:224`); the chat adapters never name a run.

`docs/CHANNELS.md` said GitHub was "mechanical now, not structural". It is
not: the GitHub mapping needs this slot first. The sentence is corrected in
the same commit that opens this task.

### Do

1. Add a `Context []string` field to `runtime.Input`. The runner injects it
   into the model call for that turn only, through the seam that already
   exists (`Kit.OnContextPrepare`, `docs/SPEC.md` §3). It is journalled as
   its own record kind so replay can reproduce it, and it is never merged
   into the user message.
2. Add a `chat.Turn` type: `Address`, `Text`, `Context`, `Auth`, `Title`,
   `Kind` (`dm`, `thread`, `channel`, `issue`, `pull_request`,
   `review_thread`), and `TurnPolicy`. Change `chat.Route` and
   `chat.Dispatch` to take a `Turn`. Keep `channel.SendOptions` as the wire
   form for `SessionRef.Send`; add `Context` to it.
3. Make the three adapters build a `Turn`. Each sets `Kind` and a `Title`
   (the first line of the first message, capped) on the run's first turn.
4. Expose `Kind` and the channel name to tools and instructions through the
   run's extension data, so an instruction can say "you are in a GitHub
   review thread" without the adapter pasting it into the prompt.
5. Add `context` to the HTTP channel's `StartRequest` and `SendRequest`, so
   an API client has the same slot.

### Acceptance criteria

- [x] `runtime.Input.Context` reaches the model on the turn it was sent with
      and on no later turn — a test with `fakeAgent` asserts both
- [x] A replayed run reproduces the context records without changing the
      user messages; `runtime/replay_fidelity_test.go` gains a case
- [x] `chat.Turn` is the only argument `Dispatch` and `Route` take, and the
      `channeltest` suite drives it
- [x] Every chat adapter sets `Kind` and a `Title`; `bonnie runs list` shows
      the title for a Slack-started run
- [x] `docs/CHANNELS.md` describes the normalised turn and lists the kinds
- [x] `docs/SPEC.md` §3 records which seam carries the context and how it is
      journalled

### Watch for

Do not put context into `Record.Text` of the user message and strip it on
replay. That is exactly the "rebuild from `Text`" shortcut the fidelity guard
exists to refuse. Context is its own record, or it is not durable.

Keep the runtime blind to platforms. `Kind` values are strings the channel
layer defines; `runtime/` must not import `channel/` (invariant 6).

---

## T-029 — GitHub channel: issues, PRs, and review threads become conversations

**Priority** P2 · **Size** L · **Blocked by** T-028 (shipped) · **Prior art** eve's
[GitHub channel](https://eve.dev/docs/channels/github)

**SHIPPED.** Three notes against the plan: a PR's timeline conversation is
addressed `pulls/<n>` but answered through the issues API, where GitHub
actually stores PR timeline comments; the review-thread thread ID is the
root comment's ID, and the reply goes through the replies endpoint; and the
hooks' turns get the obvious address when the hook leaves it empty, so an
opt-in `issues.opened` needs no address bookkeeping.

### Why

A GitHub App is the surface where a maintainer agent is most useful: answer a
mention in an issue, summarise a new PR, triage a failed check. It is also
the channel that proves the normalisation layer, because a GitHub event is
far from a chat message: the conversation is an issue or a PR, the text is a
comment, and most of what the model needs (the diff, the event, the actor)
is context, not history.

### Do

1. Add `channel/github`. Webhook `POST /github/events`. Verify
   `X-Hub-Signature-256` (HMAC-SHA256 over the raw body with the webhook
   secret); refuse a request without one. Dedup by `X-GitHub-Delivery`.
2. Authenticate as a GitHub App: `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`,
   `GITHUB_WEBHOOK_SECRET`, all from the environment, all required. Mint an
   installation token per event; never write it to the journal.
3. Address format: `github/<owner>/<repo>/issues/<n>` for an issue or a PR
   timeline, `github/<owner>/<repo>/pulls/<n>/reviews/<thread_id>` for a
   review thread. A PR and its review threads are separate conversations, as
   in eve.
4. Dispatch rules, each a `chat.Turn` from T-028:
   - `issue_comment` and `pull_request_review_comment` that contain
     `@<botName>` start or continue a turn. Strip the token from `Text`;
     record `is_mentioned`, `sender`, `event`, `action` in `Context`.
   - A comment on an address this channel already bound continues it with no
     mention, the same rule as a Slack thread.
   - `issues.opened`, `pull_request.opened`, `check_suite.completed` are
     opt-in through `Config` hooks that return a `Turn` or nil.
   - Every event from a bot sender is dropped.
5. PR context: when the address is a PR, put the PR title, base and head,
   and the changed-file patch into `Context`, with an exclusion list for
   generated files and a size cap.
6. Delivery: a comment on the timeline or in the review thread, split with
   `chat.SplitText` at GitHub's limit. A parked run posts its prompt as a
   comment; the next comment on the address answers it (`chat.Route`).
   Add an `eyes` reaction to the triggering comment when a turn starts.
7. `bonnie.WithGitHub(github.Config{})` mounts it, with the same
   missing-secret startup error as the other adapters.

### Acceptance criteria

- [x] A mention in an issue comment starts a run bound to the issue address;
      a second comment continues the same run
- [x] A review-thread comment and a PR-timeline comment on the same PR are
      two runs
- [x] The PR diff arrives as `Context`, not as user text; the journal shows
      one user record with the comment only
- [x] A reply to a parked run resumes it
- [x] An unsigned or badly signed delivery is refused with 401; a replayed
      delivery ID is dropped
- [x] The installation token never appears in a journal record or a log line
      (a test greps the journal after a full turn)
- [x] `docs/CHANNELS.md` gains a GitHub section with setup and env vars

### Watch for

GitHub retries a delivery when the endpoint is slow. Acknowledge at once and
run the turn through `chat.Dispatch`, the same as the other adapters.

GitHub does not autocomplete or link the App's `@name`; it is a text token.
Match it case-insensitively at a word boundary, and do not require it to be
first in the comment.

---

## T-030 — Reserved HTTP namespace and a health route

**Priority** P1 · **Size** S · **Found by** comparing `channel/http` routes
with eve's `/eve/v1/*` surface (2026-09-14)

**SHIPPED.** One note: the prefix constant lives in `channel`
(`channel.APIPrefix`, `channel.ReservedPathPrefix`), not the root package,
because `channel/http` cannot import the root. The root package's `mount`
reads the same constant.

### Why

eve mounts its framework API under one versioned prefix, `/eve/v1/*`, and
forbids custom channels from using it. BONNIE mounts `POST /runs`,
`GET /runs/{id}`, `GET /addresses/{address}` and friends at the root
(`channel/http/http.go:86-94`), next to `/slack/events`,
`/discord/interactions`, and `/telegram`. `run.go:442` registers every
channel's routes on one `http.ServeMux` with no ownership check. A user
channel that mounts `/runs/...` panics the mux at startup at best, and
shadows the framework at worst.

There is no version on the wire. The TUI (`bonnie chat`) and any external
client depend on the exact paths, so a change later is a breaking change for
all of them. The time to add the prefix is before more clients exist.

There is also no health route. A deployment probe today has to `GET` a run
that does not exist and treat 404 as alive.

### Do

1. Mount the HTTP channel under `/bonnie/v1`. Keep the route shape:
   `/bonnie/v1/runs`, `/bonnie/v1/runs/{id}`, `/bonnie/v1/runs/{id}/stream`,
   `/bonnie/v1/addresses/{address}`. Put the prefix in one constant in the
   root `bonnie` package so the CLI, the TUI, and the docs read the same
   value (invariant 14: one setting, one place).
2. Refuse any other channel that mounts a path under `/bonnie/`. The refusal
   is a startup error that names the channel and the path, not a mux panic.
3. Add `GET /bonnie/v1/health` returning `{"ok":true,"status":"ready"}`
   with no auth. Add `GET /bonnie/v1/info` returning the agent name, the
   BONNIE version, and the mounted channel names.
4. Update `bonnie chat`, `cmd/bonnie/serve.go`'s help text, the examples,
   `docs/CHANNELS.md`, and `docs/SPEC.md` §4 wherever a path is written.
5. Note the break in `CHANGELOG.md` under a minor version bump.

### Acceptance criteria

- [x] Every HTTP-channel route answers under `/bonnie/v1/` and none at the
      root
- [x] A test channel with a `/bonnie/x` route fails `Serve` with an error
      naming it
- [x] `GET /bonnie/v1/health` returns 200 before any run exists
- [x] `bonnie chat` works against a server built from the same commit
- [x] No file in the repo still says `/runs` without the prefix
      (`grep -rn '"/runs' --include='*.go' --include='*.md'` is empty)

### Watch for

The chat adapters' webhook paths (`/slack/events`, `/telegram`) stay where
they are. They are configured in the platform's console and are not part of
the framework API.

---

## T-031 — Idempotent start and stable error codes on the HTTP channel

**Priority** P2 · **Size** S · **Prior art** eve's `operationId` and
`session_not_active`

**SHIPPED.** One note against the plan: the TUI already switched on the HTTP
status, which is stable, so no change there; `code` is for clients that read
bodies.

### Why

`POST /bonnie/v1/runs` (soon `/bonnie/v1/runs`) with no `address` creates a run on
every call. A client that times out and retries gets two runs and two model
turns. eve accepts an `operationId`: the same ID from the same authenticated
principal returns the existing session instead of dispatching again. BONNIE's
`address` field gives create-once semantics only when the client thinks of
the address as an idempotency key, and nothing documents that.

`ErrorResponse` carries a free-text `error` only (`channel/http/http.go:184`).
A client cannot tell `ErrRunOwnedElsewhere` from any other 409 without
parsing prose. eve returns a stable `code` next to the message.

### Do

1. Add `operation_id` to `StartRequest`. When set with an `Auth` principal,
   bind `<principal.Authenticator>/<principal.ID>/<operation_id>` in the
   address map and resolve through it. An anonymous request with an
   `operation_id` is a 400: an idempotency key with no owner is a way to
   read someone else's run.
2. Add `code` to `ErrorResponse`. One code per sentinel `writeError` already
   maps: `run_not_found`, `run_owned_elsewhere`, `unknown_turn_policy`,
   `run_not_waiting`, `bad_request`, `internal`. Document the list in
   `docs/CHANNELS.md`.
3. Make the TUI and the `channel/http` client helpers switch on `code`, not
   on the message.

### Acceptance criteria

- [x] Two `POST` starts with the same `operation_id` and principal return
      the same run ID and produce one model turn (a `fakeAgent` call count)
- [x] The same `operation_id` under a different principal is a different run
- [x] Every non-2xx body has a non-empty `code`, asserted by a table test
      over `writeError`
- [x] `docs/CHANNELS.md` lists the codes

---

## T-032 — Framework-owned address namespace and session controls

**Priority** P2 · **Size** M · **Blocks** T-033

**SHIPPED.** Two notes against the plan: retirement is a run state
(`RunRetired`) rather than an extension-data note with a terminal flag,
because every journal read already goes through the state; and the reset
note is delivered by the adapter through `Dispatch`, not journalled — the
retirement itself is the durable fact.

### Why

eve prefixes every continuation token with the channel's name before it
reaches the runtime, so two channels cannot bind the same address by
accident. BONNIE's adapters each choose a prefix string by hand
(`"slack/"`, `"discord/"`, `"telegram/"`) and nothing checks it. A custom
channel that writes `slack/C1/dm` steals a Slack conversation. The `Attach`
path already refuses reserved run IDs for the same reason
(`channel/chat/chat.go:307`); addresses need the same guard.

eve also exposes three controls BONNIE lacks: `reset` (retire the run and
free its address so the next message starts fresh — eve's `/new`), `clear`
(drop model history but keep the run ID, tools, and workspace), and `compact`
(summarise on demand). `AddressMap.Bind` can re-key, but no adapter or route
exposes "start over in this thread", and Kit's compaction runs only on its
own threshold (`docs/SPEC.md` §5, mapping row for `compaction.thresholdPercent`).

### Do

1. Make `chat.Core` carry the channel name and prefix every address with
   `<name>/` itself. Adapters pass the bare platform key. An address that
   already starts with another mounted channel's prefix is refused.
2. Add `Reset(ctx)` to `channel.SessionRef`: mark the run terminal with a
   `RecordExtensionData` note, unbind the address, and return. A fixed
   `Attach` ref keeps pointing at the retired run; a `From` ref creates a
   new one on the next `Send`.
3. Add `Clear(ctx)` and `Compact(ctx)`. `Clear` appends a marker record
   that `Restore` treats as the new start of history, so the journal stays
   append-only. `Compact` asks Kit to summarise through the public API; if
   Kit exposes no on-demand entry point, open an upstream issue and mark the
   gap `// TODO(kit):` per `AGENTS.md`.
4. Mount the three as `POST /bonnie/v1/runs/{id}/{reset|clear|compact}`.
   Give the chat adapters a `/new` convention that calls `Reset` on the
   thread's address.
5. Update the `channeltest` conformance suite for the new methods.

### Acceptance criteria

- [x] Two channels mounted together cannot resolve the same bare key to one
      run; a test proves the prefix is applied by `Core`, not by the adapter
- [x] After `Reset`, a `Send` on the same address creates a new run and the
      old run's state is terminal in `bonnie runs list`
- [x] After `Clear`, `Restore` returns a provider-valid conversation with no
      messages before the marker, and the run ID is unchanged
- [x] `/new` in a Slack thread starts a fresh run in the same thread
- [x] `channeltest` covers `Reset`, `Clear`, and `Compact`

### Watch for

A delayed duplicate webhook that carries `/new` can retire a newer run.
Dedup before `Reset`, and have the adapters pass the platform's event ID
through so the dedup is per event, not per text.

Do not delete history on `Clear`. The journal is append-only (invariant in
`AGENTS.md`); `Restore` skips, it does not erase.

---

## T-033 — Cross-channel hand-off and proactive sessions

**Priority** P3 · **Size** M · **Blocked by** T-032 (shipped)

**SHIPPED.** One note against the plan: the destination's `Receive` is a
`channel.Receiver` on the adapter, not a `To(name, target) SessionRef`,
because creating the surface (open a Slack thread, mint an installation
token) is adapter logic a generic SessionRef cannot carry. The registry
shape — `To(name)` — is the same.

**Reviewed after the fact, six defects fixed.** The first cut ticked two
criteria it did not meet. What the review found, and where the fix is
pinned:

| Defect | Fix | Test |
|---|---|---|
| Slack reports an application failure as HTTP 200 + `ok:false`; `postMessageTS` read the status line only, so a refused hand-off bound a broken address, ran a turn, and returned success | decode `ok`, and refuse a root with no timestamp | `channel/slack/handoff_test.go` |
| `Proactive` re-keyed the address unconditionally, orphaning a live conversation on every stable-address channel | `Resolve`, not `Bind`: create on first sight, continue after | `TestProactiveContinuesAnExistingConversation` |
| the principal was journalled twice — `Ref.Send` already notes it | drop the second note | `TestProactiveRecordsTheInitiatingPrincipalOnce` |
| a PR hand-off bound the *issue* address, so the first reply started a second run | `Target.PullRequest` picks the address an inbound comment resolves to | `TestReceiveBindsTheAddressAnInboundCommentResolvesTo` |
| `GITHUB_INSTALLATION_ID` was named in an error and in the docs but never read | `fillInt` in `WithGitHub`; a value that does not parse is a startup error | `TestGitHubInstallationIDComesFromTheEnvironment` |
| (found while fixing the fourth) a mention-free follow-up on a PR was dropped: boundness was asked of the issue address | ask the address the comment will use | `TestMentionFreeFollowUpContinuesAPullRequest` |

Smaller: `http.Channel.Handler` passed a nil `Outbound` where its own doc
said empty; Telegram recorded every hand-off as kind `channel` and
accepted a target that is not a chat ID, which would have delivered to
chat 0 in silence.

### Why

eve lets a route on one channel start or continue a conversation on another
(`ctx.to(slack, target).send(...)`): an incident webhook opens an
investigation thread in Slack. It also lets a schedule or another channel
start a session with no inbound message (`receive`). BONNIE has neither.
`docs/CHANNELS.md` lists proactive sessions as not implemented and says run
creation is the HTTP channel's job. That works for a script, but a bot that
posts a Monday digest and then answers replies in its own thread needs the
chat adapter to own both halves.

### Do

1. Add `channel.Outbound`: `To(channel string, target any) SessionRef`. The
   target is the destination channel's own type (a Slack channel ID, a
   GitHub issue), and the destination channel decides the address and the
   initial delivery (open a thread, post a comment) before the first turn
   runs.
2. Give each chat adapter a `Receive(ctx, target, turn)` that creates the
   platform surface, binds the address, and dispatches the turn.
3. Hand the `Outbound` to route handlers next to `Inbound`, and to the
   scheduler when one exists.
4. Carry the initiating `Principal` through so the destination run records
   who started it.

### Acceptance criteria

- [x] A test channel's route starts a run on a fake Slack adapter, and the
      reply lands in the thread the adapter opened — `handoff_test.go`
- [x] A run started through `Receive` is bound to its address, so a later
      platform reply continues it — `TestACommentContinuesTheRunAHandOffStarted`
      (GitHub, the case where the address had to be got right),
      `TestProactiveBindsANewAddressBeforeTheTurn`
- [x] `Principal` on the destination run is the initiator's —
      `TestProactiveRecordsTheInitiatingPrincipalOnce`
- [x] `docs/CHANNELS.md` removes proactive sessions from "not implemented"

### Watch for

`To(...).Send` is agent input, not a notification API. A caller that only
wants to post text calls the platform; a notification that must survive a
crash goes through an outbox, which is a separate task if anyone needs it.

A hand-off's address must be the one an inbound event on that surface
resolves to. Every defect above except the Slack one is a version of this:
an address that is nearly right splits one conversation into two runs that
both deliver into one place, and nothing fails loudly when it happens.

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

T-011 closed the release: `v0.1.0` was tagged and published, and the
artifact check passed. T-009 resolved itself: Kit `v0.106.0`
answered the asks, and BONNIE adopted the seams.
