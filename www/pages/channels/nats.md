---
title: NATS channel
description: Configure Core NATS and JetStream tasks, worker-routed answers, status queries, cancellation, and broker security.
---

# NATS channel

The NATS channel receives JSON tasks from a broker. It has no HTTP webhook. Core NATS is a process-local, best-effort transport. JetStream retains input and uses at-least-once delivery with a local durable result cache. Neither mode guarantees exactly-once execution of tools or external actions.

## Select the mode

| Property | Core NATS | JetStream |
| --- | --- | --- |
| Enable | Leave `Stream` and `RootSubject` empty | Set `Stream` or `RootSubject` |
| Task routing | Ordinary subscription; each server receives a copy | Shared durable pull consumer |
| Input retention | None | Broker stream, within its retention limits |
| Results | Core publish and connection flush | Journal result, broker-confirmed publish, then input ACK |
| Answers | Literal `AnswerSubject` | `AnswerSubject + "." + WorkerID` |
| Versions | 0 or 1 | 1 only |
| Recovery | Explicit; input or output can be lost | Cached result reuse or a fresh independent attempt |
| Status and cancel protocol | Not supported | Optional; enabled by root-derived subjects |

NATS tasks are not chat follow-ups. An answer must use the answer protocol; another task does not answer a parked run.

## Core NATS agent setup

```go
package main

import (
    "github.com/mark3labs/bonnie"
    natschannel "github.com/mark3labs/bonnie/channel/nats"
)

func main() {
    bonnie.New(
        bonnie.WithNATS(natschannel.Config{
            Subject: "ops.tasks",
            AnswerSubject: "ops.answers",
            ResultSubject: "ops.results",
        }),
    ).Serve()
}
```

```sh
export NATS_URL='nats://127.0.0.1:4222'
# Also set the model provider key.
bonnie dev --addr 127.0.0.1:8081 --tui=false
```

Start a NATS server before the agent. In separate terminals, subscribe to results before publishing a task:

```sh
nats --server "$NATS_URL" sub ops.results
nats --server "$NATS_URL" pub ops.tasks \
  '{"version":1,"task_id":"review-42","text":"Review the staging plan.","context":["Do not deploy."]}'
```

These commands assume the NATS CLI and local broker access. Configure broker authentication for production.

## Connection and lifecycle

`bonnie.WithNATS` fills empty connection fields from `NATS_URL`, `NATS_NKEY_SEED`, `NATS_TOKEN`, `NATS_USERNAME`, and `NATS_PASSWORD`. Explicit nonempty fields win. Set only one authentication method, including in the environment: a token, a user NKey seed, or username/password. A password requires a username; an empty password is permitted. `NKeySeed` is the secret seed value, not a path.

An owned connection accepts one server URL with scheme `nats`, `tls`, `ws`, or `wss`. It must have a host and no path, query, or fragment. URL credentials cannot be combined with authentication fields. Use encrypted transport in production.

For JWT credentials, custom TLS settings, or other connection options, create an authenticated `*nats.Conn` with the NATS Go client and supply `Config.Conn`. Do not also supply `URL` or authentication fields. The root option skips all connection environment fallbacks when `Conn` is supplied. The host owns that connection; the adapter does not replace its handlers or close it on shutdown.

Low-level `natschannel.New(runner, cfg)` validates config without connecting. `Start(ctx)` connects and establishes subscriptions before it returns. The root agent manages this lifecycle when you use `WithNATS`. The channel can start only once. `Shutdown(ctx)` stops admission, cancels active work, and waits; a deadline ends the caller's wait, not the background shutdown. A later call can wait again. Transport failures are logged and returned by shutdown. An owned connection is closed when shutdown finishes.

## Task and result API

`Task` contains `version`, `task_id`, `text`, and optional `context` (array of strings). Text must not be blank. Context is saved separately from conversation history. JSON decoding rejects unknown fields and extra JSON values. Input is limited to 1 MiB, also subject to the broker's smaller payload limit.

Task and other protocol IDs must be nonempty, at most 256 bytes, have no leading or trailing whitespace or control characters, and not start with `nats/`. Subject names are literal: no wildcards, whitespace, control characters, backslashes, or empty dot-separated tokens. Protocol subjects must not overlap.

A `Result` contains:

| Field | Meaning |
| --- | --- |
| `task_id` | Caller correlation ID; can be empty for malformed input |
| `run_id` | Saved run, when one exists |
| `state` | Run boundary state, such as `waiting`, `completed`, or `failed` |
| `response` | Agent result text, when available |
| `suspend` | Current question or approval, when waiting |
| `error` | Failed execution or rejected input |
| `version` | 1 for JetStream; normally omitted in Core output |
| `attempt_id` | JetStream independent attempt identity |
| `worker_id` | Owning JetStream worker |
| `answer_subject` | JetStream worker answer route |

