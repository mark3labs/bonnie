# Channels

BONNIE's inbound transports. The HTTP channel ships with the framework and
serves a JSON API; the chat channels — Slack, Discord, Telegram — put the
same durable runs into a conversation your team already has.

Read [`docs/SPEC.md`](SPEC.md) §1 (layers) and the [channel
contract](../channel/channel.go) first. This page covers what the chat
adapters share, what each platform does differently, and what is
deliberately not implemented.

---

## The contract

Every adapter implements the same two interfaces:

- **`channel.Channel`** — one webhook route it mounts on the `serve` mux.
- **`channel.Inbound`** — `From(address)` and `Attach(runID)`, the same
  surface the HTTP channel exposes.

An adapter that can also be handed work by another channel implements a
third, **`channel.Receiver`** — `Receive(ctx, target, text, opts)`. All
four platform adapters do; see "Hand-offs and proactive sessions" below.
A route handler receives both sides: `Inbound` for its own channel, and
`channel.Outbound` for the channels mounted beside it.

Everything under those interfaces is shared, in `channel/chat`:

- **Every event becomes one `chat.Turn`.** An adapter's only job on the
  way in is to turn a platform payload into `Turn{Address, Text, Context,
  Auth, Title, Kind}` and hand it to `chat.Dispatch`. `Text` is what the
  person said with the invocation token removed — the one thing that
  enters the conversation as a user message. `Context` is what the model
  should know for this turn only: who spoke, which event fired, the diff a
  comment refers to. It is shown to the model in front of the text and
  journalled as its own record (`RecordContext`), never as history; a
  resumed run replays the conversation without it. `Kind` names the
  surface — `dm`, `thread`, `channel`, `issue`, `pull_request`,
  `review_thread` — and is recorded with the channel's name as the run's
  origin, which the model is also told. `Title` names the run in `runs
  list`; an adapter that sets none gets the first line of the first
  message. This is eve's `{ message, context, auth, title }` dispatch
  result, as a Go struct.
- **The address map is journalled.** A chat address (a Slack thread, a
  Telegram chat, a Discord channel) binds to a run in a reserved run of the
  journal — the same mechanism `channel/http` uses for its address
  bookkeeping, and the reason bindings survive a restart. Reserved runs stay
  out of operator listings (SPEC §8, invariant 8).
- **Dispatch decides send versus resume.** A chat surface gives no way to
  say "this is a resume": the same thread carries starts, follow-ups, and
  answers to parked runs. The run's state decides — a waiting run is
  answered with `Resume`, anything else starts a turn with `Send`. The
  decision and the entry run under the per-run turn lock, so two platform
  messages cannot race it.
- **Delivery happens at the boundary.** A platform's webhook wants an
  acknowledgement in seconds; a turn can run for minutes. The adapter
  acknowledges at once, runs the turn in a goroutine the handler does not
  outlive (`context.WithoutCancel`), and posts the result when the turn
  reaches a boundary: a completed turn posts its response; a parked run
  posts its question; a failure posts a short error line. Silence helps
  nobody.
- **Steering is the default.** A second message while a turn runs is
  steered into it (the turn keeps its work); the steered message delivers
  nothing of its own, because the turn's first message delivers when the
  turn ends — posting both would answer one question twice. Queue semantics
  wait for the turn to finish. An unknown turn policy is refused, never
  guessed (SPEC §8).
- **Replies are plain text.** No `parse_mode`, no mrkdwn, no Block Kit —
  the model's markdown shows as written, and it cannot inject the
  platform's formatting. Long replies are split on line boundaries to the
  platform's limit, with a cap of five parts and a truncation notice.
- **Delivery is fire-and-log.** A failed post is logged to stderr and never
  retried in a loop. The run's result is in the journal; `bonnie runs show`
  reads it back. A delivery failure must not take the process down.

---

## Slack

- **Webhook:** `POST /slack/events` — Slack's Events API. Verify the v0
  signature (`X-Slack-Signature` over `v0:<timestamp>:<body>`, HMAC-SHA256
  with the signing secret); a timestamp older than five minutes is a replay
  and is refused. The `url_verification` handshake is answered with its
  challenge.
- **For the agent:** `app_mention` always; direct messages always (one
  conversation per DM); a threaded reply only when this channel bound that
  thread — a busy channel's other threads are not the agent's business.
  A mention or a thread reply is kind `thread`; a DM is kind `dm`. The
  sender and channel reach the model as context.
- **Dedup:** Slack retries when an endpoint is slow to acknowledge. Events
  are deduplicated by `event_id` before they reach the runner.
- **Delivery:** `chat.postMessage`, threaded to the mention's message.

Setup: create the app in the Slack API console, enable Event Subscriptions
with the webhook URL, subscribe to `app_mention` and `message.im`, install
to the workspace, then set:

| Variable | What |
|---|---|
| `SLACK_BOT_TOKEN` | the OAuth bot token (`xoxb-…`) |
| `SLACK_SIGNING_SECRET` | verifies that Slack sent the request |
| `SLACK_API_URL` | optional override; tests point it at a fake |

