# Changelog

All notable changes to BONNIE are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- L2 discovery, first increment (`T-017`): the agent tree is discovered
  from a manifest. `bonnie init` scaffolds a tree — `agent.yaml`,
  `instructions.md`, and the `skills/` and `workspace/` seed directories —
  with no Go module by default; `--tools` adds a module with a sample tool.
  `bonnie serve --agent DIR` serves a discovered tree with no build and no
  toolchain: instructions from disk, model, sandbox, and channel bindings
  from the manifest. Flags override the manifest, and the startup banner
  names the source that won. Files under the manifest's `workspace:` are
  mirrored into every run's sandbox on every backend, and a file the model
  already wrote is never overwritten.
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

[0.1.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.1.0
