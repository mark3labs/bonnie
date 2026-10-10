---
title: Scheduling
description: Define durable cron work, inspect occurrences, and trigger BONNIE schedules with explicit retry and authorization rules.
---

# Scheduling

BONNIE schedules start durable agent turns or host callbacks from code-defined cron jobs. The operator defines the jobs; the model does not create or edit them. There is no schedule manifest.

A schedule occurrence is not the same as a conversation. One occurrence can prepare several dispatches. Each dispatch starts or continues agent work, with its own saved result and delivery progress.

## Define a background job

Put this in an agent tree's `main.go`. Keep the agent's general instructions in `instructions.md`:

```go
package main

import (
    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/schedule"
)

func main() {
    bonnie.New(
        bonnie.WithSchedule(schedule.Definition{
            Name:     "daily-report",
            Cron:     "0 9 * * *",
            TimeZone: "Europe/London",
            Revision: "1",
            Prompt:   "Read the supplied working files and prepare today's operations report.",
            CatchUp:  "skip",
            Overlap:  "skip",
        }),
    ).Serve()
}
```

Every definition needs:

- A unique, nonempty name with no slash or backslash.
- A five-field cron expression: minute, hour, day of month, month, day of week.
- Exactly one of `Prompt`, `Prepare`, or `Run`.
- A valid IANA time zone, or an empty `TimeZone` for UTC.

For example, `0 9 * * *` is daily at 09:00 in the configured zone, and `*/15 * * * *` is every 15 minutes. Do not add a seconds field. Zone rules include daylight-saving changes; local clock times can be absent or repeated. Use UTC when you need a schedule independent of local clock changes. Built binaries include IANA zone data.

For agent dispatches, an empty `Destination.Channel` starts background work. It uses the normal factory, sandbox, tools, skills, and completion hooks. Inspect its run through the run API or CLI. It does not publish a plain notification automatically.

## Choose missed-fire and overlap behavior

| Field | Value | Effect |
|---|---|---|
| `CatchUp` | Empty or `skip` | Do not dispatch missed cron occurrences |
| `CatchUp` | `latest` | Dispatch the latest missed occurrence, not every missed one |
| `Overlap` | Empty or `skip` | Skip a new occurrence while earlier work is unfinished |
| `Overlap` | `allow` | Permit overlapping occurrences |

`latest` requires a prior cron occurrence in the journal. A new schedule with no cron history waits for its next clock occurrence. It does not create an initial backlog.

A waiting run counts as unfinished work. With overlap skipped, an unanswered approval can block later occurrences. Do not use `allow` to avoid approval management without checking cost, shared resources, and external effects.

Set `Revision` deliberately. BONNIE records it in an occurrence. If the definition revision changes while an old occurrence is unfinished, BONNIE fails that occurrence rather than running it under changed instructions. Code changes without a revision change cannot be detected. Review unfinished work before you deploy a new revision.

## Prepare dynamic input

Use `Prepare` instead of `Prompt` when trusted host code must read data or choose dispatches. This example option produces one background dispatch:

```go
// Import context, runtime, and schedule from their public packages.
bonnie.WithSchedule(schedule.Definition{
    Name:     "daily-check",
    Cron:     "0 8 * * *",
    TimeZone: "UTC",
    Revision: "1",
    Prepare: func(ctx context.Context, fire schedule.Fire) ([]schedule.Dispatch, error) {
        if err := ctx.Err(); err != nil {
            return nil, err
        }
        return []schedule.Dispatch{{
            Key: "check",
            Input: runtime.Input{
                Text:  "Check the working files for missing records.",
                Title: "Daily check " + fire.ScheduledAt.Format("2006-01-02"),
            },
        }}, nil
    },
}),
```

`Prepare` runs on the host, not in the sandbox. It must be safe to repeat if the process stops before its dispatches are saved. `Fire.ID` is stable across retries. Use stable keys that are unique within the occurrence. An empty dispatch list with no error sets the occurrence to `Skipped`. Scheduled dispatches do not support files in this release.

Use trusted data sources, timeouts, and context-aware requests. Do not perform a non-idempotent external action in preparation. BONNIE cannot stop a callback that ignores its context.

The default identity is `bonnie:app`, with authenticator `app` and kind `runtime`. A dispatch can supply an authorized principal with `Auth`. Identity does not replace schedule provenance: `Session.CurrentTrigger()` reports the trigger separately from conversation origin and identity. Never construct privileged identity from unverified model input.

## Run host-only work

Use `Run` instead of `Prompt` or `Prepare` for work that needs no agent. Its signature is `func(context.Context, schedule.Fire) error`. This option creates a directory without a model call:

```go
// Import context, os, path/filepath, bonnie, and schedule.
bonnie.WithSchedule(schedule.Definition{
    Name:     "daily-directory",
    Cron:     "0 9 * * *",
    Revision: "1",
    Run: func(ctx context.Context, fire schedule.Fire) error {
        if err := ctx.Err(); err != nil {
            return err
        }
        return os.MkdirAll(filepath.Join("reports", fire.ID), 0o750)
    },
})
```

