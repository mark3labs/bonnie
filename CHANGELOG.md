# Changelog

All notable changes to BONNIE are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

The 0.1.0 scope below is complete in the working tree. The tag is not pushed
yet, so everything under 0.1.0 is still unreleased until it is.

### Added

- Nix flake: `packages.bonnie`, `packages.microsandbox`, `apps.msb`,
  `overlays.default`, and a `devShells.default` with Go 1.27, `golangci-lint`,
  `goreleaser`, and the microsandbox CLI
- The `bonnie` Nix package wraps the binary so `msb` is on its PATH
- microsandbox: every network policy mode is now enforced. `SetNetworkPolicy`
  maps `deny-all` to `msb create --no-net` and an allow-list to
  `--net-rule allow@<host>`, verified with real egress. Previously every mode
  except allow-all was refused with `ErrPolicyUnsupported`
- `ErrPolicyMismatch`: `Open` returns it when an existing microsandbox
  carries a different network policy than the host configured, because `msb`
  fixes policy at create time and a silent reattach would run under the old
  rules
- The live sandbox suite can run against any isolated backend:
  `BONNIE_TEST_SANDBOX=microsandbox` selects it (Docker remains the default)

### Fixed

- microsandbox: a missing guest file now returns `ErrNotFound`. `msb` words
  this as `error: stat <path>`, which the shared shell matcher did not
  recognise. The new matcher anchors on the path, so `sandbox not found` — a
  vanished workspace — is no longer flattened into an ordinary missing file
- microsandbox: a run that parked between turns could not be resumed. `msb ps`
  lists only running sandboxes, so a stopped one looked absent and `Open`
  tried to create it again, which `msb` refused. It now lists with `--all` and
  decodes the JSON `name` field rather than substring matching it
- microsandbox: `Delete` passes `--force`, so it no longer leaks a running
  microVM per sandbox. The conformance suite leaked about 9 GiB per run

## [0.1.0] — 2026-09-12

### Added

- Durable run executor (`runtime` package) built on the public Kit SDK only
- Journal-backed `kit.SessionManager` with lossless typed-message replay
- Torn-write repair on `Restore` that drops incomplete trailing tool-calling steps
- `FileJournal` with JSONL format (one file per run, fsync policy configurable)
- `Runner.Cancel` to interrupt in-flight turns and `RunCancelled` state
- `Runner.Steer` for external turn control
- Run event bus with a resumable cursor
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
  - `runs show` — inspect run timeline
- `AskTool` and `ApprovalTool` for human-in-the-loop workflows
- Live-model integration test (behind `integration` build tag)
- `sandbox` package: isolated tool execution with `Local`, `Docker`, and
  `Microsandbox` backends, all CLI-driven and adding no dependency
- `sandbox.Agent` wires a sandboxed tool set into a `runtime.AgentFactory`;
  the sandbox opens lazily, so a parked run holds no compute
- `bonnie serve --sandbox` selects a backend from the CLI

### Known limits

- Sandboxing is opt-in. Without it, tool calls run as the host process.
- Docker isolates with namespaces, not a guest kernel.
- Sandbox egress is open unless a policy is set.
- The HTTP channel carries a `Principal` but does not verify it.
- One process must own a run at a time. The file journal takes no
  cross-process lock.
- Events are not durable. The event bus keeps a bounded in-memory backlog.
- Sandbox lifecycle is not journalled: a deleted container leaves a run whose
  conversation survives but whose workspace does not.

---

[Unreleased]: https://github.com/mark3labs/bonnie/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/mark3labs/bonnie/releases/tag/v0.1.0
