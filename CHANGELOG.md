# Changelog

All notable changes to BONNIE are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Nothing yet. The next release starts here.

## [0.4.0] — 2026-09-14

The readable-answer increment: assistant messages in the TUI render as
markdown — headings, lists, tables, and code blocks in the same palette as
the rest of the surface — and assistant prose wraps at the terminal width
instead of being cut.

### Added

- Assistant messages render as markdown in `bonnie dev` and `bonnie chat`
  (a T-021 extension): headings, bold, lists, tables, and code blocks render
  through herald-md — the same typography stack upstream Kit's TUI uses — in
  the TUI's existing palette. Streaming text renders live, so the answer
  arrives already shaped; user messages, tool lines, and reasoning stay as
  they were, because typed text is not markdown.

### Changed

- **The agent's root is now the workspace.** A tool call's relative path
  resolves inside the tree's `workspace/` (or the manifest's `workspace:`)
  instead of wherever the server was started from. `serve --agent .` used to
  drop a model's files on the tree itself — beside `agent.yaml`,
  `instructions.md`, and `.bonnie/`, the journal a run's durability depends
  on. A sandboxed run already rooted everything at `/workspace`, so the two
  modes now agree. The banner names the resolved workspace. Serving without
  a tree is unchanged: the process's own directory stays the root. Note that
  this is a root, not a jail — an absolute path still escapes, which is what
  the sandbox is for (`docs/SPEC.md` §4.9.1).

### Fixed

- `bonnie dev` watched the workspace, so the agent restarted itself for doing
  its job: a model writing a file — the ordinary case now that the workspace
  is the agent's root — tripped a rebuild that SIGTERMed the child still
  serving the turn. The workspace is the loop's output, not its input, and is
  no longer watched. Edits to the manifest, instructions, tools, and go.mod
  still hot-reload; verified live in both directions.
- The manifest's `workspace:` key seeded nothing. `sandbox.Seeded` was
  written and tested but never called, so the key was accepted and ignored —
  what invariant 13 exists to forbid. A sandboxed run now really does receive
  the seed files at `/workspace`.
- Assistant lines longer than 100 columns were silently truncated by the old
  `MaxWidth` style: every cell past the limit was lost, on every message.
  Assistant prose now wraps at the terminal width and keeps everything —
  verified live against a real model, including a wrapped 300-character
  paragraph and the hot-reload reconnect path.
- The TUI's cursor landed below the footer once the transcript grew taller
  than the terminal: the view reported a frame-relative row, while inline
  mode moves the terminal cursor to that exact screen position, so the
  terminal clamped the move to its bottom row. The view now subtracts the
  rows the screen has scrolled past. Pinned by a test and verified in tmux
  at three window heights.

## [0.3.0] — 2026-09-13

The terminal increment: `bonnie dev` and `bonnie chat` open a scrollback TUI
that streams one durable conversation — tool calls render as a spinner that
becomes a check mark with the result on the next line. Two event-stream
defects that swallowed tool calls are fixed.

### Added

- Built-in terminal TUI (T-021): `bonnie dev` opens the TUI against its
  serving child, `bonnie chat` connects the same TUI to any running HTTP
  channel. One durable conversation per address, streamed from the
  journal-cursor; a `dev` hot reload reconnects without losing the
  conversation, and `ctrl+w` cancels the running turn.
- `GET /addresses/{address}` on the HTTP channel: a read-only lookup that
  returns the run bound to an address and its journal cursor, and creates
  nothing on a miss. The TUI uses it to open the stream before the first
  message of a session.
- A tool call renders as one compact entry: a spinner while the tool works, a
  check mark when it completes, and an indented arrow with a one-line,
  Unicode-safe truncated result.

### Fixed

- Live events that share one journal anchor were dropped past the first one
  (`docs/SPEC.md` §4.8.1). A tool-call start, parsed call, execution, and
  result can all land on the same anchor, so the TUI lost tool calls the
  event bus held.
- A TUI that reopened an existing address learned its run ID only after the
  first turn, so tool and reasoning events of that turn never reached it.
  The startup lookup fixes the stream; guard tests cover both.

## [0.2.0] — 2026-09-13

The L2 increment: an agent is a tree, not a hand-wired library. `bonnie init`
scaffolds one; `bonnie serve --agent` serves it with no compile; `bonnie dev`
hot-reloads it; `bonnie build` graduates it into one static binary. The chat
channels move durable runs into Slack, Discord, and Telegram, and every
scaffold is now a Go module that builds against the public modules.

### Added

