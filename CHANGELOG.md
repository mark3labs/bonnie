# Changelog

All notable changes to BONNIE are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

**Breaking: the manifest is gone. Configuration is code.** `agent.yaml` (and
`agent.toml`, `agent.json`) no longer exist. A tree's data lives at fixed
paths and everything else is a Go option on `bonnie.New`, so a setting that
does not exist is a compile error rather than a key nothing reads. See
`docs/TASKS.md` T-024 and `docs/L2.md`.

### Added

- **The root package `github.com/mark3labs/bonnie`.** `bonnie.New().Serve()` is a
  complete agent: it reads `instructions.md`, roots the agent's files in
  `workspace/`, journals to `.bonnie`, serves the HTTP channel, and drains
  in-flight turns on a signal. The scaffolded `main.go` is now one call.

  ```go
  package main

  import "github.com/mark3labs/bonnie"

  func main() { bonnie.New().Serve() }
  ```

- **Options for everything that is not a file**, passed to `New`: `WithModel`,
  `WithSandbox`, `WithNetwork`, `WithTools`, `WithKit`, `WithSlack`,
  `WithDiscord`, `WithTelegram`, `WithChannel`, `WithWorkspace`,
  `WithInstructions`, `WithJournal`, `WithAddr`, `WithShutdownTimeout`, and
  `WithAgentFactory` for a host that brings its own agent.
- **`Agent.Run(ctx) error`** is `Serve` without the process — no flags, no
  signal handler, no exit — for a host that already owns those.
- **The default layout as exported constants**: `DefaultInstructions`,
  `DefaultWorkspace`, `DefaultSkills`, `DefaultJournal`, `DefaultAddr`. The
  scaffold, codegen, the dev loop, and the runtime all read them, so the
  layout is defined once.
- **`bonnie.Register` / `Registered` / `Tree`.** The generated
  `bonnie_gen.go` registers the tree's tools and embedded data from `init`,
  so `main.go` never names a tool or an embed.
- `-addr` and `-model` are operator flags on every serving binary, applied
  after the options, so one binary can move port or model without a rebuild.

### Changed

- **`bonnie init`** scaffolds `main.go`, `instructions.md`, `go.mod`,
  `bonnie_gen.go`, `skills/`, `workspace/`. `--model` writes the option into
  `main.go`. `--format` and `--title` are gone.
- **`bonnie build`** names its output from the module path in `go.mod`.
- **`bonnie serve`** is the flag-only generic host, for running an agent with
  no tree. `--agent` and `--config` are gone: a tree's configuration is Go in
  its own `main.go`, so a tree is served by running it.
- **The startup banner reports the address actually bound**, so `-addr :0`
  names the real port.
- **Chat channels mount through options** rather than manifest keys. Their
  credentials still come only from the environment, and a missing one is
  still a startup error naming the variable.
- `gopkg.in/yaml.v3` and `github.com/pelletier/go-toml/v2` return to indirect
  dependencies. `go.sum` is unchanged.

### Removed

- `agent.Manifest`, `agent.Load`, `agent.LoadFile`, `agent.ParseManifestData`,
  `agent.APIVersion`, and every manifest sentinel error.
- `serve --agent`, `serve --config`, `init --format`, `init --title`,
  `build --config`.
- The generated `discoveredTools()`, `embeddedInstructions()`,
  `embeddedManifest()`, `embeddedSkills()`, and `embeddedWorkspace()`
  symbols, replaced by `bonnie.Register`.
- **The zero-Go path.** A host with no Go toolchain can no longer serve a
  tree from data. Authoring an agent now needs Go from the first step, as it
  did from the fifth before. The binary `bonnie build` produces still needs
  nothing on the host, which is the end of the arc that matters.

### Fixed

- **A chat-channel option can be reused.** `WithSlack`, `WithDiscord`, and
  `WithTelegram` filled their captured `Config` from the environment, and the
  fill only writes an empty field — so the first use left the credentials
  inside the option's closure and every later use skipped the environment. An
  `Option` held in a variable and passed to two agents, or an `Agent.Run`
  called twice, served the credentials read at the first call rather than the
  ones set now. Each option now copies its config per call. Guard test:
  `TestChatChannelOptionsMount`.

