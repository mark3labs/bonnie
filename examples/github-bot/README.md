# github-bot

A reference for BONNIE's GitHub channel. Mention the bot in an issue comment,
a pull request comment, or a review thread and it answers in the same thread.
The conversation is a durable run — stop the process mid-answer and the run is
still there when it starts again.

## Two shapes, one bonnie.New call

`main.go` here is a **library example**: a package inside BONNIE's own module,
so `go build ./...` compiles it and it never goes stale. Run it with `go run
./examples/github-bot`.

Your own agent is a **tree**: its own Go module from `bonnie init`, run with
`bonnie dev` and shipped with `bonnie build`. A tree cannot live inside this
repository — a nested Go module drops out of `go build ./...` and trips
`go fix` — so you scaffold it elsewhere (step 3). Its `main.go` is the same
`bonnie.New(...)` call you see here.

Use the tree for a live test: it is how a BONNIE agent is really written and
run.

## What you need

- A model key: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, or `GEMINI_API_KEY`.
- A tunnel. GitHub delivers a webhook over the internet, so the port must have
  a public URL.
- A private test repository to talk to, with an issue and a pull request in
  it.

## 1. Start a tunnel

```bash
ngrok http 8081
```

Copy the `https://….ngrok-free.app` URL (a reserved `--url` is steadier). Any
tunnel works — `cloudflared tunnel --url http://localhost:8081`, a `smee.io`
channel, a box with a public address. The bot mounts its webhook at
`/github/events`, so the full delivery URL is
`https://<tunnel>/github/events`.

Pick a port and keep it. `bonnie dev` walks to the next free port when the one
it wants is taken, and a walk silently breaks a tunnel that points at a fixed
port — so step 5 pins `--addr`, and this port must match.

## 2. Make the GitHub App by hand

