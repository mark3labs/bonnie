# slack-bot

A reference for BONNIE's Slack channel. Mention the bot in a channel, reply in
a thread it has joined, or send it a direct message, and it answers in the same
conversation. Each conversation is a durable run — stop the process mid-answer
and the run is still there when it starts again.

## This directory is an agent tree

This directory was made with `bonnie init slack-bot`. It is its own Go
module, and you run and manage it with the `bonnie` CLI, the same way as your
own agent:

| File | What it is |
|---|---|
| `instructions.md` | the system prompt |
| `main.go` | the `bonnie.New(...)` call: the Slack channel and its activity mode |
| `bonnie_gen.go` | the wiring `bonnie dev` and `bonnie build` regenerate; do not edit |
| `skills/`, `workspace/` | the agent's skills and its root for files, empty here |
| `go.mod`, `go.sum` | pin a released BONNIE |

## What you need

- The CLI: `go install github.com/mark3labs/bonnie/cmd/bonnie@latest`.
- A model key: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, or `GEMINI_API_KEY`.
- A tunnel. Slack's Events API delivers over the internet, so the port must
  have a public URL.
- A Slack workspace you can install an app into.

## 1. Start a tunnel

```bash
ngrok http 8081
```

Copy the `https://….ngrok-free.app` URL (a reserved `--url` is steadier). Any
tunnel works — `cloudflared tunnel --url http://localhost:8081`, a box with a
public address. The bot mounts its webhook at `/slack/events`, so the full
Request URL is `https://<tunnel>/slack/events`.

Pick a port and keep it. `bonnie dev` walks to the next free port when the one
it wants is taken, and a walk silently breaks a tunnel that points at a fixed
port — so step 5 pins `--addr`, and this port must match.

## 2. Make the Slack app by hand

There is no script for this. Go to <https://api.slack.com/apps> → **Create New
App** → **From scratch**, name it, and pick your workspace.

**OAuth & Permissions → Bot Token Scopes** — add:

| Scope | Why |
|---|---|
| `app_mentions:read` | See the mentions that invoke the bot |
| `chat:write` | Post replies, and the "working…" placeholder |
| `im:history` | Read direct messages sent to the bot |

**Basic Information → App Credentials** — copy the **Signing Secret**; it is
`SLACK_SIGNING_SECRET`. Set it before you go further: without it, anyone who
can reach the webhook drives the agent.

**Event Subscriptions** — toggle **Enable Events** on, and set the **Request
URL** to `https://<your-tunnel>/slack/events`. Slack immediately sends a
one-time `url_verification` handshake, so **the bot must already be running**
(step 5) for the URL to go green. Then, under **Subscribe to bot events**, add:

| Event | Why |
|---|---|
| `app_mention` | The bot answers when mentioned in a channel |
| `message.im` | The bot answers direct messages |

Save. Slack will prompt you to reinstall when scopes or events change.

## 3. Install the app and get the token

**OAuth & Permissions → Install to Workspace**, approve, and copy the **Bot
User OAuth Token** (starts `xoxb-`); it is `SLACK_BOT_TOKEN`.

Then invite the bot to a channel — in Slack, `/invite @your-bot` — or open a
direct message with it.

## 4. Get the tree

Use this directory as it is, or copy it to start your own bot from it:

```bash
cp -r examples/slack-bot ~/my-slack-bot
cd ~/my-slack-bot
```

Edit `instructions.md` to change what the bot says, and `main.go` to change
the `Activity` mode. `bonnie dev` picks up both.

**Testing a local change to `channel/slack`?** Point the tree at your checkout
with a replace, so the dev loop builds your edits and not the released
channel. Do not commit it: `go test ./examples/` refuses a replace in an
example.

```bash
go mod edit -replace github.com/mark3labs/bonnie=/path/to/bonnie
go mod tidy
```

## 5. Run it

Load the credentials into the environment:

```bash
# bash, zsh
export SLACK_BOT_TOKEN='xoxb-…'
export SLACK_SIGNING_SECRET='the-signing-secret'
```

```nu
# nushell
$env.SLACK_BOT_TOKEN = "xoxb-…"
$env.SLACK_SIGNING_SECRET = "the-signing-secret"
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

Start the bot **before** you set the Request URL in step 2, so Slack's
verification handshake finds it listening.

To ship one static binary with no Go toolchain on the host, use `bonnie build`
and run `./slack-bot -addr 127.0.0.1:8081` instead.

Either way, a missing credential is a startup error that names the variable.
Confirm the channel is mounted:

```bash
curl -s localhost:8081/bonnie/v1/info
# {"agent":"slack-bot","version":"…","channels":["http","slack"]}
```

If `channels` does not list `slack`, the binary you are running is not the one
you edited.

## 6. Talk to it

In your Slack workspace:

| Test | What to do | What must happen |
|---|---|---|
| Mention | `@your-bot what is a durable run?` in a channel | A "working…" note, then a reply in a thread |
| Follow-up | Reply in that thread **with no mention** | The run continues; it remembers the question |
| Silence | Post in a channel thread the bot never joined, no mention | Nothing. It is not its business |
| Direct message | DM the bot | It answers; one conversation per DM |
| Durability | `Ctrl-C` mid-answer, then start again | `bonnie runs list` shows the run |

Every run is in the journal, `.bonnie` inside the tree:

```bash
bonnie runs list --journal .bonnie
bonnie runs show --journal .bonnie slack/<channel-id>/<thread-ts>
```

## Notes

- **Webhook only.** Socket Mode (no public URL) is not implemented, so the
  tunnel is not optional.
- **Replies are plain text.** No Block Kit, no mrkdwn parsing: the model's
  markdown shows as written, which is why the prompt asks for a plain style.
- **The bot never answers a bot**, and it deduplicates Slack's timeout
  redeliveries by event ID, so a retried delivery cannot run the turn twice.
- **A parked run resumes in the thread.** When the agent asks a question, the
  prompt lands in the thread; the next reply there is the answer, not a new
  turn.
- **The activity indicator is best-effort.** `ActivityMessage` needs only
  `chat:write`; `ActivityStatus` needs Slack's AI assistant surface and the
  `assistant:write` scope, and is silently absent without it.

## Clean up

```bash
rm -rf .bonnie slack-bot
```

Delete the app at <https://api.slack.com/apps> → your app → **Delete App**.
