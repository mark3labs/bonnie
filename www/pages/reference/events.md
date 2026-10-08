---
title: Events
description: Runtime event types, journal cursors, live-only Kit activity, replay limits, and activity logging.
---

# Events

An event is one observable moment in a run. HTTP sends one JSON object per line as NDJSON. The journal, not the event bus, is the record of truth.

Use events for progress and state. Read a snapshot or transcript for authoritative saved conversation data. See [HTTP](/channels/http), [Runtime](/reference/runtime), and [Journal](/reference/journal).

## Event envelope

`runtime.Event` has these JSON fields:

| Field | Type | Meaning |
| --- | --- | --- |
| `run_id` | string | Owning run |
| `turn_id` | optional string | Durable turn identity when supplied |
| `seq` | integer | Per-run journal position, not an independent event counter |
| `type` | string | Runtime type or unchanged Kit lifecycle type |
| `time` | timestamp | Event time; do not use as the replay cursor |
| `text` | optional string | Display text, such as response or question |
| `state` | optional string | Lifecycle state |
| `data` | optional JSON | Type-specific structured payload |

A live publish stamps an unset time. Journal replay constructs events from records and does not reproduce every original live envelope field; replayed events can lack the original time or turn identity. Do not depend on identical timestamps, payload completeness, or every event having a TurnID. Use journal sequence and durable identity where available.

## Runtime event types

| Constant | `type` | Semantics |
| --- | --- | --- |
| `EventTurn` | `run_turn` | New durable turn identity; supplies `turn_id` for scoped controls |
| `EventCancelRequested` | `run_cancel_requested` | Cancellation command is saved, not proof execution has stopped |
| `EventState` | `run_state` | Durable run-state transition; `state: cancelled` confirms cancellation completion |
| `EventSuspend` | `run_suspend` | Run parked for input; text and data describe the suspension |
| `EventResume` | `run_resume` | Input released suspension; text contains the rendered answer |
| `EventResponse` | `run_response` | Final assistant text for a completed turn |

A normal completed turn emits turn identity, running state, response, and completed state. A suspension emits the saved request and waiting state instead of a normal final response. State is distinct from response text; do not infer completion from a text delta.

Completion checks delay `run_response` until acceptance, not live text or saved assistant messages. Rejected candidates can therefore appear in live activity or a draft snapshot. The response is anchored to the assistant message; replay publishes it before the completed state. Empty responses do not produce a reconstructed non-empty response event.

Cancellation controls and answers can use `turn_id` to avoid affecting later work. A requested cancellation is cooperative and cannot undo external effects. See [Runtime](/reference/runtime).

## Kit activity

An agent that implements Kit subscription forwards lifecycle events with their unchanged Kit type names. The envelope's `data` is the JSON-encoded Kit event. Examples include tool calls, tool results, deltas, warnings, retries, and errors. Interpret their payloads through the pinned public Kit SDK; they are not all BONNIE record kinds.

These events are live-only. A custom Agent can omit subscription support and still implement the durable executor contract. Event payload marshaling failure does not turn observability into a journal write.

**Live deltas are not durable transcript fragments.** They can disappear after restart or backlog eviction. Several live events can share the same `seq`, because they anchor to the journal position at publication. Do not deduplicate all live activity by sequence alone, require consecutive sequence numbers, or describe Seq as a unique counter for every event.

## Subscribe in Go

For journal recovery plus live delivery:

```go
events, stop := runner.StreamEvents(runID, lastSeq)
defer stop()
for ev := range events {
    // Handle the event. Save the journal cursor for durable recovery.
    lastSeq = ev.Seq
}
```

`Runner.StreamEvents(runID, after)` returns events after the cursor and an unsubscribe function. Pass zero for available history. Call the function when the reader stops; it releases blocked sends and forwarding goroutines.

`runner.Events().Subscribe(runID, after)` accesses the EventBus directly. It serves only its in-memory backlog and later publications. It does not perform journal replay. Do not use it when a reconnect must survive a server restart.

