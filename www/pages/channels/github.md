---
title: GitHub channel
description: Configure a GitHub App, signed webhooks, comment admission, event hooks, and durable issue and PR conversations.
---

# GitHub channel

The GitHub channel is a GitHub App, not a personal-access-token bot. It receives signed webhooks at `POST /github/events`, obtains installation tokens, and replies with comments. Issue conversations, pull request (PR) timelines, and review threads have separate saved addresses.

## Agent setup

```go
package main

import (
    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/channel/github"
)

func main() {
    bonnie.New(
        bonnie.WithGitHub(github.Config{BotName: "operations-bot"}),
    ).Serve()
}
```

```sh
export GITHUB_APP_ID='123456'
export GITHUB_APP_PRIVATE_KEY="$(cat /secure/path/app.private-key.pem)"
export GITHUB_WEBHOOK_SECRET='REPLACE_WITH_A_RANDOM_SECRET'
# Also set the model provider key.
bonnie dev --addr 127.0.0.1:8081 --tui=false
```

`BotName` is a code setting, without `@`. It has no environment fallback. Empty `AppID`, `PrivateKey`, `WebhookSecret`, and `APIURL` come from `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`, and `GITHUB_API_URL`. The root option requires all three credentials. `PrivateKey` is the PEM content, not a file path; PKCS#1 and PKCS#8 RSA keys are supported. Use the numeric App ID, not a client ID.

A zero `InstallationID` comes from `GITHUB_INSTALLATION_ID` if it is set. This is required for hand-offs and schedule delivery, not normal webhook replies. Explicit config values take precedence. Low-level `github.New(runner, cfg)` does not fill environment fields and requires a bot name, webhook secret, and parseable private key.

`APIURL` defaults to `https://api.github.com`. For GitHub Enterprise Server, set the correct REST API root, usually `https://HOST/api/v3`. There is no `Path` config field; the route is `/github/events`.

## GitHub App setup

1. Open account or organization Settings → Developer settings → GitHub Apps → New GitHub App.
2. Set the homepage URL and enable webhooks.
3. Set Webhook URL to `https://HOST/github/events`, with public HTTPS or a tunnel to the fixed development port.
4. Set a strong webhook secret, identical to `GITHUB_WEBHOOK_SECRET`.
5. Grant the repository permissions needed below and subscribe to the required events.
6. Create the app, record its App ID, and generate an RSA private key.
7. Install the app on selected repositories. An app registration alone does not grant installation access.
8. Start BONNIE and post `@operations-bot Review this plan.` in an issue or PR comment.

| Repository permission | Level | Use |
| --- | --- | --- |
| Issues | Read and write | Issue and PR timeline comments; comment reactions |
| Pull requests | Read and write | PR details and changed files; review-thread replies |
| Metadata | Read-only | Required app metadata access |
| Checks | Read-only, optional | Check suite events when `OnCheckSuite` is used |
| Contents | Optional, read-only | Host tools that read or clone repository contents; not a token grant to the sandbox |

Subscribe to **Issue comment** and **Pull request review comment** for the basic comment bot. Add **Issues**, **Pull request**, or **Check suite** only when you configure their hooks. The checked-in example also grants Contents read-only, but the channel's PR diff calls use the pull request API. It does not clone a repository itself.

Use GitHub's Recent deliveries view to inspect webhook errors and test redelivery. Verify the mount with `GET /bonnie/v1/info`, but keep the default HTTP API private or authenticated.

## Admission and conversation keys

The default rule admits a human comment if it mentions the bot or continues an already bound conversation. The invocation is a case-insensitive, word-bounded text token `@BotName`; it need not be first. GitHub need not autocomplete or link it. All matching tokens are removed from model input.

| Surface | Address helper | Local address example |
| --- | --- | --- |
| Issue | `AddressIssue(owner, repo, number)` | `acme/service/issues/42` |
| PR timeline | `AddressPullRequest(owner, repo, number)` | `acme/service/pulls/43` |
| Review thread | `AddressReview(owner, repo, number, rootCommentID)` | `acme/service/pulls/43/reviews/9001` |

Saved addresses add `github/`. PR timeline replies use GitHub's issue-comment API, but the BONNIE binding uses `/pulls/`, not `/issues/`. A review reply uses `in_reply_to_id` as the root; an initial review comment uses its own ID. The timeline and each review thread are separate runs.

Senders whose type is `Bot` or whose login ends in `[bot]` are ignored. Comment handling is based on the payload objects, not on an `X-GitHub-Event` switch. It does not filter comments to action `created`; edited or deleted comment deliveries can also reach admission. Select subscriptions and host gates with this behavior in mind.