`Run` starts no agent dispatches and makes no channel deliveries. A nil error sets the occurrence to `Completed`; an error sets it to `Failed`. A saved failure is not retried. In contrast, `Prepare` with no dispatches and no error produces `Skipped`.

Both callbacks run on the host, outside the agent sandbox. They must respect context cancellation and be safe to repeat. Recovery can repeat a callback if its result was not saved, with the same stable `Fire.ID`. Use that ID to prevent repeated external effects. BONNIE does not guarantee exactly-once execution.

## Dispatch to a channel

A destination must be a mounted channel with the tracked receiver capability. Slack, Telegram, Discord, and GitHub implement it. NATS and custom channels without that capability are refused as schedule destinations.

For example, mount Slack and select its channel target:

```go
// Import channel/slack and schedule; add inside bonnie.New(...).
bonnie.WithSlack(slack.Config{}),
bonnie.WithSchedule(schedule.Definition{
    Name:     "team-report",
    Cron:     "0 9 * * 1-5",
    TimeZone: "UTC",
    Revision: "1",
    Prompt:   "Prepare the team's daily operations report.",
    Destination: schedule.Destination{
        Channel: "slack",
        Target:  "C0123456789",
    },
}),
```

Set `SLACK_BOT_TOKEN` and `SLACK_SIGNING_SECRET` on the host and grant the bot access to the channel. Consult the adapter's target contract for other platforms. `Target` must be JSON-serializable, and adapters must accept the generic JSON shape after replay.

Slack opens a new thread with the public root text `Scheduled task`, not the private prompt. Telegram, Discord, and GitHub keep the conversation already bound to their target. A dispatch runs the agent at the destination; it is not simply a message containing a saved report.

## Start and inspect schedules

The compiled agent's cron clock is on by default. `WithScheduleClock(false)` disables cron evaluation through `Agent.Run(ctx)`. In the current `Agent.Serve()` implementation, the operator flag overrides this option even when omitted; use `--schedule-clock=false` to disable the clock. Dev turns the clock off by default to prevent accidental background actions:

```bash
bonnie dev --schedule-clock
bonnie schedules list --url http://127.0.0.1:8080
bonnie schedules show daily-report --url http://127.0.0.1:8080
bonnie schedules history daily-report --url http://127.0.0.1:8080
```

The read API is:

- `GET /bonnie/v1/schedules`: definitions.
- `GET /bonnie/v1/schedules/{name}`: definition and occurrence history.

The normal HTTP authenticator applies when configured. Supply `--token "$BONNIE_API_TOKEN"` to the CLI when it needs a bearer token. Protect inspection endpoints too: operational history can contain private data.

Only one scheduler can own a journal. The root serving API holds a file lock in the journal directory and refuses a second scheduler. Do not remove the lock file to try to permit two servers. Low-level `schedule.Engine` hosts must enforce exclusive journal ownership themselves.

## Enable authorized triggers

The POST trigger route exists only when you configure `WithScheduleTriggerAuthorizer`. This callback is separate from the normal HTTP authenticator; both checks apply when configured. Use it to enforce which callers can trigger which jobs. Do not use a callback that always succeeds on an exposed endpoint.

After you configure authorization, a manual trigger can use:

```bash
bonnie schedules trigger daily-report \
  --url http://127.0.0.1:8080 --token "$BONNIE_API_TOKEN"
```

Manual triggers may send `{}`. BONNIE supplies a generated ID and the current UTC time. For an external scheduler, supply a stable ID and RFC3339 time:

```bash
bonnie schedules trigger daily-report \
  --url http://127.0.0.1:8080 --token "$BONNIE_API_TOKEN" \
  --kind external --id report-2026-10-08 \
  --scheduled-at 2026-10-08T09:00:00Z
```

The equivalent API route is `POST /bonnie/v1/schedules/daily-report/trigger` with:

```json
{"kind":"external","id":"report-2026-10-08","scheduled_at":"2026-10-08T09:00:00Z"}
```

External triggers require both `id` and `scheduled_at`. Reuse both for a retry of the same occurrence. Unknown fields and extra JSON values are rejected. A new manual trigger is new work, not a retry key for a prior occurrence.

## Handle waiting work and delivery retries

Human-input tools remain enabled for background jobs. An approval request leaves the run waiting; nothing grants automatic approval. Inspect the run, then answer through the run HTTP API. Use the current turn identity for delayed responses. Waiting work can block new occurrences under the default overlap policy.

BONNIE saves prepared dispatches, run results, and delivery progress. Delivery is **at least once**, not exactly once. A crash after a successful post but before saving its receipt can repeat the post. Platform thread creation and multipart delivery also have duplicate risks.

Failed delivery retries the saved result with exponential backoff from 2 to 256 seconds. It does not start another model turn. Preparation or execution failure does not automatically get a fresh agent retry. The engine limits active occurrence executions to 16; excess accepted work remains in the journal.

Monitor schedule history, waiting runs, failed occurrences, and repeated delivery errors. Make callbacks and downstream effects safe to repeat. See [Deployment](/guides/deployment), [Troubleshooting](/guides/troubleshooting), and the [schedule godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/schedule).