There is no script for this: GitHub has no API that creates a GitHub App, only
a browser flow. Go to **Settings → Developer settings → GitHub Apps → New
GitHub App** (for an org, start at the org's settings), and fill in:

- **Name** — anything free across GitHub, e.g. `yourname-bonnie-bot`.
- **Homepage URL** — anything, e.g. `https://github.com/mark3labs/bonnie`.
- **Webhook → Active** — checked.
- **Webhook URL** — `https://<your-tunnel>/github/events`.
- **Webhook secret** — invent a strong string and keep it; it is
  `GITHUB_WEBHOOK_SECRET`.

**Repository permissions** — grant only what the channel uses:

| Permission | Level | Why |
|---|---|---|
| Issues | Read and write | Post comments; add the eyes reaction |
| Pull requests | Read and write | Reply in review threads; list changed files |
| Contents | Read-only | Read the patches in a pull request's diff |
| Metadata | Read-only | Mandatory; GitHub selects it for you |

**Subscribe to events** — check exactly these four:

- Issue comment
- Pull request review comment
- Issues
- Pull request

> GitHub gates each event behind a permission and rejects the whole form if
> one is uncovered. The four above are covered by the permissions in the
> table. `Check suite` is **not** — it needs a `Checks: read` permission, and
> this example has no `OnCheckSuite` hook to act on it, so leave it off. Add
> both together only if you write that hook.

**Where can this GitHub App be installed?** — "Only on this account".

Click **Create GitHub App**. Then, on the App's page:

- Note the **App ID** — it is `GITHUB_APP_ID`.
- Under **Private keys**, click **Generate a private key**. GitHub downloads a
  `.pem` file — that is `GITHUB_APP_PRIVATE_KEY`, in full.

## 3. Install the App on your test repository

On the App's page, open **Install App**, install it on your account, and
choose **Only select repositories** → your test repo.

## 4. Scaffold the agent tree

Outside this repository, because a tree is its own module:

```bash
bonnie init ~/bonnie-live-bot
cd ~/bonnie-live-bot
```

Put the channel in `main.go` — copy the `bonnie.New(...)` body from
[`main.go`](main.go) in this directory (the `WithGitHub` call, the `BotName`,
and the `OnIssue` hook if you want the proactive triage comment). Move the
system prompt into `instructions.md`; in a tree the prompt is a file, not a
`WithSystemPrompt` option.

**Testing a local change to `channel/github`?** Point the tree at your
checkout with an uncommitted replace, so the dev loop builds your edits and
not the released channel:

```bash
go mod edit -replace github.com/mark3labs/bonnie=/path/to/bonnie
go mod tidy
```

## 5. Run it

Load the credentials into the environment. The private key is many lines, so
read it from the file GitHub gave you rather than pasting it.

```bash
# bash, zsh
export GITHUB_APP_ID=123456
export GITHUB_APP_PRIVATE_KEY="$(cat ~/Downloads/your-app.private-key.pem)"
export GITHUB_WEBHOOK_SECRET='the-secret-you-invented'
```

```nu
# nushell — $(...) is not command substitution here; open the file instead
$env.GITHUB_APP_ID = "123456"
$env.GITHUB_APP_PRIVATE_KEY = (open --raw ~/Downloads/your-app.private-key.pem | decode utf-8)
$env.GITHUB_WEBHOOK_SECRET = "the-secret-you-invented"
```

Then, from the tree, start the dev loop with hot reload:

```bash
bonnie dev --addr 127.0.0.1:8081 --tui=false
```

Two flags matter, and the defaults are wrong for a webhook bot:

- `--addr` is **required**. Without it `bonnie dev` starts at `:8080` and walks
  `:8081`, `:8082`, … until it finds a free port; your tunnel points at one
  fixed port, so a walk silently breaks every delivery. The port here must be
  the one from step 1.
- `--tui=false` keeps the terminal for delivery logs. Drop it to chat with the
  bot directly instead.

To ship one static binary with no Go toolchain on the host, use `bonnie build`
and run `./bonnie-live-bot -addr 127.0.0.1:8081` instead.

Either way, a missing credential is a startup error that names the variable.
Confirm the channel is mounted:

```bash
curl -s localhost:8081/bonnie/v1/info
# {"agent":"github-bot","version":"…","channels":["http","github"]}
```

If `channels` does not list `github`, the binary you are running is not the
one you edited.

## 6. Talk to it

On your test repository:

| Test | What to do | What must happen |
|---|---|---|
| Issue | Comment `@bonnie what is a durable run?` on an issue | An eyes reaction, then a reply in the thread |
| Follow-up | Reply in that thread **with no mention** | The run continues; it remembers the question |
| Silence | Comment with no mention in a thread it never joined | Nothing. It is not its business |
| Pull request | Comment `@bonnie review this` on a PR | It answers with the diff in mind |
| Review thread | Mention it in a review comment on a line | A reply in that thread, as a separate run |
| New issue | Open a new issue | An unprompted triage comment, from `OnIssue` |
| Label trigger | Add the `agent-fix` label to an issue | The agent clones, branches, and works the change (needs a coding sandbox — see below) |
| Durability | `Ctrl-C` mid-answer, then start again | `bonnie runs list` shows the run |

Every run is in the journal, `.bonnie` inside the tree:

```bash
bonnie runs list --journal .bonnie
bonnie runs show --journal .bonnie github/<owner>/<repo>/issues/1
```

## Coding on a label

The `OnIssue` hook fires for **every** `issues` action, not only `opened`, so
the bot decides what to act on. This example triages a new issue and, when a
maintainer adds the `agent-fix` label, treats the issue as a request to write
the change.

Every issue and pull-request turn carries a **checkout descriptor** in its
context — the clone URL, the default branch, and a pull request's base and
head. It is public repository metadata, never a token, so it is safe in the
journal a replay re-injects. The agent clones from it with the `bash` tool and
branches from the default branch.

Two things the coding path needs that the default setup does not give:

- **A sandbox with network egress.** The default Landlock sandbox has none, so
  `git clone` cannot reach GitHub. Select a Docker sandbox and an allow-list
  network policy in `main.go`:

  ```go
  bonnie.WithSandbox(sandbox.Docker()),
  bonnie.WithNetwork(sandbox.NetworkPolicy{
      Mode:  sandbox.NetworkAllowList,
      Allow: []string{"github.com", "*.githubusercontent.com", "codeload.github.com"},
  }),
  ```

  The Docker image must have `git` installed.

- **Authenticated egress for a private repository.** A public repository
  clones over the token-free HTTPS URL the descriptor names. A private one
  needs a credential to reach the remote, and this example injects none — so
  it clones public repositories only. Pushing a branch and opening a pull
  request need the same authenticated egress, plus a `POST /pulls` call. That
  is a deliberately separate step, because putting a token where `git` can
  read it changes the channel's "the token never enters a run" guarantee.

## Notes

- **The bot never answers a bot.** The channel drops any delivery whose sender
  is a bot, so its own comments cannot start a new run.
- **A pull request and its review threads are separate runs.** The address of
  the timeline is `…/pulls/1`; a review thread adds `/reviews/<root-comment-id>`.
- **The App token never enters a run.** The channel mints an installation
  token per event, uses it to post, and drops it. A journal replay cannot leak
  it.
- `@bonnie` is a text token, not a GitHub mention. GitHub will not autocomplete
  it unless the App's slug happens to match.

## Clean up

```bash
rm -rf ~/bonnie-live-bot
```

Delete the App in its settings page, and delete or archive the test
repository.
