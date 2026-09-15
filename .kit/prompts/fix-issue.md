---
description: Resolve a GitHub issue — read it, classify it, land the change on a branch
---

Resolve GitHub issue #$1 by reading it, classifying it, and producing the appropriate code or doc change. **Stop once the working tree contains the change** — committing, pushing, and opening a PR are handled by `/commit-push` and `/create-pr`.

## Steps

1. **Fetch the issue** — run:

       gh issue view $1 --json number,title,body,labels,state,author,comments
   - If the issue is closed, stop and ask the user whether to proceed
   - Read the **entire** thread — the latest comment often refines the ask

2. **Is it really a BONNIE issue?** BONNIE is a layer over Kit. If the root
   cause sits in Kit (`pkg/kit` behavior, a missing export, a `SessionManager`
   contract gap), stop here: file it upstream instead, per
   `docs/UPSTREAM.md` — write the ask with `file:line` citations into that
   document, then file the issue on `mark3labs/kit`. Record the local
   workaround (if any) as `// TODO(kit):` in a separate commit.

3. **Classify** from labels, title prefix, and body:
   - `bug` / `fix:` → reproduce, then fix
   - `enhancement` / `feature` / `feat:` → design, then implement
   - `documentation` / `docs:` → locate and update the docs
   - `question` / `discussion` → answer in a comment, write **no** code
   - Anything else → ask the user

4. **Branch off the default branch**:
   - `git checkout master && git pull --ff-only`
   - Name: `<type>/$1-<slug>` (e.g. `fix/42-replay-drops-reasoning`)

5. **Do the work** by type:
   - **Bug**: reproduce first (a failing test if feasible); fix the cause,
     not the symptom; add a regression test. Durability bugs need the
     cross-process shape: a second `Runner` sharing only the journal
   - **Feature**: check `docs/TASKS.md` and `docs/L2.md` first — if the
     feature has a spec, follow it; if it is large or breaking, sketch the
     design on the issue and wait for sign-off. Godoc on every exported
     symbol; `t.Parallel()` by default; new `Journal`/sandbox/channel
     implementations must join the matching conformance suite
   - **Docs**: update the surface named in the issue; verify examples compile
     (`go build ./...`) and snippets match real signatures

6. **Verify**: `task ci` (fmt-check, build and vet, `go test -race`,
   lint). If the change touches durability claims, also run the live suites
   (`task test-live`) when a provider key is present.

7. **Report**: branch name, `git status -s` summary, test/lint results, and
   the next steps: `/commit-push` (reference `#$1`, include `Fixes #$1`), then
   `/create-pr`.

## Guidelines

- **Stops at a clean working tree** — no `git commit`, `git push`, `gh pr create`
- If the change teaches something that contradicts `docs/SPEC.md`, correct the
  spec in the same commit and say so in the report
- Keep the change scoped; surface unrelated cleanups separately
- If already fixed on `master`, comment with the reference and stop
- Do not close the issue manually — the PR's `Fixes #$1` handles that

$@
