# Contributing to BONNIE

## Public API boundary

**BONNIE imports only `github.com/mark3labs/kit/pkg/kit`.**

Never import `github.com/mark3labs/kit/internal/...`. Never import `charm.land/fantasy`.

This rule is not optional. It separates BONNIE from Kit's internal changes.

Kit re-exports every model type BONNIE needs as an alias:
- `kit.LLMMessage`
- `kit.LLMToolCallPart`
- `kit.LLMToolResultOutputContentText`
- (and all others)

Name fantasy directly and you pin BONNIE to Kit's own transitive dependency.
It must stay `// indirect` in `go.mod`.

### Enforcement layers

1. **Go compiler** — BONNIE's module path is not a prefix of Kit's, so the
   compiler already rejects internal imports at build time.

2. **depguard in .golangci.yml** — linter enforcement as a safety net.

3. **CI boundary job in .github/workflows/ci.yml** — the final check: a
   dedicated step that fails the build if the rule breaks.

If Kit exports a type but not a helper that operates on it, write the small
helper in BONNIE. See `toolResultText` in `runtime/util.go` for an example.

If Kit cannot do a thing at all, do these steps in order:

1. Check the public API again. It usually can.
2. Open an issue on `mark3labs/kit` to export what you need. Write it up in
   `docs/UPSTREAM.md` first.
3. Only then add a local workaround, and mark it `// TODO(kit):`.

## Local development

BONNIE and Kit are separate repositories.

### Working against the pinned Kit

Nothing is needed. `go.mod` pins the Kit version BONNIE is built and tested
against, and every test — including the ones that compile a scaffolded agent
tree in a temporary directory — uses that version. Clone and run `task check`.

### Working against Kit HEAD

When you are changing Kit and BONNIE together, add a `replace` directive to
your local `go.mod` and **do not commit it**:

```bash
go mod edit -replace github.com/mark3labs/kit=../kit
# ... work ...
go mod edit -dropreplace github.com/mark3labs/kit
```

A `go.work` in the parent directory also works, and is equally uncommitted.
Prefer the `replace`: a workspace silently covers every module in the graph,
so it is easy to build against a stale local checkout without noticing.
BONNIE used to require one for its build tests and no longer does.

### Publishing

Never commit a `replace` directive in `go.mod`, and never commit a `go.work`.

CI runs with `GOWORK=off`, so the published module builds against the Kit
version pinned in `go.mod`. This verifies the release works for users who
have neither.

## Building and testing

### Build

```sh
go build ./...
```

### Test all

```sh
go test -race ./...
```

### Test single package

```sh
go test -race ./runtime -run TestResumeAcrossProcessBoundary
```

### Lint

```sh
golangci-lint run
```

### Vet

```sh
go vet ./...
```

### Format

```sh
go fmt ./...
```

## Code style

- **Imports**: stdlib → third-party → local (blank lines between groups)
- **Naming**: camelCase for unexported, PascalCase for exported
- **Errors**: always check, wrap with `fmt.Errorf("bonnie: context: %w", err)`
- **Sentinel errors**: define at package level, test with `errors.Is`
- **Types**: prefer `any` over `interface{}`
- **JSON tags**: snake_case with `omitempty` where appropriate
- **Context**: first parameter for blocking operations
- **Godoc**: every exported symbol gets a doc comment. BONNIE is a public
  framework; treat the doc comment as part of the API.

## Testing

- **t.Parallel()** by default in every test.
- Use `fakeAgent` from `runner_test.go` rather than a live model.
- Every durability claim needs a test that crosses a process boundary in
  spirit: build a second `Runner` sharing only the journal. See
  `TestResumeAcrossProcessBoundary` for the pattern.

Live-model integration tests use the `integration` build tag. Run them with:

```sh
go test -tags integration -race ./runtime
```

These tests need a provider API key. They skip cleanly when the key is absent.
See `README.md` for the environment variable name.

## Commit messages

Use conventional commits:

- `feat:` — new feature
- `fix:` — bug fix
- `docs:` — documentation only
- `test:` — test changes only

Example:

```
feat: add Runner.Cancel for interrupting active turns

- Call it from channel/http when the client sends POST /bonnie/v1/runs/{id}/cancel
- Checkpoint RunCancelled state to the journal
- Verify the repair from T-003 handles torn writes on cancelled runs
```

## Git workflow

1. Create a branch from `master`.
2. Make your changes. Follow the code style and test rules.
3. Run `go test -race ./...` and `golangci-lint run`.
4. Verify no new direct imports of `kit/internal/*` or `charm.land/fantasy`.
5. Open a pull request.
6. Ensure every commit message describes *what changed and why*, not the
   checklist process.

## Before you merge

Check the PR template. Every box matters.
