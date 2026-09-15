---
description: Open a GitHub PR for the current branch using the repo's PR template
---

Open a GitHub pull request for the current branch, filling out the repository's PR template with a description grounded in the actual commits and diff.

## Steps

1. **Verify the branch is pushed**:
   - `git status -sb` and `git log @{u}..HEAD --oneline 2>/dev/null` — if there is no upstream or unpushed commits, run `git push -u origin "$(git branch --show-current)"` first
   - If the working tree is dirty, stop and suggest `/commit-push`
2. **Gate on CI parity first**: `task ci` — the PR will run the test and lint jobs; do not open a PR that fails locally
3. **Gather context**:
   - `git log origin/master..HEAD --oneline`
   - `git diff origin/master...HEAD --stat`, then the full diff — read it
   - Identify the linked issue (commit messages, branch name, or user input: $@) — capture as `Fixes #N`
4. **Fill the template** at `.github/pull_request_template.md`:
   - **Description**: what changed and why, grounded in the diff. For durability changes, name the invariant (SPEC §8) and the test that crosses a process boundary
   - **Type of change**: tick the one accurate box
   - **Checklist**: tick only what is genuinely true
5. **Open it**: `gh pr create --fill` with the drafted body; base branch is `master`
6. **Watch the checks**: `gh pr checks --watch` — `test`, `lint`, and `boundary` must all go green. If `boundary` fails, a direct import crossed the public-Kit-SDK line; fix it, do not weaken the job
7. **Report** the PR URL and the check status

## Guidelines

- One PR per logical change
- If the PR changes a public API, say so in the description and check whether the godoc, `README.md`, and `examples/` need the same change
- Never edit `.github/workflows/ci.yml` to make a check pass — the checks are the contract

$@