Do not infer success only from `state`; check `error` too. A validation error can have no run ID or state. All task and answer results go to the configured `ResultSubject`, never `Msg.Reply` or a publisher-supplied destination. `reply_to` is not a supported JSON field.

## Core behavior and answers

Each task creates an independent run bound to local address `task_id`, saved as `nats/TASK_ID`. A duplicate ID is rejected if a binding exists or the process-local 4,096-entry deduplication cache remembers it. This does not return the earlier result. Existing bindings survive restart, but failed admission cache entries do not. Do not reuse IDs for new work.

Core subscriptions are not queue subscriptions. Two BONNIE servers on the same subjects can each execute the task. Use one Core owner, or use JetStream for shared-worker routing. Do not concurrently mutate NATS-owned tasks through `From(taskID)` or another runner.

To answer a Core waiting result, copy its `suspend.tool_call_id` and send:

```sh
nats --server "$NATS_URL" pub ops.answers \
  '{"version":1,"task_id":"review-42","tool_call_id":"CURRENT_TOOL_CALL_ID","responses":[{"turn_id":"CURRENT_SUSPEND_TURN_ID","text":"Use staging only.","approved":true}]}'
```

`responses` uses `runtime.InputResponse`: `text`, optional `turn_id`, and optional `approved`. Omit `approved` for a text question. Explicit `false` is rejection; absence is not rejection. Include the current suspension turn ID when available. The adapter requires a waiting run, matching tool-call ID, and a nonempty response list. Unknown tasks and stale answers produce error results. Core does not use `run_id`, `worker_id`, or `message_id` to route or deduplicate answers.

Core input and output can be lost if no subscriber is present, buffers overflow, the broker disconnects, or shutdown interrupts publication. The result flush confirms connection progress, not subscriber receipt. There is no durable result-publication retry. A saved task is not automatically resumed after a crash.

## JetStream setup

Enable JetStream on the broker, for example with `nats-server -js` for local testing. Give it a persistent storage directory for deployment. Replace the Core config with:

```go
natschannel.Config{
    RootSubject: "ops.agent",
    WorkerID: "worker-1",
    CreateStream: true,
}
```

This derives the complete protocol:

| Config field | Derived value |
| --- | --- |
| `Subject` | `ops.agent.tasks` |
| `ResultSubject` | `ops.agent.results` |
| `AnswerSubject` | `ops.agent.answers` |
| `EventSubject` | `ops.agent.events` |
| `CommandSubject` | `ops.agent.commands` |
| `QuerySubject` | `ops.agent.queries` |

Explicit nonempty subjects override derived values. Setting a root enables JetStream; it cannot select Core mode. A root also fills empty status/control bases. For JetStream without those features, omit the root and set `Stream` plus task, answer, and result subjects explicitly.

`WorkerID` is required in JetStream. Keep it stable, unique among live workers, and attached to the same local journal across restarts. Stream, consumer, and worker config names must contain only letters, digits, and hyphens, at most 128 bytes. Do not run two processes with the same worker identity and journal. A worker ID identifies ownership; it is not an authentication credential.

`DefaultConsumerName(Subject)` returns `bonnie-` plus the full SHA-256 hex digest of the task subject. Workers on the same stream and subject share that durable task consumer unless you set `Consumer`. Different consumer names are separate processing groups and can each execute retained tasks.

The input stream name is derived with `DefaultInputStreamName(Subject)` when a root is set. Output names use `DefaultResultStreamName(ResultSubject)` and `DefaultEventStreamName(EventSubject)`. You can override `Stream`, `ResultStream`, and `EventStream`.

### Broker resources

`CreateStream: true` permits creation of a missing input stream. With a root it also permits creation of the result stream; an enabled event subject permits creation of the event stream. Created streams use file storage and limits retention. Existing resources are never changed. Configure retention, disk limits, replicas, and access rules as an operator; automatic creation is not a production capacity policy.

The input stream must explicitly list the task subject and `AnswerSubject + ".*"`, with limits retention. If targeted tasks are enabled, it must also list `Subject + ".worker.*"`. A broad `>` entry alone does not satisfy these checks. Root-derived result and event streams must explicitly contain their output subject and use limits retention.

Without a root, the result stream is operator-owned: provision a stream that retains `ResultSubject`. Results require a JetStream publication ACK. A Core-only result subscriber cannot replace that stream. If no result stream exists, publication fails and input stays unacknowledged.