Mount it in `main.go`:

```go
bonnie.New(bonnie.WithSlack(slack.Config{})).Serve()
```

## Discord

- **Webhook:** `POST /discord/interactions` — Discord's Interactions
  Endpoint. Verify the Ed25519 signature (`X-Signature-Ed25519` over
  `X-Signature-Timestamp` plus the raw body); answer the `PING` handshake;
  acknowledge a command with a deferred response inside Discord's
  three-second deadline.
- **For the agent:** the slash command (default `/ask`, with a required
  string option named `message`) is the only invocation. Discord's webhooks
  see interactions, **not message traffic** — a plain reply in a channel
  never reaches a webhook. That is a property of the transport, not a
  limitation BONNIE chose: the gateway would see messages and needs a
  websocket dependency BONNIE does not carry.
- **One channel or thread is one conversation.** `/ask` again continues it;
  `/ask` while a turn runs steers that turn; `/ask <answer>` on a parked
  run is the answer. The kind is always `channel`: an interaction carries
  the channel ID and nothing that says whether it is a thread.
- **Delivery:** `POST /channels/{id}/messages` with the bot token — not the
  interaction token, which expires in fifteen minutes and would lose the
  reply of a long turn.

Setup: create the application and bot, register the command (a slash
command named `ask`, type 1, with a string option `message` — required),
set the webhook URL as the Interactions Endpoint URL, then set:

| Variable | What |
|---|---|
| `DISCORD_BOT_TOKEN` | the bot token |
| `DISCORD_PUBLIC_KEY` | the application's public key, hex |
| `DISCORD_API_URL` | optional override; tests point it at a fake |

Mount it in `main.go`:

```go
bonnie.New(bonnie.WithDiscord(discord.Config{})).Serve()
```

## Telegram

- **Webhook:** `POST /telegram` — Telegram's update webhook. Every request
  must carry the shared secret in `X-Telegram-Bot-Api-Secret-Token`
  (configure it in `setWebhook`); a wrong or missing secret is refused with
  `401`.
- **For the agent:** every text message in a private chat; in groups, the
  `/ask` command (`/ask@botname` to be sure) or a mention of the bot's
  username. Keep Telegram's privacy mode on: then the bot receives only
  commands and mentions, which is exactly what the adapter expects.
- **One chat is one conversation**; a forum topic is its own conversation
  (`message_thread_id` is part of the address, and the reply lands in the
  topic). A private chat is kind `dm`, a topic is `thread`, a group is
  `channel`.
- **Delivery:** `sendMessage`.

Setup: create the bot with BotFather, set the webhook with the same secret
you configure here (the URL must be HTTPS), then set:

| Variable | What |
|---|---|
| `TELEGRAM_BOT_TOKEN` | the bot token |
| `TELEGRAM_WEBHOOK_SECRET` | the `setWebhook` secret — required |
| `TELEGRAM_API_URL` | optional override; tests point it at a fake |

Mount it in `main.go`:

```go
bonnie.New(bonnie.WithTelegram(telegram.Config{Username: "mybot"})).Serve()
```

## GitHub

- **Webhook:** `POST /github/events` — GitHub App webhooks. Verify
  `X-Hub-Signature-256` (HMAC-SHA256 over the raw body with the webhook
  secret); a request without one is refused. Deliveries are deduplicated by
  `X-GitHub-Delivery`, so a redelivered webhook does not run the turn
  twice — which matters twice over, because a retried `/new` must not
  retire the run that replaced the one it meant.
- **For the agent:** a comment containing `@<bot>` — a text token, not a
  GitHub mention: GitHub may not autocomplete or link it, so the match is
  textual and case-insensitive. A comment in an already-bound thread
  continues it with no mention, the same rule as Slack. A comment that is
  neither is ignored: everything in a repository is not the agent's
  business. Bot senders are always dropped.
- **Addresses:** `octo/repo/issues/42` for an issue's timeline,
  `octo/repo/pulls/42` for a PR's timeline (GitHub posts PR timeline
  comments through the issues API, so the reply goes to
  `/issues/42/comments`), and `octo/repo/pulls/42/reviews/<root>` for one
  review thread — its own conversation, answered through
  `/pulls/42/comments/<root>/replies`.
- **Context:** the event, the sender, and whether the agent was mentioned
  are one line; a PR adds its title, base, head, and changed-file patches,
  with generated files (lock files, and anything in `ExcludedFiles`)
  contributing their name but not their patch. The whole block is capped
  (`MaxPatchBytes`, 32 KiB default). The diff is never conversation
  history — T-028's `RecordContext` carries it.
- **Delivery:** a comment on the surface the conversation lives on, split
  at GitHub's limit with a cap of five parts. A triggering comment gets an
  `eyes` reaction. A parked run posts its prompt as a comment; the next
  comment on the address answers it.
