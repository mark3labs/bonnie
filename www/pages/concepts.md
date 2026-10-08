---
title: Core concepts
description: Understand runs, turns, steps, durable replay, human input, addresses, and sandbox state.
---

## Run, turn, and step

A **run** is a durable conversation identified by a run ID. It can contain many **turns**. A turn starts with input and ends with a response, suspension, cancellation, or failure. A **step** is a model response and the tool results associated with it.

```text
Run: support-42
  Turn: "Check the deployment."
    Step: model calls a tool; tool returns a result
    Step: model calls ask_human
  State: waiting
  Turn after Resume: human supplies "Use staging."
    Step: model continues with the stored conversation
  State: completed
```

`completed` means a turn has finished. The run can receive another turn. `retired` means the run is closed permanently.

## Durable state

The default journal is SQLite at `.bonnie/journal.db`. It stores typed conversation messages, run state, and control records. Typed messages preserve tool call IDs, arguments, results, and other message parts. Display text alone is not enough for replay.

BONNIE uses the public Kit session manager and lifecycle hooks. A tool-calling step reaches SQLite as an atomic transaction. When another process opens the same journal, the runner restores the conversation before it calls the model again.

Keep sandbox working files as well as the journal. Conversation replay cannot recreate every output file. Read [Journal](/reference/journal) for storage, backup, and recovery details.

## What can repeat

BONNIE does not provide exactly-once external execution. Consider this order:

1. A custom tool charges a card.
2. The process crashes before the step commits.
3. Recovery removes or retries incomplete work.
4. The model can call the tool again.

Use external idempotency keys and reconcile effects whose outcomes are uncertain. Completion callbacks can also repeat after interruption. A restored committed step is different: its tool results are already in conversation history.

SQLite serializes writes, but it does not coordinate model execution across servers. Use one execution host for a journal. Do not place the journal on a network filesystem with unreliable POSIX locks.

## Waiting for a person

The built-in tools are `ask_human` and `request_approval`. They return a halting tool result with a `runtime.SuspendRequest`. The runner saves it and returns a `waiting` run. No agent process or running sandbox compute is needed while it waits.

Later, `Runner.Resume` supplies `InputResponse` values to the parked tool call. Channels expose this through HTTP responses or messages in the original thread.

**A halt stops the next model step, not sibling tool calls.** If the model calls an approval tool and an action tool in the same step, the action can still execute. Enforce critical authorization inside the action tool itself. The model decides when to request approval unless your host adds a policy.

## Input and history

`runtime.Input.Text` becomes conversation history. `Files` adds typed file parts. `Context` provides information for this turn only; it is not stored as user conversation history. `Title` and `Origin` provide metadata on the first turn.

Authored `context/` files serve a different purpose: they seed each run's sandbox without replacing files already there. They are not automatically inserted into the prompt.

## Addresses and ownership

A channel address names a conversation in a platform: an HTTP session label, Slack thread, Telegram topic, or GitHub issue. The journal maps it to a run. A restart does not lose the mapping. Channel names separate otherwise identical addresses.

An exact run-ID request must target an existing run. An address request can create or continue a conversation. Retirement frees the address for a new run while preserving the old run for inspection.

A principal is an application identity. HTTP must verify that identity through an authenticator or trusted gateway; an unverified JSON field is not proof. Chat adapters verify platform signatures, not independent proof of the person behind a platform user ID.

## Sandboxes and host tools

Landlock is the default. It confines filesystem access and limits the command environment, but it shares the host kernel and does not restrict network access. Docker adds container namespaces. Microsandbox supplies a guest kernel. Local provides no isolation and is for explicit development use only.

Sandbox core tools run in the selected backend. Custom Go tools run in your host process. Do not assume they inherit sandbox restrictions. The sandbox opens lazily on the first tool call that needs it.

## State and control

| State | Meaning | How to continue |
| --- | --- | --- |
| `pending` | Run is not executing yet | Start |
| `running` | A turn is executing | Wait, steer, or cancel |
| `waiting` | A tool needs human input | Resume |
| `completed` | Turn finished | Start another turn |
| `cancelled` | Operator stopped the turn | Start another turn |
| `failed` | Turn did not finish | Inspect the error, then start when safe |
| `retired` | Run is permanently closed | Use a new run |

Clear drops conversation history while retaining the run. Compact summarizes older messages. Cancel preserves finished steps but does not undo external effects. See [Runtime](/reference/runtime) for exact contracts.

## Events and schedules

Events expose state changes and live model activity. Durable event cursors remain meaningful after a restart. Live-only deltas are not durable and cannot be replayed. See [Events](/reference/events).

Schedules start tasks at configured times. They persist their control state in the journal; they are not permission to run multiple execution servers against the same journal. See [Scheduling](/guides/scheduling).
