# Upstream asks for Kit

The home for anything BONNIE needs Kit to export or promise. BONNIE is
Kit's first serious external consumer, so every gap it hits is a gap real
SDK users hit — that pressure is the point of filing these.

**Status: all four `v0.1.0` asks were answered by Kit `v0.106.0` before
anything was filed.** That record — what was asked, why, and what each ask
became — is archived in
[`docs/archive/UPSTREAM.md`](archive/UPSTREAM.md). One ask is open.

## Open

### Export the extension event and result types from `pkg/extensions`

**The gap.** A consumer cannot write a Go test for its own Kit extension
using only the public API.

**The citation.** Kit ships a test harness at `pkg/extensions/test`, but the
harness signs its API with internal types:
`Harness.Emit(event extensions.Event) (extensions.Result, error)`
(`pkg/extensions/test/harness.go:152`), where `extensions` is
`github.com/mark3labs/kit/internal/extensions`. `New`, `LoadFile` and
`EmitJSON` have the same shape (`harness.go:60`, `:72`, `:163`). The package
sits under `pkg/`, so it reads as public, but it cannot be called from
outside Kit's module.

**The reproduction.** BONNIE's boundary guard is a Kit extension
(`.kit/extensions/kit-boundary.go`, `docs/SPEC.md` §2). A BONNIE test of it
would have to import `internal/extensions` to build a `ToolCallEvent` — which
is exactly the rule the guard exists to enforce. The cases were therefore run
from a temporary test placed **inside the Kit checkout**, then deleted. The
guard has no test in its own repository.

This is not cosmetic. The guard shipped with two defects that a passing unit
test did not catch — it read a composite-literal element as an import, and it
applied to Go files in other checkouts, including Kit's own. Both were found
by using it. An extension its author cannot test in their own repository is
an extension that rots.

**The ask.** Re-export the extension-facing event and result types
(`ToolCallEvent`, `ToolCallResult`, `SessionStartEvent`, the `Event` and
`Result` interfaces, ...) from `pkg/extensions`, as Kit already re-exports the
model types from `pkg/kit`, and restate the harness signatures in terms of
the aliases. Nothing else about the harness needs to change: `New`,
`Context()`, `LoadFile` and `Emit` are already the right shape.

**What it became.** Not filed yet.

## How to record a future ask

The rule is in `AGENTS.md` and `CONTRIBUTING.md`: check the public API
again, open an issue on `mark3labs/kit`, and only then reach around — with
a `// TODO(kit):` marker. Before you file, write the ask here:

1. **Name the gap** in one sentence — what BONNIE needs and cannot do with
   the public `pkg/kit`.
2. **Cite it.** The public API's file:line, verified against the version in
   `go.mod`. An ask without a citation is a guess; `docs/SPEC.md` §3 is the
   model for how precise the citation should be.
3. **Give a reproduction** or the failing shape, as small as it can be.
4. **Say what it became** after it lands: its PR, the symbol it exported,
   and where BONNIE adopted it.

An ask is open until it lands. When it does, the record moves to
[`docs/archive/UPSTREAM.md`](archive/UPSTREAM.md) and the adoption lands in
`docs/SPEC.md` §3 (re-verify the file:line against the new Kit version in
the same commit).

The `file-issue` and `fix-issue` prompts in `.kit/prompts/` drive this same
document; keep its shape stable.