Missing pull consumers are created independently of `CreateStream`. The shared task consumer filters `Subject`; the answer consumer filters the owning worker's answer route and has name `Consumer + "_" + WorkerID`. Consumers use explicit ACK, deliver-all, instant replay, default 30-second ACK wait, unlimited deliveries, and default `MaxAckPending` equal to concurrency. Existing consumers must be compatible and are explicitly bound, never changed or deleted by this adapter. Finite `MaxDeliver`, backoff, push delivery, multiple filters, headers-only delivery, and inactivity expiry are rejected. See source for all compatibility checks before pre-provisioning consumers.

### Publish and observe

```sh
# Subscribe before sending; use a durable result consumer for replay after disconnect.
nats --server "$NATS_URL" sub ops.agent.results
nats --server "$NATS_URL" sub ops.agent.events

# Publish input. This Core publish is retained when a stream covers the subject,
# but this command does not wait for a JetStream storage ACK.
nats --server "$NATS_URL" pub ops.agent.tasks \
  '{"version":1,"task_id":"review-42","text":"Review the staging plan."}'
```

For broker-confirmed input publication, use the NATS Go client's JetStream API (or an equivalent client):

```go
// nc is your authenticated *nats.Conn; ctx is the host's context.
js, err := nc.JetStream()
if err != nil {
    return err
}
_, err = js.Publish("ops.agent.tasks",
    []byte(`{"version":1,"task_id":"review-42","text":"Review the staging plan."}`),
    nats.Context(ctx), nats.MsgId("review-42"))
if err != nil {
    return err
}
```

The JetStream publish ACK is input storage confirmation, not the agent result. Results arrive on `ops.agent.results`. The `sub` examples show live observation only; they do not replay retained output. Production clients must create or bind their own durable result/event consumers and deduplicate repeated output.

## JetStream attempts and recovery

A task creates a random `attempt_id` and independent run at local address `js/ATTEMPT_ID`, not a chat binding at `task_id`. Correlate work with `task_id`, `attempt_id`, `run_id`, and `worker_id`, not task ID alone. Publishing the same task again as a new stream message can start another attempt. Use broker publication deduplication with a stable `Nats-Msg-Id` when appropriate, but its deduplication window is finite.

The adapter sends in-progress heartbeats at one third of the effective ACK wait while it reads the journal, waits for locks, executes, and publishes. It saves a bounded result locally before publishing it with a stable `Nats-Msg-Id`. Only after the broker confirms the result does it synchronously acknowledge input. A waiting result also completes this input-delivery transaction; an explicit answer is a separate message.

If the same input is redelivered to the same worker with its journal, a cached result is republished without another model turn. A crash after execution but before saving that result leaves an uncertain attempt. Task redelivery starts a fresh independent attempt, not automatic continuation of the uncertain run. Another worker with a separate journal can also execute the task again. Tool effects can repeat. Preserve journals and make external operations safe to repeat.

Result publication or ACK failure leaves input eligible for redelivery. Stream retention limits can still expire or remove input. Broker deduplication can suppress repeated output only within its configured window. This is not distributed exactly-once execution or delivery.

## JetStream answers

Copy the worker route and identities from the waiting result. Check `answer_subject` against your configured answer base and expected worker before publishing; do not publish to an arbitrary subject received in JSON.

```sh
nats --server "$NATS_URL" pub ops.agent.answers.worker-1 \
  '{"version":1,"message_id":"answer-42-1","task_id":"review-42","run_id":"RUN_ID_FROM_RESULT","worker_id":"worker-1","tool_call_id":"CURRENT_TOOL_CALL_ID","responses":[{"turn_id":"CURRENT_SUSPEND_TURN_ID","text":"Proceed in staging.","approved":true}]}'
```

For a storage ACK, publish the answer with the JetStream client API shown above, using the answer route and JSON instead. All identities in this example must match the waiting result. `message_id` is a stable answer identity: retry the same logical answer with the same ID. Do not reuse it for a different answer. The adapter checks the owning worker, saved task/run association, current waiting state, and tool-call ID. The attempt ID stays the same when an answer resumes it.

Accepted answer content and suspension are journalled before resume. A retry uses that saved content, not changed input. A saved answer result is republished without resume. If resume completed before its result was saved, the adapter can recover the completed or next-waiting snapshot. Failed, cancelled, or interrupted admitted resumes are not automatically repeated: the result reports that safe automatic continuation is unavailable. Inspect the run and resolve external effects before manual recovery.

## Status events, queries, and cancellation

These are JetStream-only features. A root enables all three subjects; without a root, configure each needed base explicitly.

`StatusEvent` contains `version: 1`, the four target identities, stable `event_id`, `type`, journal cursor `seq`, `time`, and `state`. Types are `task_accepted` and `run_state`. Acceptance has sequence zero. Events are read from durable journal records, not the live activity bus. They contain no prompt, tool arguments, response text, or model deltas. Deduplicate by event ID. Publication is at least once, including after restart with the same worker and journal.

