# AGENTS.md

Guidance for coding agents that work in this repository. Human contributors
read [`README.md`](README.md) and [`CONTRIBUTING.md`](CONTRIBUTING.md).

Write and speak in ASD-STE100 Simplified Technical English.

## Project overview

BONNIE makes an agent run durable. It wraps the
[Kit](https://github.com/mark3labs/kit) agent SDK. A run continues after a
crash, waits days for a human answer, and replies over HTTP.

- Module: `github.com/mark3labs/bonnie`
- Language: Go 1.27
- Entry point for an agent tree: `bonnie.New().Serve()`

**The code is the spec.** There is no specification document and no task file.
The godoc on each exported symbol, and the comments on the tests, carry the
reasoning — many of them record a real defect and why the shape is what it is.
Read them before you change the shape.

When you learn that a comment is wrong, **correct the comment in the same
commit**. A stale comment is worse than none.

Open work is ad hoc or a
[GitHub issue](https://github.com/mark3labs/bonnie/issues).

Layout:

| Path | Function |
|---|---|
| `bonnie.go`, `options.go`, `run.go` | root package: serve, options, run API |
| `runtime/` | durable run executor and SQLite journal |
| `channel/` | inbound transports: `http`, `chat`, `slack`, `discord`, `telegram`, `github` |
| `agent/` | agent-tree scaffold and code generation |
| `sandbox/` | tool sandboxes: local, exec, docker, landlock, microsandbox |
| `cmd/bonnie/` | CLI (`init`, `dev`, `chat`, `serve`, `build`, `runs`, `sandbox`) |
| `examples/` | `minimal`, `hitl-restart` |

## The one rule that matters

**BONNIE imports the public Kit SDK only: `github.com/mark3labs/kit/pkg/kit`.**

- Never import `github.com/mark3labs/kit/internal/...`.
- Never import `charm.land/fantasy`. Kit re-exports each model type as an
  alias (`kit.LLMMessage`, `kit.LLMToolCallPart`,
  `kit.LLMToolResultOutputContentText`). `fantasy` must stay `// indirect` in
  `go.mod`.
- Do not vendor Kit, copy internal code, or merge the two repositories.

If Kit exports a type but not a helper for it, write the small helper in
BONNIE. See `toolResultText` in `runtime/util.go`.

If the public API cannot do the task:

1. Examine the public API again. Usually it can.
2. Open an issue on `mark3labs/kit` to export what you need.
3. Only then write a local workaround, and mark it `// TODO(kit):`.

Three layers enforce the rule. The order they fire in is not the order of
authority:

| Layer | Where | Fires |
|---|---|---|
| Kit extension | `.kit/extensions/kit-boundary.go` | before a `write`/`edit` lands, in this checkout only |
| Go compiler | BONNIE's module path is not a prefix of Kit's | at build time |
| `depguard` | `.golangci.yml` | in `task lint` and the CI `lint` job |

**`depguard` is the authority.** The extension is only a guard-rail: it does
not see an edit from an editor, a different tool, or a dependency bump. Do not
delete the `depguard` rule because the extension exists. There is no
`boundary` CI job any more: `depguard` denies both paths by prefix whatever
the module layout, so the job added nothing.

## Setup commands

Use `task` (see [`Taskfile.yml`](Taskfile.yml)). Nix users get the full tool
set with `direnv allow` or `nix develop`.

- Build the binary: `task build`
- Install the CLI: `task install`
- Run the CLI: `task dev -- serve --journal .bonnie`
- Tidy modules: `task tidy`

Work against the pinned Kit by default. `go.mod` names the version, and each
test uses it. To work against Kit HEAD, add an **uncommitted** replace:

```bash
go mod edit -replace github.com/mark3labs/kit=../kit
# ... work ...
go mod edit -dropreplace github.com/mark3labs/kit
```

Never commit a `replace` directive. CI builds against the pinned version, so a
release is always proven against the version a user gets.

## Build and test commands

- Full quality loop: `task check` (format check, lint, test)
- CI parity, run it before you push: `task ci`
- Build: `go build ./...`
- Test all: `go test -race ./...`
- Test one test: `go test -race ./runtime -run TestResumeAcrossProcessBoundary`
- Lint: `golangci-lint run ./...`
- Vet: `go vet ./...`
- Format: `go fmt ./...`
- Coverage report: `task test-cover`

Correct each test failure, lint message, and type error before you finish a
task.

The CGO-free build is proven, not assumed:

```bash
CGO_ENABLED=0 go build ./...
```

## Testing instructions

- CI is in [`.github/workflows/ci.yml`](.github/workflows/ci.yml). The `test`
  job builds, vets, tests with `-race`, and builds again with `CGO_ENABLED=0`.
  The `lint` job runs `golangci-lint`.
- Use `t.Parallel()` by default.
- Use `fakeAgent` from `runtime/runner_test.go`. Do not call a live model in a
  standard test.
- Each durability claim needs a test that crosses a process boundary in
  spirit: make a second `Runner` that shares the journal only. See
  `TestResumeAcrossProcessBoundary`.
- Live-model tests use the `integration` build tag and a provider key
  (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, or `GEMINI_API_KEY`). They must skip
  cleanly without a key, never fail. Run them with `task test-live`.
- Add or update tests for the code you change, also when nobody asks.

## Code style

- **Imports**: stdlib → third-party → local, with a blank line between groups.
- **Naming**: camelCase for unexported, PascalCase for exported.
- **Errors**: always examine them. Wrap with
  `fmt.Errorf("bonnie: context: %w", err)`.
- **Sentinel errors**: declare at package level, test with `errors.Is`.
- **Types**: prefer `any` to `interface{}`.
- **JSON**: snake_case tags, with `omitempty` where it applies.
- **Context**: the first parameter of each blocking operation.
- **Godoc**: each exported symbol gets a doc comment. BONNIE is a public
  framework; the doc comment is part of the API.

## Architecture notes

```
L4  CLI, evals, traces                 CLI implemented; evals planned
L3  channel/    inbound transports     channel/http implemented
L2  discovery   agent/ tree + codegen  default layout, init, codegen, dev,
                                       build; configuration is code
L1  runtime/    durable run executor   implemented
L0  kit/pkg/kit                        upstream, unmodified
```

The root package is the entry point an agent tree calls. It owns the serving
and the default layout constants, and the CLI calls the same code, so the two
cannot drift.

### Durability seams (`runtime/`)

BONNIE gets durability from four public Kit extension points. Learn them
before you change `runtime/`.

| Need | Kit public API | BONNIE file |
|---|---|---|
| Journal each message | `Options.SessionManager` | `session.go` |
| Checkpoint each step | `Kit.OnStepFinish` | `runner.go` |
| Inject replayed context | `Kit.OnContextPrepare` | `runner.go` |
| Suspend for human input | `ToolOutput{Halt, FinalValue}` | `suspend.go` |

- `Session` implements all 20 methods of `kit.SessionManager` (the count was
  recorded as 21 until `v0.106.0` was verified; it was wrong). The assertion
  `var _ kit.SessionManager = (*Session)(nil)` in `session.go` is a deliberate
  tripwire: if Kit breaks the v0.x freeze and widens the interface, the build
  breaks here first. `Session` also implements `kit.StepAppender`
  (`AppendStep`), so a tool-calling step reaches the journal as one atomic
  write.
- `Agent` (`runtime/runner.go`) is an interface, not `*kit.Kit`. This keeps
  the executor testable without credentials, and documents how small the Kit
  surface actually is. Do not replace it with a concrete type.
- Records are append-only. `Replay` must be deterministic.
- **Replay must stay lossless.** `Record.Payload` holds the JSON-encoded
  `kit.LLMMessage`. `Record.Text` is a display-only projection. Never rebuild
  a message from `Text` alone, because that drops tool calls silently. Guard
  test: `runtime/replay_fidelity_test.go`.

### The journal is SQLite and must stay CGO-free

`runtime/sqlitejournal.go` keeps one `<root>/journal.db` with WAL and one
transaction per step. The driver is `modernc.org/sqlite`, which is pure Go.

- Never use `github.com/mattn/go-sqlite3` or another driver that needs C.
  `bonnie build` promises one static binary, and `goreleaser` cross-compiles
  four targets from one machine.
- Put settings in the DSN, not in a `PRAGMA` after `sql.Open`. A pragma
  applies to one pooled connection, not to the pool.
- Do not delete the torn-write repair (`runtime/repair.go`). Runs imported
  from the older JSONL format, and third-party journals, still have the shape
  it corrects.

### Configuration is code

There is no manifest file. A setting is a file at a fixed path or a Go option,
never both. Do not add a configuration file.

### Terminal rendering

Kit renders the agent's terminal UI. BONNIE's framework packages ship no TUI
today. The CLI owns one interactive surface, `bonnie dev` / `bonnie chat`
(`cmd/bonnie/tui`, charm's bubbletea/bubbles/lipgloss v2, with herald-md for
assistant markdown — the same libraries upstream Kit's TUI uses), and styles
its own help and errors with fang. The layered framework packages stay
terminal-free. A host that wants an off-screen conversation uses the HTTP
channel. The hard boundary is the public-Kit-SDK rule above, and it extends
here: the TUI talks to the wire the channel exposes, never to Kit internals.

## Commit and PR instructions

- Use conventional commits: `feat:`, `fix:`, `docs:`, `test:`, `chore:`,
  `refactor:`. Add `!` for a breaking change.
- Write what changed and why, not the process you followed.
- Branch from `master`.
- Run `task check` before each commit.
- Make sure no new import of `kit/internal/*` or `charm.land/fantasy` exists.
- Complete each box in the PR template.
- Release steps are in [`docs/RELEASE.md`](docs/RELEASE.md). Keep
  `CHANGELOG.md` current.
