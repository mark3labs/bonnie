---
description: File a GitHub issue on this repo — or on Kit, when the bug lives upstream
---

File a GitHub issue. The user wants to create an issue about: $@

## First decision: which repo?

- **BONNIE's own behavior** (runtime, channels, sandbox, CLI, docs) → file
  here, with the templates below
- **A Kit gap** (`pkg/kit` missing an export, wrong `SessionManager` behavior,
  something BONNIE can only work around) → file on `mark3labs/kit` instead.
  The ask must carry `file:line` citations into Kit at the pinned version and
  a proof the public API cannot do it. Record the outcome in
  `docs/UPSTREAM.md` — that document holds the standing asks and the format
- Say which choice you made and why before filing

## BONNIE issue templates

| Type | Template | Use for |
|------|----------|---------|
| Bug | `--template bug_report` | A run did not survive, replay lost something, a suspension misbehaved, the CLI lied |
| Feature | `--template feature_request` | A missing capability — including documentation gaps (there is no docs template) |

## Steps

1. **Determine the type** from the user input
2. **Gather evidence before writing**:
   - For bugs: the exact command, the journal directory contents
     (`sqlite3 .bonnie/journal.db "SELECT run_id, state FROM runs"`,
     `bonnie runs show <id> --json`), the Go version,
     and whether `GOWORK=off` reproduces it — a go.work checkout hides
     version-pinning bugs
   - For durability bugs: run `bonnie runs show <id> --json` and attach the
     record kinds (redact message text if asked); state which invariant of
     `docs/SPEC.md` §8 broke
   - For features: check `docs/TASKS.md` and `docs/L2.md` first — the feature
     may already be specified or deliberately out of scope; say so and stop
     if it is (scope decisions live in SPEC §5 and §7)
3. **Reproduce before writing**, when possible: a minimal `go run` against
   `examples/minimal` is worth more than a paragraph of guesswork
4. **Write the issue** with the template filled truthfully:
   - Title: one line, the observable failure or the missing capability
   - Body: what happened, what was expected, the minimal reproduction, and
     the version (`bonnie version`)
5. **File it**: `gh issue create --template <template> --title "..." --body "..."`
6. **Report** the issue URL and any follow-up the user should run
   (`/fix-issue <number>` once triaged)

## Guidelines

- Never invent a reproduction — run it or say it is unverified
- One issue per report; split multi-part asks
- Search existing issues first (`gh issue list --search "..."`) and link a
  duplicate instead of filing a new one

$@
