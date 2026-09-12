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

Everything under those two methods is shared, in `channel/chat`:

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

Enable it in the manifest:

```yaml
channels:
  slack: {}
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
  run is the answer.
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

Enable it in the manifest:

```yaml
channels:
  discord: {}
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
  topic).
- **Delivery:** `sendMessage`.

Setup: create the bot with BotFather, set the webhook with the same secret
you configure here (the URL must be HTTPS), then set:

| Variable | What |
|---|---|
| `TELEGRAM_BOT_TOKEN` | the bot token |
| `TELEGRAM_WEBHOOK_SECRET` | the `setWebhook` secret — required |
| `TELEGRAM_API_URL` | optional override; tests point it at a fake |

Enable it in the manifest:

```yaml
channels:
  telegram:
    username: mybot
```

---

## Secrets never live in the manifest

A channel's manifest key enables and configures it — the webhook path, the
command name, the bot username. The credentials come from the environment,
and their absence is a startup error that names the variable. The rule is
the invariant-10 ethic one level up: **a channel that cannot verify its
caller refuses to serve.** A webhook that does not check Slack's signature,
Discord's Ed25519, or Telegram's secret token is a door with no lock, and
`bonnie serve --agent` will not open one.

Verification is of the platform, not the person: a verified request says
the message came from Slack; the user ID inside it is Slack's word. The
`Principal` recorded on the run is platform-asserted identity, and
`SECURITY.md` says so.

## Not implemented, on purpose

- **Streaming edits.** eve's chat SDK posts an initial message and edits it
  as tokens arrive. BONNIE's chat channels deliver one message per turn —
  the NDJSON stream (the HTTP channel's `GET /runs/{id}/stream`) is the
  integration surface for live output.
- **Button-driven HITL** (Slack Block Kit actions, Discord message
  components). A parked run is answered in text. The resume path behind it
  is the same; only the gesture is missing.
- **Proactive sessions** — a bot that starts the conversation. The run
  creation path is the HTTP channel's job for now.
- **Attachments and files.** Text in, text out.
- **Gateway/Socket Mode transports.** Webhook only; both platforms' push
  transports need websocket dependencies BONNIE does not carry.
- **GitHub, Linear, and other eve channels.** The adapters above are the
  proof of the pattern; more of them are mechanical now, not structural.
