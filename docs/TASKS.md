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
| T-021 | Built-in terminal TUI: `bonnie dev` opens a scrollback chat; `bonnie chat` connects to an HTTP channel; cursor reconnect and cancel; assistant messages render as markdown through herald-md (Kit's typography patterns) | `cmd/bonnie/tui/`, `cmd/bonnie/chat.go`, `cmd/bonnie/dev.go` |
| T-018 | L2 codegen: tool discovery (`agent/gen`), `bonnie dev` (fsnotify loop), `bonnie build` (go:embed + static binary), `--dry-run`; import allowlist; duplicate-name refusal; idempotent codegen | `agent/generate.go`, `agent/generate_test.go`, `cmd/bonnie/build.go`, `cmd/bonnie/dev.go`, `cmd/bonnie/l2_test.go` |
| T-020 | Chat channels: Slack, Discord, Telegram adapters with verified webhooks, dispatch (a reply to a parked run resumes it), threaded delivery; `channel/chat` shared plumbing; the manifest's `channels:` keys with env-only credentials | `channel/slack/`, `channel/discord/`, `channel/telegram/`, `channel/chat/`, `cmd/bonnie/serve.go`, `docs/CHANNELS.md` |
| T-017 | L2 core: the strict manifest loader (`agent/`), `bonnie init` (always a Go module; `--tools` adds a sample tool — the zero-Go fork was replaced in `v0.2.0`), `serve --agent` with flag-over-manifest precedence and source-annotated banner, the go-tree refusal, workspace seeding | `agent/manifest.go`, `agent/scaffold.go`, `cmd/bonnie/init.go`, `cmd/bonnie/serve.go`, `sandbox/seed.go` |
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

## T-011 — Tag and release `v0.1.0`

**RESOLVED.** `v0.1.0` was tagged at `15e1727` and published on 2026-09-12;
`release.yml` completed successfully. Verified post-publish, not assumed:
a downloaded `linux_amd64` artifact prints `bonnie 0.1.0` (the injected
version, not `dev`), and the GitHub release notes lead with the three claims
and the limits — the auto-generated notes had only the commit list, so the
notes were edited to the required form.

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
- The sandbox conformance suite **skips** Docker and microsandbox on a bare
  runner. CI green does not mean those adapters were exercised. The
  microsandbox defects fixed in T-013 were invisible to CI for exactly this
  reason.

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
      (`TestServeMountsChatChannels`, `TestChatChannelNeedsItsSecrets`)
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
  `go mod tidy` fills it. The guard test builds the fresh scaffold through a
  synthetic `go.work` over the local checkouts (`TestScaffoldToolsModuleBuilds`)
  to stay hermetic, but a standalone `go build` off the proxy is the verified
  public path.
- **`skills:` is reserved, not wired.** Seeding skill files into a sandbox
  nothing reads would be a dead key — a control nothing applied. The key
  is refused with the same message class as `mcp`, until a skill-loading
  story exists. `workspace:` IS wired: `sandbox.Seeded` wraps any provider
  and mirrors the seed directory over the `Sandbox` interface, skip-if-
  exists, so a resumed run never has its edits reverted
  (`sandbox/seed_test.go`). The wrapper forwards `Networked`,
  `ExistenceChecker`, and `RunDeleter`, so wrapping never widens what a
  caller can request.
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

- **The hermetic build/dev tests skip without a kit checkout.** They build a
  real child binary through a temp `go.work`, so like
  `TestScaffoldToolsModuleBuilds` they need the upstream `kit` checkout beside
  the repo. CI green does not mean they ran unless that checkout exists.
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
  tree; testing a local change through `bonnie dev` needs the hermetic
  `go.work` beside the tree that `agent/generate_test.go` already uses.
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

T-011 closed the release: `v0.1.0` was tagged and published, and the
artifact check passed. T-009 resolved itself: Kit `v0.106.0`
answered the asks, and BONNIE adopted the seams.