- **Opt-in events:** `OnIssue`, `OnPullRequest`, and `OnCheckSuite` hooks
  on the config return a turn or nil. `check_suite` turns must anchor to a
  pull request; a suite with none is dropped.
- **Credentials:** a GitHub App. The channel mints an installation token
  per event and uses it only to post and to fetch the PR context — the
  token never enters a run, a journal record, or a log line, and a test
  greps the journal to prove it.

Setup: create the GitHub App, point its webhook URL at
`<server>/github/events`, subscribe to `issue_comment`,
`pull_request_review_comment`, and — for the hooks — `issues`,
`pull_request`, and `check_suite`, then set:

| Variable | What |
|---|---|
| `GITHUB_APP_ID` | the App ID |
| `GITHUB_APP_PRIVATE_KEY` | the App's private key (PEM) |
| `GITHUB_WEBHOOK_SECRET` | verifies the webhook signature |
| `GITHUB_INSTALLATION_ID` | the installation a hand-off posts with; only a hand-off needs it, because a webhook carries its own |
| `GITHUB_API_URL` | optional override; tests point it at a fake |

Mount it in `main.go`:

```go
bonnie.New(bonnie.WithGitHub(github.Config{BotName: "my-agent"})).Serve()
```

---

## Hand-offs and proactive sessions

A channel can start a conversation on another channel. `out.To("slack")` in
a route handler returns the Slack adapter's receiver, and
`Receive(ctx, "C123", "summarise the incident", opts)` posts the
instruction there as a thread root, binds the thread's address **before**
the turn runs (so a platform event that arrives mid-turn continues this
run instead of racing it), and dispatches. The reply lands in the thread.
The options carry the initiating `Principal`, so the destination run
records who started it.

| Channel | Target | Surface the reply lands on |
|---|---|---|
| Slack | the channel ID | a new thread under the posted root message |
| Discord | the channel or thread ID | the channel |
| Telegram | the chat ID, or `<chat_id>/<topic>` | the chat or topic |
| GitHub | `github.Target{Owner, Repo, Number, PullRequest}` | a comment on the issue or PR timeline; needs `GITHUB_INSTALLATION_ID` |

The target has to name the surface exactly, because the address it derives
is the one a later platform event resolves to. GitHub numbers issues and
pull requests in one sequence and they have different addresses, so
`Target.PullRequest` says which; a hand-off that named the wrong one would
be answered by a second, separate run.

**A hand-off continues a conversation that is already there.** Only Slack
opens a fresh surface per hand-off; a Telegram chat, a Discord channel,
and a GitHub issue each have one stable address, and a second hand-off to
one of them joins the run already bound rather than replacing it —
replacing it would strand the run a person is talking to while both runs
kept delivering into the one surface. A caller that wants a clean
conversation resets the address first.

A hand-off is agent input, not a notification API. A caller that only wants
to post text calls the platform; a notification that must survive a crash
goes through an outbox of its own.

---

## Secrets never live in code

A channel's option enables and configures it — the webhook path, the command
name, the bot username. The credentials come from the environment, and their
absence is a startup error that names the variable. The rule is the
invariant-10 ethic one level up: **a channel that cannot verify its caller
refuses to serve.** A webhook that does not check Slack's signature,
Discord's Ed25519, or Telegram's secret token is a door with no lock, and
BONNIE will not open one.

Verification is of the platform, not the person: a verified request says
the message came from Slack; the user ID inside it is Slack's word. The
`Principal` recorded on the run is platform-asserted identity, and
`SECURITY.md` says so.

## Not implemented, on purpose

- **Streaming edits.** eve's chat SDK posts an initial message and edits it
  as tokens arrive. BONNIE's channels deliver one message per turn — the
  NDJSON stream (`GET /bonnie/v1/runs/{id}/stream`) is the integration
  surface for live output.
- **Button-driven HITL** (Slack Block Kit actions, Discord message
  components). A parked run is answered in text. The resume path behind it
  is the same; only the gesture is missing.
- **Proactive sessions** — done in T-033: see "Hand-offs and proactive
  sessions" above.
- **Message attachments.** Text in, text out. The GitHub channel fetches
  the PR diff itself, which is what a file-bearing surface needs before an
  attachment slot does.
- **Gateway/Socket Mode transports.** Webhook only; both platforms' push
  transports need websocket dependencies BONNIE does not carry.
- **Linear and other eve channels.** The GitHub adapter proves the pattern
  works for a non-chat surface; the next one is mechanical on top of it.
- **Framework HTTP namespace, session controls, idempotent start, stable
  error codes.** Done in T-030, T-032, and T-031: the HTTP channel serves
  under `/bonnie/v1/` and `/bonnie/` is reserved; `Reset`/`Clear`/`Compact`
  are on `SessionRef` and the HTTP channel, and `/new` works in every chat
  surface; `operation_id` makes a start idempotent, and every error body
  carries a stable `code`.
- **Cross-channel hand-off** (`to(channel).send`) — done in T-033; see
  "Hand-offs and proactive sessions" above. Schedules get the same surface
  when one exists.
