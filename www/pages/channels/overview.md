---
title: Channels
description: Connect durable BONNIE runs to HTTP, chat platforms, and NATS tasks.
---

# Channels

A channel receives input, maps a conversation to a durable run, and delivers the result. All channels use the same runner, instructions, tools, and sandbox. A channel does not give the agent extra sandbox permissions.

## Select a channel

| Channel | Input | Default endpoint | Conversation key |
| --- | --- | --- | --- |
| [HTTP](/channels/http) | JSON requests and NDJSON streams | `/bonnie/v1` | Caller-selected address or exact run ID |
| [Slack](/channels/slack) | Events API and a cancel slash command | `/slack/events` | Channel/thread timestamp, or channel/DM |
| [Discord](/channels/discord) | Slash commands and button interactions | `/discord/interactions` | Channel or thread ID |
| [Telegram](/channels/telegram) | Bot webhook messages | `/telegram` | Chat ID, optionally a forum topic |
| [GitHub](/channels/github) | GitHub App webhooks | `/github/events` | Issue, pull request timeline, or review thread |
| [NATS](/channels/nats) | Core NATS or JetStream tasks | No HTTP webhook | Core: task ID; JetStream: independent attempt |

`channel/chat` is shared code, not another network channel. It supplies address storage, turn locks, dispatch, text controls, and delivery helpers. The terminal command `bonnie chat` uses the HTTP channel.

## Mount channels

The root agent mounts HTTP by default. Add platform channels in the agent tree's `main.go`:

```go
package main

import (
    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/channel/slack"
)

func main() {
    bonnie.New(
        bonnie.WithSlack(slack.Config{}),
    ).Serve()
}
```

Set a model provider key and the channel credentials outside source code. Then run the tree:

```sh
bonnie dev --addr 127.0.0.1:8081 --tui=false
curl -s http://127.0.0.1:8081/bonnie/v1/info
```

Use a public HTTPS endpoint or a tunnel for platform webhooks. Set an explicit development address so the tunnel always points to the correct port. Build the tree with `bonnie build` for deployment.

The root options fill empty credential fields from environment variables. Explicit nonempty fields take precedence. Low-level adapter constructors do not do this environment setup. `WithSlack`, `WithDiscord`, and `WithTelegram` require both verification credentials and delivery tokens. Their low-level constructors permit a missing delivery token for receive-only use, but never a missing verification credential.

## Addresses and runs

An address is a channel-local conversation key, not a run ID. BONNIE prefixes it with the channel name before it saves the binding. For example, Slack thread `C123/1700000000.000900` has durable address `slack/C123/1700000000.000900`. The binding points to a separate run ID, usually `run-<random hex>`.

Bindings are saved in the journal and survive a restart. Use `From(address)` to follow the run that currently serves an address. Use `Attach(runID)` to select one exact run; an unknown ID does not create a run. HTTP addresses cannot select Slack bindings by adding `slack/` to their text: they are still in the HTTP namespace.

Text is conversation history. `Context` is model input for this turn only, saved separately from history. `Title` and conversation `Kind` describe the first turn. `Auth` carries the caller's principal.

Keep one live runner owner for each run. SQLite protects journal writes, but does not coordinate two servers that execute the same conversation. A saved `running` state after a crash does not mean execution continues automatically.

## Follow-ups, questions, and controls

Slack, Discord, Telegram, and GitHub use the shared chat dispatch rule:

- New conversation: start a run.
- Idle conversation: start another turn on the same run.
- Waiting conversation: resume it with the admitted reply.
- Active conversation: steer the active turn by default.

`steer` adds input to the active turn. `queue` waits for it to finish before processing the next message. Unknown policies are errors. HTTP exposes separate message and answer routes; NATS requires explicit answer messages, not repeated tasks.

Shared text controls match the whole trimmed message, without case sensitivity:

| Control | Effect |
| --- | --- |
| `/help` | List controls |
| `/cancel` | Request cancellation of active work or withdraw waiting input |
| `/new` | Retire the run and free the address; the next message creates a run |
| `/clear` | Remove conversation context, but keep the run, address, working files, and journal |
| `/compact` | Summarize older context if the agent supports it |

