---
title: Telegram channel
description: Configure Telegram bot webhooks, secret verification, private chats, groups, and forum topics.
---

# Telegram channel

Telegram sends bot updates to `POST /telegram`. BONNIE checks the shared webhook secret, acknowledges the update, and sends results through the Bot API's `sendMessage` method. Long polling with `getUpdates` is not implemented.

## Agent setup

```go
package main

import (
    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/channel/telegram"
)

func main() {
    bonnie.New(
        bonnie.WithTelegram(telegram.Config{Username: "your_bot"}),
    ).Serve()
}
```

```sh
export TELEGRAM_BOT_TOKEN='123456789:REPLACE_ME'
export TELEGRAM_WEBHOOK_SECRET='REPLACE_WITH_A_RANDOM_SECRET'
# Also set the model provider key.
bonnie dev --addr 127.0.0.1:8081 --tui=false
```

The root option requires both credentials. Empty `Token`, `Secret`, and `APIURL` fields come from `TELEGRAM_BOT_TOKEN`, `TELEGRAM_WEBHOOK_SECRET`, and `TELEGRAM_API_URL`. Explicit nonempty fields take precedence. Low-level `telegram.New(runner, cfg, coreOptions...)` requires `Secret` but permits no `Token`; it can receive input but cannot send replies.

Set `Username` to the bot username without `@`. There is no username environment fallback or automatic `getMe` discovery. `Command` defaults to `ask`, without `/`. `Path` defaults to `/telegram`. `APIURL` defaults to `https://api.telegram.org`; normally leave it unset.

## Bot and webhook setup

1. Create a bot with BotFather and save the bot token.
2. Set `Username` to the username BotFather assigned.
3. Generate a separate webhook secret. Telegram accepts 1–256 characters from letters, digits, `_`, and `-` for `secret_token`; the adapter checks only that its configured secret is nonempty.
4. Start BONNIE behind public HTTPS or a tunnel.
5. Register the webhook with the same secret:

```sh
curl -sS -X POST \
  "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/setWebhook" \
  --data-urlencode 'url=https://HOST/telegram' \
  --data-urlencode "secret_token=$TELEGRAM_WEBHOOK_SECRET" \
  --data-urlencode 'allowed_updates=["message"]'

curl -sS "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/getWebhookInfo"
```

Use the configured `Path` if you changed it. Bot API URLs contain the token: protect shell history, proxy logs, and monitoring output. Do not publish the URL with a real token.

For groups, add the bot and keep privacy mode enabled unless it must receive more traffic. Privacy mode controls which messages Telegram delivers; BONNIE's admission rules still apply. Use `/ask@your_bot` for an explicit group invocation. Registering command descriptions with BotFather or `setMyCommands` can improve the UI, but it is not required by the handler. Give only the chat permissions needed to send replies, including forum-topic access where applicable.

## Admission and addresses

| Surface | Admitted input | Local address |
| --- | --- | --- |
| Private chat | Every nonempty text message from a non-bot sender | `<chat_id>` |
| Group or supergroup | Leading `/ask`, matching `/ask@your_bot`, or an entity mention of the configured username | `<chat_id>` |
| Forum topic | Same group invocation rule, with `message_thread_id` | `<chat_id>/<topic_id>` |
| Channel posts | Not supported | None |

Saved addresses add `telegram/`, for example `telegram/-1001234567890/42`. A normal group without forum topics has one shared conversation. A message reply ID is not a separate conversation key. Bindings survive a restart.

Matching leading commands are removed from input. A mention admits a group message but is not removed from its text. Username and command matches are case-sensitive. The mention implementation indexes text by bytes, although Telegram entity offsets use UTF-16 units. Mentions after non-ASCII text can fail admission; a leading `/ask@your_bot` avoids dependence on those offsets.

Only the top-level `message` update is read. Edited messages, callback queries, channel posts, attachments, and bot-authored messages are ignored. The adapter does not read or deduplicate `update_id`. A webhook retry can therefore produce another input or cancellation request.

## Questions and controls

A waiting run sends its question as text. The next admitted text on the same chat or topic answers it. There are no inline approval buttons. In a group, an ordinary reply without the invocation is ignored even if the bot already has a binding. Use `/ask@your_bot ANSWER`.

Shared controls can be sent directly in a private chat, or through the invocation in a group, for example `/ask@your_bot /new`. Native `/cancel` and `/cancel@your_bot` work in private chats, groups, and supergroups without `/ask`. They require no trailing text, and a targeted command must match `Username`. Cancellation selects the current chat or topic; it has no turn-ID guard. Repeated updates are not suppressed. See [shared controls](/channels/overview#follow-ups-questions-and-controls).

The result can say that cancellation was requested. This does not confirm that external work stopped or undo an action.

## Verification and identity

Every request must carry `X-Telegram-Bot-Api-Secret-Token` equal to `Secret`. The comparison is constant-time. A missing or incorrect header returns 401. This is a shared secret check, not a signature over the body or a timestamp check. Anyone with the secret can submit an update with a claimed user ID. Keep HTTPS and the secret secure.

The JSON body reader is capped at 1 MiB. Verified requests are acknowledged with 200 and body `ok`, including malformed or ignored updates. An ACK is not a durable input receipt or completed turn.

Admitted input records principal authenticator `telegram`, kind `user`, decimal sender ID, and attributes `username` and `chat_id`. This is not an approval authorization policy. Users of a shared chat can answer its waiting run. Add host checks for sensitive actions, and protect the default [HTTP API](/channels/http#authentication-and-authorization).

## Delivery and limits

`sendMessage` receives `chat_id`, `text`, and, for topics, `message_thread_id`. No `parse_mode` is set: replies are plain text, with model markdown shown as written. There is no activity indicator or file delivery.

The current splitter uses a 4,096-byte part budget and at most five parts, not a Unicode character-count guarantee. Excess text can be cut without a truncation notice. Ordinary delivery logs HTTP failures and does not have a durable retry queue. It does not separately validate an `ok: false` body returned with HTTP 2xx. The tracked schedule path does validate Telegram's `ok` field. The durable run remains available for inspection.

## Hand-offs and schedules

`Receive(ctx, "CHAT_ID", text, opts)` or `Receive(ctx, "CHAT_ID/TOPIC_ID", text, opts)` binds or continues that conversation before execution. Targets must be strings with numeric chat and topic components, not Go integer values. No root instruction is posted; the result is the visible message.

Use the same string target for tracked schedules. Delivery failures retry the saved result, but a crash or multipart retry can repeat posts. See [scheduled channel work](/channels/overview#scheduled-channel-work).

## Source and tests

[Telegram godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/channel/telegram). Source: `channel/telegram/telegram.go` and `tracked.go`, with `options.go` for environment defaults. Tests cover secret verification, private and group admission, topics, bot filtering, cancellation, hand-off continuity, and tracked delivery in `telegram_test.go`, `cancel_test.go`, `handoff_test.go`, and `tracked_test.go`.
