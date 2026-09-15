---
description: Fix bot review findings on the current PR, push, and poll until the review loop is clean
---

Resolve all automated review-bot findings (CodeRabbit and similar) on a pull request: verify each finding against the current code, fix the valid ones, push, wait for the bot's re-review, and repeat until no actionable comments remain. Extra context from the user: $@

## Identify the PR

- If the user input names a PR number, use it; otherwise:
  `gh pr view --json number,headRefName,state -q '{number: .number, branch: .headRefName, state: .state}'`
- If the PR is merged or closed, stop and tell the user
- If the working tree is dirty with unrelated changes, stop and suggest `/commit-push` first

## Fetch the findings

Pull **both** comment surfaces — bots use them differently:

1. **Review-level bodies**:

       gh pr view <pr> --json reviews --jq '.reviews[] | select(.author.login | test("coderabbit|copilot|bot")) | {state, body}'

2. **Line comments**:

       gh api repos/$(gh repo view --json nameWithOwner -q .nameWithOwner)/pulls/<pr>/comments --jq '.[] | select(.user.login | endswith("[bot]")) | "=== \(.path):\(.line) ===\n\(.body)"'

CodeRabbit line comments embed a `🤖 Prompt for AI Agents` block. Read the **full body** — severity markers, committable suggestions, and "Also applies to" lists all matter.

## Triage each finding — verify before fixing

- **Still valid** → fix it, minimal and scoped to the finding
- **Already addressed** → skip, note the commit that fixed it
- **Intentional behavior the bot misread** → skip and reply on the thread. This repo has intentional weirdness a reviewer will flag: the deliberate `var _ kit.SessionManager = (*Session)(nil)` tripwire, `context.WithoutCancel` for terminal bookkeeping, repair-on-restore rewriting history, and the fang styling in `cmd/bonnie`. Explain with a citation to the godoc that states the reason before dismissing
- **Wrong or out of scope** → skip with a brief reason; do not silently ignore

## Round-trip

1. Apply the fixes; run `task ci` — the PR's `test`, `lint`, and `boundary` checks must stay green
2. Commit and push (conventional message, no issue closure unless a finding maps to one)
3. Wait for the bot's re-review (`gh pr checks --watch`, then poll the review comments)
4. Repeat until no actionable comments remain

## Guidelines

- One finding, one fix — do not refactor beyond the comment
- Never merge or mark ready unless asked
- Report at the end: findings fixed, skipped (with reasons), and the final check state

$@