Queries and cancellation commands use Core NATS request/reply on worker routes, even in JetStream mode. They are not retained commands:

```sh
nats --server "$NATS_URL" request ops.agent.queries.worker-1 \
  '{"version":1,"task_id":"review-42","attempt_id":"ATTEMPT_ID","run_id":"RUN_ID","worker_id":"worker-1"}'

nats --server "$NATS_URL" request ops.agent.commands.worker-1 \
  '{"version":1,"task_id":"review-42","attempt_id":"ATTEMPT_ID","run_id":"RUN_ID","worker_id":"worker-1","turn_id":"OBSERVED_TURN_ID"}'
```

All four `Target` fields are required and must match a locally admitted attempt. A control request needs a valid `_INBOX.` reply subject. Other reply routes are ignored. The command is cancellation; there is no separate action field. Optional `turn_id` protects against cancelling a later turn.

`Status` includes the target, `state`, process-local `active`, `seq`, optional current `suspend`, `turn_id`, cancellation fields, and `error`. A saved `running` state with `active: false` means interrupted work, not active execution. `cancel_status` can be `requested`, `not_active`, or `stale`. Requested cancellation is not completion; it cannot undo external effects. A transport timeout is not proof that a cancel request failed. Check status again.

## Targeted tasks

Set `TargetedTasks: true` to also accept tasks on `Subject + ".worker." + WorkerID`, for example `ops.agent.tasks.worker.worker-1`. JetStream is required. Existing input streams must already list `Subject + ".worker.*"`; the adapter does not update them. Each worker has a separate stable targeted pull consumer. Targeted routing is useful when the chosen journal must own the task, but does not make side effects exactly once.

## Presence registry

Worker discovery is opt-in and separate from the task protocol. Use `bonnie.WithPresence` with the JetStream KV store in `github.com/mark3labs/bonnie/presence/nats`. Use the same `WorkerID` in the presence and channel configurations. A presence bucket defines discovery scope independently of task streams and consumers; enabling a NATS channel does not create a registry.

The channel advertises its task subject, or its worker-targeted subject when `TargetedTasks` is enabled. Readiness follows the connection state and becomes false when the channel stops. Discovery does not submit tasks or guarantee that a selected worker can execute them. Expiry must not trigger automatic reassignment. See [Deployment](/guides/deployment#advertise-worker-presence) for KV creation, TTL, ownership, and watch behavior.

## Resource and security limits

- `Concurrency` defaults to 4 and must be 1–1,024. JetStream workers fetch one message when an execution slot is free and alternate task and answer consumers.
- `Buffer` defaults to 64 and must be 1–65,536. It bounds Core pending messages per subscription and the work queue. Core can drop excess messages. It is not a JetStream retention setting.
- JSON input is capped at 1 MiB. JetStream result JSON is bounded by that limit and by broker/stream limits, with 512 bytes reserved for headers and framing. If too large, the transport returns `state: "failed"`, `error: "result too large"`, and omits response and suspension. This does not change the journalled run state. Suspensions are never truncated for delivery. Inspect the saved run if needed.
- Tasks and answers have no `auth` field and create no verified publisher principal. Broker authentication and subject ACLs are the trust boundary. A payload's `worker_id` is a route, not proof of identity.
- Restrict task publishers, answer publishers, cancellation callers, and result/status readers separately. Results and query replies can contain private text or questions even though status events do not.
- Workers need the required JetStream API, pull-consumer, ACK, publication, subscription, and request-inbox permissions. Pre-provision resources if workers must not create streams or consumers. Do not grant broad broker administration merely to make setup succeed.
- Keep NKey seeds, tokens, passwords, and credential files out of prompts, journals, source, and sandbox environment settings. A supplied authenticated connection remains a host resource.
- NATS has no shared chat text controls: `/cancel` in task text is model input, not a transport cancellation command. It also has no file-upload protocol, proactive `Receiver`, or tracked schedule destination. Protect the separately mounted [HTTP API](/channels/http#authentication-and-authorization).

## Source and tests

[NATS godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/channel/nats). Source: `channel/nats/nats.go`, `jetstream.go`, `subjects.go`, `status.go`, and `options.go`. Tests cover strict decoding, broker authentication, NKey validation, Core limits, consumer compatibility, shared routing, independent attempts, interrupted answer recovery, payload limits, targeted tasks, status replay, cancellation, and lifecycle shutdown. Key files include `nats_test.go`, `jetstream_test.go`, `jetstream_attempt_test.go`, `jetstream_review_test.go`, `subjects_test.go`, `status_test.go`, and `lifecycle_test.go`.
