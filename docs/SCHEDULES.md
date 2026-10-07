# Schedules

BONNIE schedules start durable agent turns from code-defined cron jobs. They do not create agent-owned jobs. There is no schedule manifest. Add definitions with `bonnie.WithSchedule` when you build the agent.

```go
bonnie.New(
    bonnie.WithSchedule(schedule.Definition{
        Name: "daily-report",
        Cron: "0 9 * * *",
        TimeZone: "Europe/London",
        Prompt: "Prepare today's operations report.",
    }),
).Serve()
```

Import `github.com/mark3labs/bonnie/schedule`. A definition needs a unique name, a five-field cron expression, and exactly one of `Prompt` or `Prepare`. Leave `Destination` empty to start a background run (the default). Set it to route work to a tracked channel. `Prepare(context.Context, schedule.Fire)` runs in the host, not in the agent sandbox. It returns dispatches and must be safe to repeat until BONNIE saves them. Use this to prepare input from trusted host data; do not treat it as an agent schedule API.

## Time and missed fires

Schedules use UTC when `TimeZone` is empty. Otherwise, use an IANA time zone such as `Europe/London`. Cron follows that zone's clock, including daylight-saving transitions. `CatchUp` is `"skip"` by default or `"latest"`. `latest` runs the latest missed cron occurrence, not every missed occurrence. It needs a prior cron occurrence in the journal; a new schedule with no cron history waits for the clock's next occurrence. `Overlap` is `"skip"` by default or `"allow"`. With skip, unfinished work blocks a new occurrence; a waiting run is unfinished work.

An occurrence records its definition `Revision`. If you change its revision while an old occurrence is unfinished, BONNIE fails that occurrence rather than executing it under changed instructions. Code changes without a revision change cannot be detected. Set and change revisions deliberately.

## Delivery and limits

BONNIE saves prepared dispatches, run results, and delivery progress. Delivery is **at least once**, not exactly once: after a successful channel post followed by a crash before its receipt is saved, the post can repeat. Failed delivery retries with a saved result and exponential backoff from 2 to 256 seconds; it does not start a fresh model turn. Platform thread creation can have a crash gap. Multipart delivery can create duplicates. Callback code must cooperate with context cancellation; BONNIE cannot stop a callback that ignores its context.

Channel dispatch requires a mounted tracked receiver. Slack, Telegram, Discord, and GitHub implement it. Slack opens a new thread with the public root `Scheduled task`, not the private agent prompt. Telegram, Discord, and GitHub keep the conversation already bound to their target. A hand-off runs the agent at the destination; it is not a plain notification. NATS and custom channels without the tracked capability are refused for schedule delivery. A schedule can still use the default background-run destination. Scheduled dispatches do not support files in this release. The engine limits execution to 16 active occurrence workers; excess accepted work stays in the journal. A callback must finish or return when its context is cancelled.

Only one scheduler can own a journal at a time. BONNIE uses a file lock in the journal directory and refuses a second scheduler. The cron clock is enabled by default in production. `bonnie dev` does not run it by default; use `bonnie dev --schedule-clock` to enable it for development. `WithScheduleClock` controls the clock in code.

Background runs use the normal agent factory, sandbox, tools, skills, and completion hooks. Human-input tools remain available. A background run that asks for approval stays waiting; use the run HTTP API to answer it. Nothing grants automatic approval. The default principal is `bonnie:app`, with authenticator `app` and kind `runtime`. A dispatch can supply an authorized principal explicitly. `Session.CurrentTrigger()` reports schedule provenance separately from identity and conversation origin.

## HTTP triggers and CLI

The schedule HTTP API is under `/bonnie/v1/schedules`: `GET` lists schedules, and `GET /{name}` returns its definition and history. A `POST /{name}/trigger` route is mounted only when `WithScheduleTriggerAuthorizer` is configured. That callback authorizes external/manual trigger requests; it is separate from and composes with the normal HTTP authenticator, which still applies. Protect the route with both as appropriate.

The POST JSON body accepts `id`, `scheduled_at` (RFC3339 time), and `kind`. External triggers must supply a stable `id` and `scheduled_at`, with `kind: "external"`. Manual triggers may send `{}`; BONNIE supplies the current UTC time and a generated ID. The API rejects unknown fields and extra JSON values.

Use the CLI to inspect and trigger schedules:

```sh
bonnie schedules list --url http://127.0.0.1:8080
bonnie schedules show daily-report --url http://127.0.0.1:8080
bonnie schedules history daily-report --url http://127.0.0.1:8080
bonnie schedules trigger daily-report --url http://127.0.0.1:8080
```

Add `--token TOKEN` when the HTTP authenticator requires a bearer token. External trigger example:

```sh
bonnie schedules trigger daily-report --kind external --id report-2026-10-07 --scheduled-at 2026-10-07T09:00:00Z
```

The external authorization callback must accept that request. The scheduled time and ID form the occurrence identity; reuse them to make a retry refer to the same occurrence.
