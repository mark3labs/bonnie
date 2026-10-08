---
title: Slack channel
description: Configure signed Slack webhooks, durable threads, human input, and activity messages.
---

# Slack channel

Slack uses the Events API, not Socket Mode. BONNIE receives signed events at `POST /slack/events` and replies through `chat.postMessage` with the bot token.

## Agent setup

```go
package main

import (
    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/channel/slack"
)

func main() {
    bonnie.New(
        bonnie.WithSlack(slack.Config{Activity: slack.ActivityMessage}),
    ).Serve()
}
```

```sh
export SLACK_BOT_TOKEN='xoxb-REPLACE_ME'
export SLACK_SIGNING_SECRET='REPLACE_ME'
# Also set the model provider key.
bonnie dev --addr 127.0.0.1:8081 --tui=false
```

Empty config credentials come from these variables. Nonempty config values take precedence. The root option requires both. Low-level `slack.New` requires a signing secret but permits no bot token; that mode cannot deliver replies.

`Path` overrides `/slack/events`. `CancelPath` defaults to `Path + "/cancel"`. `APIURL`, or the root option's `SLACK_API_URL` fallback, overrides `https://slack.com/api`; normally leave it unset.

## Slack app and permissions

1. Create a Slack app for the workspace.
2. Add bot token scopes below.
3. Install the app and obtain its bot token and signing secret.
4. Start BONNIE behind public HTTPS or a tunnel.
5. Enable Event Subscriptions and set Request URL to `https://HOST/slack/events`.
6. Subscribe to the needed bot events and reinstall when scopes change.
7. Invite the bot into each channel it must use.

| Bot scope | Use |
| --- | --- |
| `app_mentions:read` | Invocation mentions |
| `chat:write` | Replies and default activity placeholder |
| `im:history` | Direct messages |
| `channels:history` | Public-channel thread replies |
| `groups:history` | Private-channel thread replies |
| `assistant:write` | Only for `ActivityStatus` on Slack's assistant surface |

Subscribe to `app_mention`, `message.im`, `message.channels`, and `message.groups` for those surfaces. Mention-only setup cannot receive mention-free follow-ups in channel threads. The server must be running when Slack sends URL verification.

For native cancellation, register a Slack slash command with Request URL `https://HOST/slack/events/cancel` and the app's command scope, `commands`. Its visible command name is your Slack registration; the handler uses its form fields, not its name.

## Dispatch and addresses

| Event | Admission | Channel-local address |
| --- | --- | --- |
| `app_mention` | Always, unless bot/subtype filtering rejects it | `<channel>/<thread_ts>`; root message `ts` starts a thread |
| `message`, `channel_type: "im"` | Every normal text DM | `<channel>/dm` |
| Threaded `message` | Only if the thread is already bound | `<channel>/<thread_ts>` |
| Unbound thread without mention | Ignored | None |

The durable form adds `slack/`. A DM is `slack/D123/dm`, not just the channel ID. Replies to DMs are not threaded. Any event with `bot_id` or nonempty `subtype` is ignored, including edits. Slack mention tokens `<@USER_ID>` are removed from mention input.

A follow-up uses the same saved binding after a restart. Run IDs are separate; use `bonnie runs list --journal .bonnie` to find them. A waiting run posts its prompt; the next admitted text in the same conversation answers it. Slack approval buttons are not implemented: answers are text.

Use exact text controls such as `/new`, `/clear`, `/compact`, and `/help` after the message passes admission. In the Slack UI, a leading slash can invoke a platform command instead of sending text. A mention such as `@your-bot /new` produces the shared control after mention removal. Native cancellation avoids this ambiguity.

## Native cancel

In a public or private channel, supply the exact thread timestamp:

```text
/your-cancel-command 1700000000.000900
```

The signed form must carry `channel_id`, `user_id`, `text`, and `trigger_id`. An empty `text` is allowed only for a DM channel whose ID starts with `D`. A nonempty target must be a numeric `seconds.fraction` timestamp. Duplicate trigger IDs are suppressed in the process so a retry cannot cancel later work. The response reports cancellation requested or no active turn; it is not completion confirmation.

## Wire and verification

The adapter reads this event shape; Slack supplies and signs it:

```json
{
  "type": "event_callback",
  "event_id": "Ev123",
  "event": {
    "type": "app_mention",
    "text": "<@U_BOT> Review the deployment plan.",
    "ts": "1700000000.000900",
    "channel": "C123",
    "user": "U123"
  }
}
```

Other read fields are top-level `challenge` and event `thread_ts`, `channel_type`, `bot_id`, and `subtype`. `url_verification` returns `{"challenge":"..."}` after verification. Event callbacks are acknowledged with 200 before agent execution.

Required headers are `X-Slack-Signature` and `X-Slack-Request-Timestamp`. The signature is `v0=` plus HMAC-SHA256 over `v0:<timestamp>:<raw body>`. Modified bodies fail verification with 401. The implementation rejects timestamps older than five minutes; it does not separately reject future timestamps. Keep clocks correct and do not alter signed bodies in a proxy. Bodies are capped at 1 MiB.

`event_id` deduplication is bounded and process-local. A restart or cache reset forgets IDs. It is not durable exactly-once delivery. The principal records authenticator `slack`, kind `user`, the event's user ID, and channel attribute. The signature proves Slack sent it, not that the user may run a sensitive tool or answer an approval. Add host authorization rules.

## Activity and delivery limits

- `ActivityMessage` is the default: one placeholder in the thread, edited and deleted before the final reply.
- `ActivityStatus` uses Slack's assistant status API; it needs that surface and `assistant:write`.
- `ActivityOff` disables the indicator.

Activity is best-effort. It can include reasoning and tool argument labels; decide whether that information is suitable for the conversation. Replies are plain text with markdown parsing disabled, not Block Kit. Attachments and files are ignored.

The current splitter uses a 39,000-byte part budget and at most five parts. Do not rely on a character-count guarantee or a truncation marker; excess text can be cut. Ordinary post failures are logged, not handled by a durable retry queue. The saved run remains available for inspection.

## Hand-offs and schedules

`Receive(ctx, "C123", text, opts)` opens a new thread by posting the instruction, binds its address before execution, then runs the agent there. It is not a plain notification. A refused root post is returned as an error and must not start a turn.

Slack also implements tracked schedule delivery. Use a string channel ID as the schedule target. Scheduled threads use the public root `Scheduled task`, not the private prompt. Saved results are retried on delivery failure; posts can repeat after a crash. See [schedules](/channels/overview#scheduled-channel-work).

## Source and tests

[Slack godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/channel/slack). Setup permissions are also shown in `examples/slack-bot/README.md`. Source: `slack.go`, `cancel.go`, `activity.go`, and `tracked.go`. Tests cover signatures, stale timestamps, DMs, bound threads, duplicate events, activity, refused hand-offs, cancel targets, and tracked delivery in `channel/slack`.
