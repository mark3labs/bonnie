# BONNIE Agent Guidelines

Always talk in ASD-STE100 Simplified Technical English.

## Start here

Read [`docs/SPEC.md`](docs/SPEC.md) before changing anything. It holds the
verified facts about Kit (with file:line citations), the known defects, and the
invariants. Then read [`docs/TASKS.md`](docs/TASKS.md) for the work queue.

If you learn something that contradicts the spec, **correct the spec in the
same commit**. A stale spec is worse than none.

## Build/Test Commands
- **Shortcut**: `task` — `task check` (fmt, lint, test), `task ci` (CI parity), `task dev -- serve` (Taskfile.yml mirrors everything below)
- **Build**: `go build ./...`
- **Test all**: `go test -race ./...`
- **Test single**: `go test -race ./runtime -run TestResumeAcrossProcessBoundary`
- **Lint**: `golangci-lint run`
- **Vet**: `go vet ./...`
- **Format**: `go fmt ./...`

## The one rule that matters

**BONNIE uses the public Kit SDK only: `github.com/mark3labs/kit/pkg/kit`.**

Never import `github.com/mark3labs/kit/internal/...`. The Go compiler already
rejects it, because BONNIE's module path is not a prefix of Kit's. Do not try
to work around this by vendoring Kit, copying internal code, or merging the
repositories.

Never import `charm.land/fantasy` either. Kit re-exports every model type
BONNIE needs as an alias (`kit.LLMMessage`, `kit.LLMToolCallPart`,
`kit.LLMToolResultOutputContentText`, ...). Naming fantasy directly pins BONNIE
to Kit's own transitive dependency. It must stay `// indirect` in `go.mod`.
When Kit aliases a type but not a helper that operates on it, write the small
helper in BONNIE — see `toolResultText` in `runtime/util.go`.

If you need something Kit does not export:
1. Check whether the public API can already do it. It usually can.
2. If not, open an issue on Kit to export it from `pkg/kit`.
3. Only then consider a local workaround, and mark it `// TODO(kit):`.

The rule is enforced three ways, and the order they fire in is not the order
of authority:

| Layer | Where | Fires |
|---|---|---|
| Kit extension | `.kit/extensions/kit-boundary.go` | before the `write`/`edit` lands, in this checkout only |
| Go compiler | module path is not a prefix of Kit's | at build time |
| `depguard` | `.golangci.yml` | in `task lint` and the CI `lint` job |

**`depguard` is the authority.** The extension is a guard-rail: it only runs
when a person drives Kit here, so it cannot see an edit made in an editor, by
another tool, or by a dependency bump. Never delete the `depguard` rule
because the extension exists. There is no `boundary` CI job any more;
`docs/SPEC.md` §2 records why it went and what it had to get right.

## Architecture

```
L4  CLI, evals, traces                 CLI implemented; evals planned
L3  channel/    inbound transports     channel/http implemented
L2  discovery   agent/ tree + codegen  default layout, init, codegen, dev,
                                       build; configuration is code (T-024)
L1  runtime/    durable run executor   implemented
L0  kit/pkg/kit                        upstream, unmodified
```

The root package `github.com/mark3labs/bonnie` is the entry point an agent
tree calls: `bonnie.New().Serve()` is a complete agent. It owns the serving
the default layout constants, and the CLI calls the same code, so the two
cannot drift. **There is no manifest file** — a setting is a file at a fixed
path or a Go option, never both (invariant 14). Do not add a config file back.

### L1 durability seams (runtime/)
BONNIE gets durability from four public Kit extension points. Know these before
changing anything in `runtime/`. Full detail with citations in `docs/SPEC.md` §3.

| Need | Kit public API | BONNIE file |
|---|---|---|
| Journal every message | `Options.SessionManager` | `session.go` |
| Checkpoint each step | `Kit.OnStepFinish` | `runner.go` |
| Inject replayed context | `Kit.OnContextPrepare` | `runner.go` |
| Suspend for human input | `ToolOutput{Halt, FinalValue}` | `suspend.go` |

