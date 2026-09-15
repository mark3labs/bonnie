---
description: Read-only audit for dead code, duplication, boundary violations, and durability holes
---

Perform a comprehensive **read-only** audit of this repository and report
findings. **Do not edit, rename, or delete any files.** Optional focus / scope
hints from the user: $@

## Scope

If the user supplied focus hints above (a package path, a subsystem name, a
concern like "journal" or "sandbox"), scope the audit accordingly. Otherwise
audit the whole repo, prioritising the highest-traffic packages first
(`runtime/`, `channel/http/`, `sandbox/`, `cmd/bonnie/`).

## Steps

1. **Map the repo first**:
   - `ls` / `find` the top-level layout; list every Go package and its layer
     (L0 Kit → L1 runtime → L3 channel → L4 cmd; `sandbox/` beside L1)
   - Read `AGENTS.md` and the package godoc — the contracts stated there
     define what counts as a violation
   - Note the public surface: `runtime/`, `channel/`, `sandbox/` are the SDK;
     `cmd/bonnie/` is a developer CLI, not the framework

2. **Hunt for dead code**:
   - Run `go vet ./...` and capture warnings
   - Grep for exported symbols (`^func [A-Z]`, `^type [A-Z]`) and
     cross-reference call sites; zero non-test references is a suspect
   - Check for unreferenced files, stale `// TODO(kit):` markers,
     commented-out blocks, and `_ = x` discard patterns
   - `unused` and `staticcheck` run inside `golangci-lint`; use `task lint`
   - **Do not delete anything** — list candidates with file:line and a
     confidence level (high / medium / low)

3. **Find unnecessary duplication**:
   - Near-identical bodies across `runtime/`, `channel/http/`, `sandbox/`,
     and `cmd/bonnie/` (flag plumbing is a repeat offender)
   - Distinguish *coincidental* duplication from *unnecessary* duplication —
     only flag the latter; propose the helper's home package

4. **Check concerns / boundary violations** — the ones this repo enforces:
   - **Public Kit SDK only**: any direct import of `kit/internal/...` or
     `charm.land/fantasy` outside `pkg/kit` is a violation (depguard and the
     `boundary` CI job exist for this; verify they still fire by reading
     `.golangci.yml` and `.github/workflows/ci.yml`)
   - **Layering**: `runtime/` must not import `channel/` or `sandbox/`
     (invariants 6, and the same logic for `sandbox/`); `cmd/bonnie` may
     import everything
   - **Journal integrity**: `Record.Payload` must carry the typed
     `kit.LLMMessage`; `Record.Text` is display-only. Any code path that
     rebuilds a message from `Text` is a replay-fidelity hole
   - **Append-only**: nothing may rewrite or reorder journal records;
     `Restore` repairs only the trailing torn step (`runtime/repair.go`)
   - **Refusal ethics**: a backend that cannot enforce a control must refuse
     it (invariant 10) — hunt for accepted-but-ignored security options

5. **Audit the durability claims** — this repo's reason to exist:
   - Every claim needs a test that crosses a process boundary in spirit
     (a second `Runner` sharing only the journal). List claims with no such
     test
   - Check the three conformance suites still cover the built-ins: journal
     (`runtime/journal_conformance_test.go`), sandbox
     (`sandbox/conformance_test.go`), channel (`channeltest/`)

6. **Spot refactor opportunities**:
   - Long functions, deep nesting, repeated `fmt.Errorf("bonnie: ...: %w", err)`
     chains that deserve a helper — but only where the wrapping context is
     genuinely uniform
   - Flag each with: location, current shape, proposed shape, risk

7. **Write the report** as your final message (do not write it to disk):
   Summary counts → dead code (by confidence) → duplication clusters →
   boundary violations (citing the rule) → durability holes (citing the
   invariant) → refactor opportunities → suggested next steps.

8. **End with an explicit reminder** that no files were modified.

## Guidelines

- **Read-only, always**: no `edit`, no `write`, no `git commit`. Use only
  `read`, `grep`, `find`, `ls`, and read-only bash (`go vet`, `go build -o
  /tmp/...`, `task lint`)
- **Cite every finding** with `path:line`
- **Be honest about confidence**; false positives are expensive
- 10 sharp findings beat 100 nitpicks
- **Don't propose architectural rewrites** — incremental, reviewable changes
  within the existing layer shape

$@