An admitted follow-up continues the run after a restart, steers active work by default, or answers a waiting question. Questions and approvals are text-only; there are no native approval buttons. Shared controls work after admission and invocation removal, for example `@operations-bot /cancel` or `@operations-bot /new`. A mention-free `/cancel` also works in a bound thread. See [shared controls](/channels/overview#follow-ups-questions-and-controls).

## Restrict comment admission

A signed webhook does not prove that a commenter has a repository role or may approve a deployment. By default, any admitted human commenter can affect the shared conversation. `OnComment` replaces the default admission rule, rather than adding another check:

```go
github.Config{
    BotName: "operations-bot",
    OnComment: func(c github.CommentCtx) bool {
        allowed := c.Sender == "trusted-operator"
        return allowed && (c.Mentioned || c.Bound)
    },
}
```

`CommentCtx` contains `Owner`, `Repo`, `Number`, `Kind`, `Sender`, raw `Body`, `Mentioned`, and `Bound`. It has no repository-role or comment-action field. Use a host-owned allowlist or another verified role source where needed. Apply the gate to answers and controls too. Enforce sensitive-tool policy independently of model instructions.

Comment turns record principal authenticator `github`, kind `user`, sender login, and repository and number attributes. Non-comment event hooks do not automatically get that principal; if needed, the hook must set `chat.Turn.Auth` from a host-defined identity policy. Protect the default [HTTP API](/channels/http#authentication-and-authorization) separately.

## Optional event hooks

`OnIssue`, `OnPullRequest`, and `OnCheckSuite` return `*chat.Turn`; nil ignores the event. The hook supplies `Text` and any additional context. The channel fills `Kind`, adds sender-event context and a repository checkout descriptor, and supplies a default address when applicable.

```go
// Also import github.com/mark3labs/bonnie/channel/chat.
github.Config{
    BotName: "operations-bot",
    OnIssue: func(c github.IssueCtx) *chat.Turn {
        if c.Action != "labeled" || c.Label != "needs-triage" {
            return nil
        }
        return &chat.Turn{Text: "Triage this issue: " + c.Title}
    },
}
```

- `IssueCtx` includes repository, number, title, action, sender, state, body, current labels, changed label, assignees, and full event JSON in `Raw`.
- `PullRequestCtx` includes repository, number, title, action, sender, state, body, draft flag, labels, changed label, base/head refs, head SHA, and `Raw`.
- These two hooks see every action delivered for their event. Filter explicitly; do not assume only `opened` or `labeled`.
- `CheckSuiteCtx` includes suite ID, conclusion, head SHA, PR numbers, app slug, action, and sender. The hook is called only for action `completed`. A suite without an associated PR is ignored even if the hook returns a turn. The default address is the first associated PR.

Hook turns do not automatically fetch the comment path's PR diff. Add only the context your host policy permits. Return a deliverable issue or PR address if you override it.

## Context and credentials

Comment text enters conversation history. Event metadata and fetched PR details enter per-turn `Context`, saved separately from history. The checkout descriptor names the clone URL, default branch, and available PR base/head metadata. It does not provide clone credentials or automatically copy repository files into the sandbox.

For a PR comment, the channel calls `GET /repos/{owner}/{repo}/pulls/{n}` and `/pulls/{n}/files?per_page=100`. It reads only that first files page. Missing patches and generated or excluded files are listed without their patch. API failures can omit the context without failing the comment turn.

`MaxPatchBytes` defaults to 32 KiB. The implementation budgets patch bytes, not the whole formatted context: titles, filenames, and labels can add more. A patch can be cut at a byte boundary. `ExcludedFiles` adds basename or suffix matches, such as `.min.js`; it is not a glob list. Common lock files, including `go.sum`, are excluded by default. Do not assume the supplied context is a complete diff.

The adapter signs an RS256 app JWT and calls `POST /app/installations/{id}/access_tokens`. The installation token is used for API calls, not inserted into prompts or journal records. Webhooks supply their installation ID. For private-repository tool access, configure a separate secure host mechanism; never add tokens to model context.

## Verification and delivery limits

`X-Hub-Signature-256` must equal `sha256=` plus the HMAC-SHA256 of the raw body with `WebhookSecret`. Failure returns 401. Bodies are limited to 1 MiB; read failures return 400. Verified requests get 200 before agent work. Malformed or ignored payloads can therefore also get 200.

There is no timestamp check. `X-GitHub-Delivery` deduplication is process-local, resets at 4,096 entries, and is lost at restart. A missing delivery ID is accepted without deduplication. This is not durable exactly-once admission.

Timeline results use `POST /repos/{owner}/{repo}/issues/{n}/comments`; review results use `/pulls/{n}/comments/{root}/replies`. GitHub renders the comment's markdown. Treat repository text and model-generated content as untrusted. Ordinary failures are logged, not retried by a durable outbox. The triggering comment gets a best-effort eyes reaction; it is not completion confirmation.

The splitter uses a 63,488-byte budget per comment and at most five parts. It is not a character-count guarantee. Excess text can be cut without a truncation notice. Attachments are not transferred.

## Hand-offs and schedules

`Receive(ctx, target, text, opts)` requires a typed target:

```go
github.Target{Owner: "acme", Repo: "service", Number: 43, PullRequest: true}
```

Set `PullRequest: true` for a PR timeline, or its follow-ups will bind a different run. `InstallationID` must be configured and cover the target repository. This API binds or continues the timeline before execution; it does not post a root instruction. Review-thread proactive targets are not supported by `github.Target`.

The same target is supported for tracked schedule delivery. Its saved receipt includes the installation ID so delivery can continue after restart. Failed posts retry the saved result; crashes and multipart retries can repeat comments. See [scheduled channel work](/channels/overview#scheduled-channel-work).

## Source and tests

[GitHub godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/channel/github). Source: `channel/github/github.go`, `tracked.go`, and `options.go`. The agent-tree example is `examples/github-bot`. Tests cover signatures, invocation matching, PR and review addresses, gates and hooks, context limits, cancellation, hand-off continuity, and tracked delivery in `github_test.go`, `cancel_test.go`, `handoff_test.go`, and `tracked_test.go`.