- `Session` implements all 20 methods of `kit.SessionManager` (the count was
  recorded as 21 until `v0.106.0` was verified; it was wrong). The
  `var _ kit.SessionManager = (*Session)(nil)` assertion in `session.go` is a
  deliberate tripwire: if Kit breaks the v0.x freeze and widens the interface,
  the build breaks here first. `Session` also implements `kit.StepAppender`
  (`AppendStep`), so a tool-calling step reaches the journal as one atomic
  write.
- `Agent` is an interface, not `*kit.Kit`. This keeps the executor testable
  without credentials and documents how small the Kit surface actually is.
  Do not replace it with a concrete type.
- Records are append-only. `Replay` must be deterministic.
- **Replay must stay lossless.** `Record.Payload` carries the JSON-encoded
  `kit.LLMMessage`; `Record.Text` is a display-only projection. Never rebuild a
  message from `Text` alone — that drops tool calls silently. Guard test:
  `runtime/replay_fidelity_test.go`.

### The journal is SQLite, and it must stay CGO-free
`SQLiteJournal` (`runtime/sqlitejournal.go`) is the durable journal: one
`<root>/journal.db`, WAL, one transaction per step. The driver is
`modernc.org/sqlite`, which is **pure Go**. Never swap in
`github.com/mattn/go-sqlite3` or anything else that needs C: `bonnie build`
promises a single static binary and `goreleaser` cross-compiles four targets
from one machine. `depguard` denies the CGO driver and CI runs
`CGO_ENABLED=0 go build ./...`, so a mistake fails loudly — do not silence
either guard.

Two more rules that are easy to break:

- **Settings go in the DSN, not in a `PRAGMA` after `sql.Open`.** A pragma
  statement applies to one pooled connection, not the pool.
- **Do not delete the torn-write repair** (`runtime/repair.go`) on the
  grounds that a step is now a transaction. Runs imported from the old JSONL
  format, and third-party journals, still carry the shape it fixes.

See `docs/SPEC.md` §4.14.

## Code Style
- **Imports**: stdlib → third-party → local (blank lines between)
- **Naming**: camelCase (unexported), PascalCase (exported)
- **Errors**: always check, wrap with `fmt.Errorf("bonnie: context: %w", err)`
- **Sentinel errors**: define at package level, test with `errors.Is`
- **Types**: prefer `any` over `interface{}`
- **JSON**: snake_case tags with `omitempty` where appropriate
- **Context**: first parameter for blocking operations
- **Godoc**: every exported symbol. BONNIE is a public framework; treat the
  doc comment as part of the API.

## Testing
- Every durability claim needs a test that crosses a process boundary in
  spirit: build a second `Runner` sharing only the journal.
- Use `fakeAgent` in `runner_test.go` rather than a live model.
- `t.Parallel()` by default.

## Terminal rendering
Kit renders the agent's terminal UI. BONNIE's framework packages ship no TUI
today; the CLI owns one interactive surface, `bonnie dev` / `bonnie chat`
(`cmd/bonnie/tui`, charm's bubbletea/bubbles/lipgloss v2, with herald-md for
assistant markdown — the same libraries upstream Kit's TUI uses), and styles its
own help and errors with fang. The layered framework packages stay
terminal-free; a host that wants an off-screen conversation uses the HTTP
channel. The hard boundary is the public-Kit-SDK rule above, and it extends
here: the TUI talks to the wire the channel exposes, never to Kit internals.

## Local development
BONNIE and Kit are separate repos. Nothing is needed to work against the
pinned Kit: `go.mod` names the version, and every test — including the ones
that compile a scaffolded tree in a temp directory — uses it.

To work against Kit HEAD, add an **uncommitted** replace:

```
go mod edit -replace github.com/mark3labs/kit=../kit
```

Never put a `replace` directive in the published `go.mod`. CI builds against
the Kit version pinned there, so a release is always proven against the
version a user gets.
