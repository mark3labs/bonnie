# Upstream asks for Kit

The home for anything BONNIE needs Kit to export or promise. BONNIE is
Kit's first serious external consumer, so every gap it hits is a gap real
SDK users hit — that pressure is the point of filing these.

**Status: all four `v0.1.0` asks were answered by Kit `v0.106.0` before
anything was filed.** That record — what was asked, why, and what each ask
became — is archived in
[`docs/archive/UPSTREAM.md`](archive/UPSTREAM.md). Nothing is open.

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