### Migration

For each key in your `agent.yaml`, write the option in `main.go`:

| Manifest | `main.go` |
|---|---|
| `model:` | `bonnie.WithModel(...)` |
| `instructions:` | `bonnie.WithInstructions(...)` — or rename the file to `instructions.md` |
| `workspace:` | `bonnie.WithWorkspace(...)` — or rename the directory to `workspace/` |
| `sandbox.kind`, `sandbox.image` | `bonnie.WithSandbox(sandbox.Docker(...))` |
| `sandbox.network` | `bonnie.WithNetwork(sandbox.NetworkPolicy{...})` |
| `channels.http.addr` | `bonnie.WithAddr(...)` |
| `channels.slack` / `discord` / `telegram` | `bonnie.WithSlack(...)` / `WithDiscord(...)` / `WithTelegram(...)` |
| `title:` | the directory name — `bonnie build` reads it from `go.mod` |
| `apiVersion:` | nothing; the Go type system replaced it |

Then delete `agent.yaml` and run `bonnie build`. A tree that has no `main.go`
(the old zero-Go scaffold) gets one from `bonnie init .`, which never
overwrites what is already there.

---

The audit increment: a read-only pass over every non-test file against the
invariants in `docs/SPEC.md` §8. It found no boundary violation and no
journal-integrity hole; it found two resource leaks, two settings that were
accepted and ignored, and one durability claim with no test in the shape the
project demands. All are closed here. See `docs/SPEC.md` §4.12.

### Fixed

- **A run's event stream no longer leaks goroutines when a client
  disconnects.** `Runner.StreamEvents` forwarded on an unbuffered channel, so
  a client that went away between two events left the forwarder parked on its
  send and the bus subscriber's pump parked behind it. A goroutine blocked in
  a send cannot see the unsubscribe. Every reconnect that raced an event cost
  a long-lived server two goroutines and their queued events, for the life of
  the process. Stopping a stream now releases every send it owns.
- **Reading an unknown run no longer grows the file journal.** `FileJournal`
  kept a handle for every run ID it was ever asked about, so a server
  reachable from outside paid a permanent map entry for each 404. Reads now
  keep a handle only for a run that exists.
- **`--sandbox-image` reaches every backend that runs an image.**
  `--sandbox auto` built its candidates without the image, so an operator who
  named one silently got the default; `--sandbox local` accepted an image it
  cannot run and is now refused. The startup banner names the image in force.
- **Reserved runs are no longer addressable.** BONNIE keeps its address map
  in a run under `runtime.ReservedRunPrefix`. A caller who knew the prefix
  could start a turn on it through any chat transport, and `bonnie sandbox
  prune` listed it to operators. Both now refuse and filter.
- **`channel/http` caps a request body at 1 MiB**, with a 413 that names the
  limit — the webhook adapters always did. A write refused because another
  process owns the run (`ErrRunOwnedElsewhere`) now answers 409, not 500.

### Added

- `sandbox.Imaged`, the optional interface a provider implements to report
  the image it really runs. It is what makes the image testable and the
  banner honest.
- `chat.DeliveryText` and `chat.FirstLine`: the rule for what a person sees
  at the end of a turn, which lived three times, byte for byte, in the Slack,
  Discord, and Telegram adapters.
- `Manifest.WorkspaceDir`, one answer for the agent's root. The serve
  wiring, the dev loop's watcher, and codegen each held their own copy of the
  default, so a renamed workspace could be honored by one and missed by
  another.
- `TestCancelledRunContinuesInASecondRunner`: `Runner.Cancel` promises a
  cancelled run can be continued, and every durability claim needs a test
  that crosses a process boundary. This one had only a same-process
  `Restore`.

### Removed

- `EventBus.Backlog`, which had no caller in the repository.
  `Runner.StreamEvents` is the supported way to read a run's events, and it
  has been since journal-backed catch-up landed.

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
