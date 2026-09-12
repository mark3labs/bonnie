---
description: Audit and update project documentation for a recent change — grounded in the diff
---

Review recent code changes, identify all documentation surfaces that should mention them, and update each one — grounded in the actual diff, not guesses.

## Steps

1. **Identify the change**:
   - If the user input ($@) names a commit / PR / branch / topic, use that as the focus
   - Otherwise inspect `git log origin/master..HEAD --oneline` and `git diff origin/master...HEAD --stat`
   - Read the actual diff — never document features that are not in the code

2. **Inventory the doc surfaces** — BONNIE's are:
   - `README.md` — user-facing; every snippet must compile and every claim must hold
   - `docs/SPEC.md` — the authoritative spec. **Correct it in the same commit** when a change contradicts it: verified Kit facts (§3, with `file:line`), risks (§4), invariants (§8)
   - `docs/TASKS.md` — new work gets the next free ID; shipped work moves to the archive with a one-line pointer
   - `docs/L2.md` — the v0.2 discovery spec; update if the change touches the tree, manifest, or codegen contract
   - `docs/SANDBOX.md` — adapter contracts and what was verified on what hardware
   - `docs/HANDOVER.md` — state tables and "what I would do next"
   - `docs/UPSTREAM.md` — only if the change answers or adds a Kit ask
   - `CHANGELOG.md` — user-visible changes under `[Unreleased]`
   - `examples/` and their `README.md` — runnable, so they cannot drift silently
   - Godoc on every exported symbol — BONNIE is a public framework; the doc comment is part of the API

3. **Audit each surface** with `grep`:
   - Search for names of APIs the change touched, and every page that discusses the area
   - Decide per hit: needs an update, needs a cross-reference, or stays untouched

4. **Decide where new content lives**:
   - Prefer extending an existing section; check the page's heading outline first
   - Skip surfaces that genuinely do not apply — and say so explicitly in the report

5. **Draft the updates**:
   - Lead with what is new and why; one sentence
   - Code examples copied from real signatures — compile them
   - Match each surface's voice: the spec is terse and cites `file:line`; the README is direct and states limits plainly

6. **Verify** before committing:
   - `task fmt-check` and `go build ./...` if snippets changed
   - `go doc <pkg> <Symbol>` to sanity-check new godoc rendering

7. **Report**: every file changed, every file deliberately left alone (one-line reason), and the next step (`/commit-push`) — do not auto-commit unless asked

## Guidelines

- Read the diff before writing anything — invented API names erode trust faster than missing docs
- One topic per doc commit; keep doc updates separate from code changes when possible
- If the change contradicts the spec, the spec correction lands **in the same commit**, not a follow-up

$@
