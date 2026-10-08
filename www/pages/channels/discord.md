---
title: Discord channel
description: Configure signed Discord interactions, slash commands, durable conversations, and approval buttons.
---

# Discord channel

Discord sends slash commands and button presses to `POST /discord/interactions`. BONNIE verifies the interaction, acknowledges it, and sends the result with the bot token through `POST /channels/{id}/messages`.

This adapter does not use the Discord Gateway. Normal messages and plain replies do not reach it.

## Agent setup

```go
package main

import (
    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/channel/discord"
)

func main() {
    bonnie.New(
        bonnie.WithDiscord(discord.Config{}),
    ).Serve()
}
```

```sh
export DISCORD_BOT_TOKEN='REPLACE_ME'
export DISCORD_PUBLIC_KEY='HEX_PUBLIC_KEY_FROM_DEVELOPER_PORTAL'
# Also set the model provider key.
bonnie dev --addr 127.0.0.1:8081 --tui=false
```

The root option fills empty `BotToken`, `PublicKey`, and `APIURL` from `DISCORD_BOT_TOKEN`, `DISCORD_PUBLIC_KEY`, and `DISCORD_API_URL`. Explicit values take precedence. Both credentials are required. Low-level `discord.New(runner, cfg, coreOptions...)` requires a valid 32-byte hex public key but permits no bot token; receive-only mode cannot send results.

| Config field | Default | Use |
| --- | --- | --- |
| `Command` | `ask` | Registered input command, without `/` |
| `CancelCommand` | `cancel` | Registered cancellation command; must differ from `Command` |
| `Path` | `/discord/interactions` | Webhook route |
| `APIURL` | `https://discord.com/api/v10` | API base; normally leave unset |

The application ID is needed to register commands. It is not a BONNIE config field or an environment fallback.

## Discord application setup

1. Create an application and bot in the Discord Developer Portal.
2. Copy the application's public key and bot token.
3. Install the application in a test server with `bot` and `applications.commands` scopes.
4. Give the bot access to the target channel and permission to send messages. For threads, also give it permission to send messages in threads. Do not grant Administrator for this adapter.
5. Register the commands below. BONNIE does not register them for you.
6. Start the server behind public HTTPS or a tunnel.
7. Set the Interactions Endpoint URL to `https://HOST/discord/interactions`. The server must be running to answer Discord's signed PING.

For a test guild, register each command with this API. Set `DISCORD_APPLICATION_ID` and `DISCORD_GUILD_ID` yourself; BONNIE does not read them.

```sh
curl -sS -X POST \
  "https://discord.com/api/v10/applications/$DISCORD_APPLICATION_ID/guilds/$DISCORD_GUILD_ID/commands" \
  -H "Authorization: Bot $DISCORD_BOT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"ask","description":"Send input to the agent","type":1,"options":[{"name":"message","description":"Instruction or answer","type":3,"required":true}]}'

curl -sS -X POST \
  "https://discord.com/api/v10/applications/$DISCORD_APPLICATION_ID/guilds/$DISCORD_GUILD_ID/commands" \
  -H "Authorization: Bot $DISCORD_BOT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"cancel","description":"Cancel the current turn","type":1}'
```

For global registration, omit `/guilds/$DISCORD_GUILD_ID`. If you change command names in config, change the registrations too. The input option must still be named `message` and contain a string. No Message Content intent is needed for this interaction-only transport.

## Conversations and controls

Use `/ask` with its `message` option. One channel or thread ID is one conversation. For channel `123456789`, the local address is `123456789` and the saved address is `discord/123456789`. A thread has its own ID and therefore its own run. All users in the same channel share that run; it is not a per-user session.

A later `/ask` continues the saved run after a restart. While work is active, it steers the turn by default. While the run waits for human input, it answers the current question. A plain Discord reply does neither.

Shared text controls use the input option, for example `/ask` with `message: /new`. Native `/cancel` sends the shared cancellation control for the current channel. It takes no target or turn-ID option. Repeated interaction IDs are suppressed in a bounded process-local cache, not a durable receipt. Cancellation is a request, not proof that work stopped. See [shared controls](/channels/overview#follow-ups-questions-and-controls).

## Questions and approval buttons

Approval prompts have Approve and Reject buttons. An option question can have up to 25 buttons, in five rows of five. The controls appear on the last reply part. Labels are shortened to the shared 80-byte limit. If any answer token exceeds the shared 64-byte limit, no buttons are offered; use `/ask`. A free-text question must also be answered with `/ask`.

Button tokens are resolved against the saved run's current suspension. An old control cannot answer a different question. A valid press returns interaction response type 7, with the question text retained and components removed, then resumes the run. A second answer is also checked by the runtime. Approval buttons supply a Boolean verdict, not just the button label.

For more than 25 choices, no buttons are rendered. Use `/ask` even if the delivered prompt has no text-answer hint. A button press is not an authorization check: anyone who can use the control must be treated as a possible respondent. The press path does not add a new user principal to the answer. Enforce approval policy in the host, not from the visible answer note alone.

## Verification and wire

Required headers are `X-Signature-Ed25519` and `X-Signature-Timestamp`. Discord signs the timestamp string followed directly by the raw request body. The signature is hex-encoded. Verification failure returns 401. Bodies are limited to 1 MiB; body-read failures return 400.

The adapter reads interaction `id`, `type`, `channel_id`, `data.name`, `data.options`, `data.custom_id`, guild `member.user` or DM `user`, and `message.content` for a button press. PING type 1 gets type 1. Accepted commands get deferred response type 5 within the interaction deadline. The adapter does not edit that deferred response or use interaction follow-up tokens. Results are separate channel messages, so delivery does not depend on the interaction token's 15-minute lifetime.

There is no timestamp-age check. Command deduplication is limited to 4,096 IDs and is lost at a restart or cache reset. Do not treat signature verification as durable replay protection.

Command input records principal authenticator `discord`, kind `user`, user ID, and attributes `username` and `channel_id`. This proves platform identity, not permission to run a sensitive tool. Restrict command access in Discord and add host authorization. Also protect the default [HTTP API](/channels/http#authentication-and-authorization).

## Delivery and limits

Replies use Discord message content, not embeds. The payload has no `allowed_mentions` restriction; model-generated mention text can notify users according to Discord's rules. Apply a host policy if that is not acceptable. Attachments are ignored, and there is no native activity indicator.

The current splitter uses a 2,000-byte budget per part and at most five parts. It is not a Unicode character-count guarantee. Excess text can be cut without a truncation notice. Ordinary post failures are logged; there is no durable retry queue for webhook replies. Inspect the saved result with `bonnie runs show --journal .bonnie RUN_ID`.

## Hand-offs and schedules

`Receive(ctx, "CHANNEL_OR_THREAD_ID", text, opts)` binds or continues that destination before it starts work. It does not create a thread or post a root instruction. The result is the visible message. A string channel or thread ID is also the tracked schedule target.

Tracked delivery returns post failures for retry of the saved result. A crash or multipart retry can repeat messages. See [scheduled channel work](/channels/overview#scheduled-channel-work).

## Source and tests

[Discord godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/channel/discord). Source: `channel/discord/discord.go` and `tracked.go`, plus shared `channel/chat/answer.go` and `chat.go`. Tests cover signatures, PING, commands, stale button presses, verdicts, cancellation, and tracked delivery in `discord_test.go`, `press_test.go`, `cancel_test.go`, and `tracked_test.go`.