## HTTP cursor recovery

```bash
curl -sN 'http://127.0.0.1:8080/bonnie/v1/runs/RUN_ID/stream?cursor=12'
```

The cursor is the last per-run Seq observed; the stream filters strictly after it. A durable event anchors to its causing record. A cursor remains meaningful after reopening the same journal. Journal records without events cause sequence gaps.

The bus retains `DefaultEventBuffer` (1024) recent events per run by default. Low-level hosts can set `WithEventBuffer` on the Runner. The backlog is bounded and in memory only.

When the cursor still reaches the backlog, StreamEvents uses it. When it is older than that backlog, or the process has restarted, StreamEvents reconstructs durable events from journal records and joins live delivery, filtering already-covered positions. A `Positioner` journal supplies anchoring; built-in Memory and SQLite implement it. Custom stores without that interface do not provide the same journal-anchored stream contract.

If journal catch-up fails, StreamEvents falls back to the live backlog instead of returning that storage error through the event channel. A stream alone is therefore not proof that storage replay succeeded. Check snapshots and journal errors when recovery data is missing.

Recovery guarantees apply to durable transitions and final text, not every Kit delta or progress update. A reconnect can recover state after losing intermediate animation. Strict cursor filtering can exclude live-only events anchored at the same position the client already saw. If a client needs all conversation parts, read saved messages rather than treating the stream as a complete model transcript.

Stream replay can reconstruct turn, cancellation, state, suspension, resume, and completed response events. It is not an export of every journal record. Branches, clears, repairs, sandbox records, and completion metadata must be inspected through saved state or journal data.

## Ordering and duplicate limits

Durable event positions follow the run's journal order. Sequence numbers are local to a run, not comparable across runs. Multiple Kit publications can share an anchor. Do not order distinct runs with Seq or wall-clock time.

Replay and live handoff filter already-covered durable positions. However, final response publication and the completed checkpoint are not one transaction. A crash in that interval can cause a response to be published again during recovery. External delivery can also retry. Client-side effects must tolerate repeat notifications; the runtime event stream does not grant exactly-once effects.

## Subscriber resources

The EventBus keeps a bounded reconnect backlog, but each subscriber has an unbounded queued delivery path. Slow readers consume memory rather than silently dropping events. Stop subscriptions promptly on disconnect and bound the number of clients. Do not assume the 1024-event backlog also bounds each client's queued memory.

`EventBus.Publish` calls activity logging outside the bus lock. Logging is synchronous: a slow logger can still delay the publishing execution. A standalone bus without a Runner anchor can leave Seq unset and has no journal recovery contract.

## Activity logging

Logging is disabled by default. Enable it with the root option:

```go
bonnie.WithActivityLogger(bonnie.NewActivityLogger(nil))
```

The default logger writes timestamped Info output to stdout. Supply a Charm `*log.Logger` for JSON formatting or a different level, or implement `runtime.ActivityLogger.LogActivity(Event)`.

| Level | Default logger content |
| --- | --- |
| Info | Run states, suspend/resume, tool names and call IDs, final response text |
| Error | Failed states, Kit errors, error tool results |
| Warn | Warnings and retries |
| Debug | Other lifecycle events and raw payloads |

Replayed events are not logged again. Logging works across channels, including NATS. Final response text is sensitive even at Info level. Debug output can include prompts, arguments, results, and reasoning. Protect log access and retention.

## NATS task status is a separate protocol

NATS JetStream task status events are not the runtime NDJSON stream. They use durable `task_accepted` and `run_state` notifications with task, attempt, worker, and event identities. They do not contain agent text or tool activity. Deduplicate by `event_id`, order a run by Seq, and expect at-least-once broker delivery within retention limits. Result and event streams are independent; do not assume cross-stream ordering.

See [Channels](/channels/overview) for transport selection and [Deployment](/guides/deployment) for stable worker ownership.