A platform must first admit the message. Discord uses `/ask /new` for a text control and has a native `/cancel` command. Telegram group messages still need the invocation except for native `/cancel`. Slack has a separate signed cancel slash-command route. HTTP message bodies are not a substitute for its explicit control routes.

Cancellation is a request, not proof that execution stopped. It is cooperative and cannot undo external effects. A later `cancelled` state confirms completion. A retired run stays readable but cannot start or resume.

Human-input tools are enabled by default. A waiting run holds no active model computation. Approval is not an automatic policy: the model must call `request_approval`, and other tools in the same model step can still run. Enforce sensitive-action policy in host tools too.

## Identity and access

Webhook checks prove the platform sent the request. They do not grant a repository role or make a group member an authorized approver. Shared conversations can have multiple speakers. Add admission and tool authorization rules for your deployment.

HTTP has no verifier by default. Configure `bonnie.WithHTTPAuthenticator` or protect it at the network boundary. An HTTP authenticator verifies identity; the channel does not supply a per-run ownership access policy. Do not expose the default HTTP API just because a platform webhook has a signature check.

NATS uses broker authentication and subject permissions. Its JSON payloads do not prove a publisher's identity. Keep credentials out of prompts, journals, logs, and sandbox environment settings.

Ordinary chat delivery is asynchronous and best-effort. A webhook ACK is not a completed turn or a durable delivery receipt. Delivery failures are logged; inspect the saved run with `bonnie runs list` and `bonnie runs show --journal .bonnie RUN_ID`.

## Scheduled channel work

Schedules are code-defined jobs, not another transport. Slack, Discord, Telegram, and GitHub support tracked schedule delivery. NATS does not support tracked schedule destinations. For agent dispatches, an empty destination starts a background run.

```go
// Import github.com/mark3labs/bonnie/schedule and mount Slack too.
bonnie.WithSchedule(schedule.Definition{
    Name: "daily-report",
    Cron: "0 9 * * *",
    TimeZone: "Europe/London",
    Revision: "1",
    Prompt: "Prepare today's operations report.",
    Destination: schedule.Destination{
        Channel: "slack",
        Target: "C0123456789",
    },
})
```

Definitions need a unique name, five-field cron expression, and exactly one of `Prompt`, `Prepare`, or `Run`. Time is UTC by default. `Run` has signature `func(context.Context, schedule.Fire) error` and starts no agent dispatches or channel deliveries. A nil error sets the occurrence to `Completed`; an error sets it to `Failed`. An empty `Prepare` result with no error sets it to `Skipped`.

`Prepare` and `Run` execute on the host, outside the sandbox. Both must respect context cancellation and be safe to repeat until their results are saved. Recovery uses the same stable `Fire.ID`; use it to prevent repeated external effects. BONNIE does not guarantee exactly-once execution. See [Scheduling](/guides/scheduling) for a host-only example.

Waiting work blocks new occurrences with the default overlap policy, `skip`. `CatchUp: "latest"` can run the latest missed occurrence when prior cron history exists; it does not run every missed occurrence.

Slack opens a new thread with public root text `Scheduled task`, not the private prompt. The other tracked adapters continue the destination's bound conversation. GitHub needs an installation ID for this use. Scheduled dispatches do not support files.

Delivery is at least once. BONNIE saves results and delivery progress; failed delivery retries the saved result with backoff, not a new model turn. A crash after a successful post but before saving its receipt can repeat the post. Thread creation and multipart posts also have crash gaps. Keep external effects safe to repeat.

Only one scheduler can own a journal; a file lock enforces this. The production clock is enabled by default. Development requires `bonnie dev --schedule-clock`. Change `Revision` when instructions change: an unfinished occurrence with a different revision fails rather than using the changed definition.

## Source and tests

Contracts: [channel godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/channel), [chat godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/channel/chat), and [schedule godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/schedule).

Source checks include `channel/chat/command_test.go`, `channel/chat/proactive_test.go`, `channel/chat/answer_test.go`, each adapter's webhook and tracked-delivery tests, and `schedule/recovery_test.go`. These cover controls, address continuity, stale answers, and recovery with a second runner.