- Chat channels: Slack, Discord, and Telegram adapters (`channel/slack`,
  `channel/discord`, `channel/telegram`), zero new dependencies. Each
  mounts one verified webhook — Slack's v0 HMAC with a replay window,
  Discord's Ed25519, Telegram's shared secret — acknowledges inside the
  platform's deadline, runs the turn in a goroutine the handler does not
  outlive, and delivers the reply to the thread. A run that parks for human
  input posts its question into the chat, and the next message there
  resumes it. Each adapter joins the `channeltest` conformance suite.
- `channel/chat`, the shared plumbing the adapters are built on: the
  journalled address map (moved from `channel/http`), per-run turn locks,
  the `SessionRef` implementation, the dispatch rule (a reply to a parked
  run resumes it — a chat surface cannot say "this is a resume"), and the
  goroutine delivery model.
- The manifest's `channels:` keys (`channels.slack`, `channels.discord`,
  `channels.telegram`) enable the chat adapters for `bonnie serve --agent`.
  Configuration lives in the manifest; credentials live in the environment
  (`SLACK_BOT_TOKEN`, `SLACK_SIGNING_SECRET`, `DISCORD_BOT_TOKEN`,
  `DISCORD_PUBLIC_KEY`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_WEBHOOK_SECRET`),
  and a missing one is a startup error that names the variable. Secrets in
  the manifest are refused by construction: the keys do not exist.
- New invariant 15 in `docs/SPEC.md` §8: a chat channel verifies its caller
  or refuses to serve. See `docs/CHANNELS.md` for the per-platform setup,
  the dispatch and steering rules, and what is deliberately not
  implemented.

### Changed

- L2 discovery, first increment (`T-017`): the agent tree is discovered
  from a manifest. `bonnie init` scaffolds a tree — `agent.yaml`,
  `instructions.md`, and the `skills/` and `workspace/` seed directories —
  as a Go module with a `main.go` that defines the default agent;
  `--tools` adds a sample tool. `bonnie serve --agent DIR` serves a
  discovered tree with no build and no toolchain: instructions from disk,
  model, sandbox, and channel bindings from the manifest. Flags override
  the manifest, and the startup banner names the source that won. Files under
  the manifest's `workspace:` are mirrored into every run's sandbox on every
  backend, and a file the model already wrote is never overwritten.
- Always a Go module (`Option A`, this release): `bonnie init` no longer
  offers a zero-Go fork. Every scaffold builds out of the box — the authored
  `main.go` reads the manifest for the model, address, and instructions and
  wires the default agent, so `go run .`, `bonnie dev`, and `bonnie build`
  converge on it. `--tools` only adds a sample tool directory; the modules
  are public, so `go mod tidy` resolves them from the proxy.
- The manifest loader (`agent/`) is strict on one code path for `agent.yaml`,
  `agent.toml`, and `agent.json`: an unknown key, an unknown `apiVersion`,
  or two manifests in one root is an error naming what is wrong. `mcp` and
  `skills` are reserved and refused until their loading stories exist.
  `serve --agent` refuses a tree that carries Go tools and names
  `bonnie build` (the codegen increment, T-018) — a partial run is never
  the answer.

### Changed

- The `bonnie` CLI is built on cobra, and its help and errors render with
  [fang](https://github.com/charmbracelet/fang). Commands, flags, and exit
  codes are unchanged; help gains styling, shell completions, and a man
  page (`bonnie man`). `--version` and `bonnie version` print the same
  version. A bare `bonnie` now prints help instead of exiting 2.

### Removed

- The depguard bans on the Charm stack (bubbletea, bubbles, huh, lipgloss,
  fang) and the "headless" invariant behind them. They were a design
  starting position, not a boundary. BONNIE ships no TUI today and none is
  planned, but importing the stack is no longer a lint failure. The
  public-Kit-SDK boundary (`kit/internal`, `charm.land/fantasy`) is
  unchanged and still enforced by `depguard` and the `boundary` CI job.

## [0.1.0] — 2026-09-12

The first release. Tagged at the state of `master` on 2026-09-12; everything
the working tree held is in the tag.

### Added

- Durable run executor (`runtime` package) built on the public Kit SDK only
- Journal-backed `kit.SessionManager` with lossless typed-message replay
- `Session.AppendStep` implements `kit.StepAppender` (Kit `v0.106.0`): a
  tool-calling step reaches the journal as one call, and
  `FileJournal.AppendStep` commits it as one buffered write and one fsync —
  the torn-write window went from "any crash between two fsyncs" to "a torn
  single Write". A cancelled context cannot drop a completed step
- `runtime.StepJournal`: an optional interface on BONNIE's own `Journal`
  seam, mirroring Kit's pattern so a host journal is not forced to implement
  the batch method. `FileJournal` and `MemoryJournal` implement it; anything
  else keeps the per-record fallback, covered by the torn-write repair
- Torn-write repair on `Restore` that drops incomplete trailing tool-calling
  steps
- `FileJournal` with JSONL format (one file per run, fsync policy configurable)
- `Runner.Cancel` to interrupt in-flight turns and `RunCancelled` state
- `Runner.Steer` for external turn control
- Events are journal-anchored and survive a reconnect past the backlog: every
  event carries the journal position it belongs to, the stream replays the
  records when the in-memory backlog has moved past the cursor, and the
  stream survives a process restart. Kit's mid-turn deltas stay live-only,
  marked as ephemeral
- `runtime.RunnerOption` and `runtime.WithEventBuffer` tune the reconnect
  backlog; a smaller buffer costs memory, not correctness
- Cross-process run ownership: the file journal takes an exclusive `flock`
  per run on first write and refuses a second writer with
  `runtime.ErrRunOwnedElsewhere` instead of letting records interleave and
  sequence numbers collide. Reads stay unlocked, so `runs list` and
  `runs show` work from any process. The lock is per host and says nothing on
  a network filesystem
- The `channeltest` package: the conformance suite a channel adapter joins
  instead of writing its own tests. The HTTP adapter is the first member. An
  unknown `TurnPolicy` is refused with `channel.ErrUnknownTurnPolicy`
  (HTTP: 400) rather than silently queueing
- microsandbox: every network policy mode is enforced. `SetNetworkPolicy`
  maps `deny-all` to `msb create --no-net` and an allow-list to
  `--net-rule allow@<host>`, verified with real egress
- `ErrPolicyMismatch`: `Open` returns it when an existing microsandbox
  carries a different network policy than the host configured, because `msb`
  fixes policy at create time and a silent reattach would run under the old
  rules
- The sandbox lifecycle is journalled: a sandbox that opens for a run writes
  a `RecordSandbox` naming the backend and the sandbox. `bonnie runs show`
  prints it in the timeline. A resumed run whose workspace was pruned gets a
  note in its conversation — never an empty workspace in silence.
  `bonnie sandbox prune [--dry-run]` deletes the sandboxes of terminal runs
- Provider capabilities behind optional interfaces, mirroring Kit's pattern:
  `sandbox.ExistenceChecker` and `sandbox.RunDeleter`. All three built-in
  backends implement both
- The live suites default to `opencode/kimi-k2.5` (`BONNIE_TEST_MODEL`
  still overrides)
- Nix flake: `packages.bonnie`, `packages.microsandbox`, `apps.msb`,
  `overlays.default`, and a `devShells.default` with Go 1.27, `golangci-lint`,
  `goreleaser`, and the microsandbox CLI. The `bonnie` Nix package wraps the
  binary so `msb` is on its PATH
- HTTP channel with six routes:
  - `POST /runs` — start a new run
  - `GET /runs/{id}` — fetch run state
  - `POST /runs/{id}` — send a message to a run
  - `POST /runs/{id}/respond` — answer a suspension
  - `POST /runs/{id}/cancel` — cancel a run
  - `GET /runs/{id}/stream` — NDJSON event stream with cursor support
- `bonnie` CLI with:
  - `serve` — mount the HTTP channel
  - `runs list` — show runs filtered by state
  - `runs show` — inspect run timeline, including the sandbox records
  - `sandbox prune` — reclaim the sandboxes of finished runs
- `AskTool` and `ApprovalTool` for human-in-the-loop workflows
- Live-model integration suites (behind the `integration` build tag), for the
  runtime and the sandbox
- Conformance suites a new implementation joins instead of writing its own
  tests: the journal suite (`memory` + `file`), the sandbox suite (18 cases
  per backend), and the channel suite
- `sandbox` package: isolated tool execution with `Local`, `Docker`, and
  `Microsandbox` backends, all CLI-driven and adding no dependency
- `sandbox.Agent` wires a sandboxed tool set into a `runtime.AgentFactory`;
  the sandbox opens lazily, so a parked run holds no compute
- `bonnie serve --sandbox` selects a backend from the CLI

### Known limits

Stated as plainly as the claims, because a framework that hides its limits
gets deployed into situations it cannot handle.

- Sandboxing is opt-in. Without it, tool calls run as the host process.
- Docker isolates with namespaces, not a guest kernel. Use microsandbox when
  the threat model includes hostile code.
- Sandbox egress is open unless a policy is set.
- microsandbox is verified on Linux with KVM only, not on macOS with Apple
  Silicon, and its network policy is fixed at create time.
- The HTTP channel carries a `Principal` but does not verify it.
- Run ownership is enforced per host with a `flock` per run; a shared network
  filesystem or a second writer still needs one owner in front.
- Events are journal-anchored: a reconnect past the in-memory backlog is
  served from the journal, and Kit's mid-turn deltas stay live-only.
- Reclaiming the sandboxes of finished runs is a command, not a background
  sweep.

---

[0.2.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.2.0
[0.1.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.1.0
