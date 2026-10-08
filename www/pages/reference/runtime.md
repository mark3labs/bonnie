---
title: Runtime
description: Durable run states, execution APIs, human input, cancellation, completion checks, and limits of crash recovery.
---

# Runtime

`github.com/mark3labs/bonnie/runtime` is the durable execution layer. A run contains a conversation, control identity, state transitions, and metadata. A turn is one execution within that run. A waiting run holds no agent compute and can wait indefinitely while its journal remains available.

The code and exported godoc define the contract. BONNIE is experimental, pre-1.0 software. Recovery tests do not establish exactly-once external effects or production suitability.

For normal hosting, use `bonnie.New(...).Serve()` or `Agent.Run(ctx)`. They install the managed sandbox, SQLite storage, and HTTP channel. See [Options](/reference/options), [Deployment](/guides/deployment), and [Channels](/channels/overview).

## Execution interfaces

`runtime.Agent` has three methods:

```go
type Agent interface {
    PromptResult(context.Context, string) (*kit.TurnResult, error)
    InjectSteer(string)
    Close() error
}
```

`*kit.Kit` implements it. `AgentFactory` is `func(context.Context, *Session) (Agent, error)`. The factory runs for each Start or Resume and receives a restored session. Agent instances and closure state are not retained across executions.

`NewRunner(j Journal, f AgentFactory, opts ...RunnerOption)` constructs an executor. A nil journal selects non-persistent `MemoryJournal`. A nil factory selects `KitAgent()`.

**The low-level default is not sandboxed.** `KitAgent`, `KitAgentWithoutHumanInput`, and `KitAgentWithSetup` run Kit core tools in the host process. A working-directory option does not prevent absolute-path access. Use the root managed agent or `sandbox.Agent` unless the host provides isolation itself.

Optional capabilities include `FileAgent` for file prompts, `Compactor` for compaction, `CompletionAgent` for completion checks, and Kit event subscription for live activity. Unsupported files fail with `ErrFilesUnsupported`; unsupported compaction fails with `ErrCompactionUnsupported`.

Runner options include `WithEventBuffer`, `WithActivityLogger`, `WithCompletionHook(hook, limit)`, and `WithCompletionLimit(limit)`. These are runtime options, not root `bonnie.Option` values. A Runner completion hook takes precedence over an agent's completion capability. The default continuation limit is zero; a negative runtime limit becomes zero.

## Run states

| Constant | Wire value | Meaning and next action |
| --- | --- | --- |
| `RunPending` | `pending` | Recorded, no executed step yet |
| `RunRunning` | `running` | Saved execution state; use `IsActive` to determine whether this Runner is executing it now |
| `RunWaiting` | `waiting` | Suspended for external input; answer with `Resume` |
| `RunCompleted` | `completed` | Normal outcome; another `Start` can add a turn |
| `RunFailed` | `failed` | Execution error; another `Start` can add a turn after investigation |
| `RunCancelled` | `cancelled` | Operator stopped work; completed steps remain; continue with `Start` |
| `RunRetired` | `retired` | Permanently closed; history remains readable |

`IsTerminal()` is true for completed, failed, cancelled, and retired. Terminal does not mean permanently closed except for retired. A saved running state can remain after a crash without an active executor.

## Start and resume

`Runner.Start(ctx, runID, Input)` begins a run or restores an existing non-waiting run before execution. `Runner.Resume(ctx, runID, []InputResponse)` answers a waiting run, including one parked by another process.

| `Input` field | Purpose |
| --- | --- |
| `Text` | User conversation input |
| `Files` | `[]kit.LLMFilePart` passed to a file-capable agent |
| `Context` | `[]string` facts for this turn only; journalled separately, not added as user history |
| `Title` | First non-empty title is retained for operator listings |
| `Origin` | First supplied conversation location is retained |
| `Trigger` | Structured turn provenance, such as schedule, occurrence, dispatch, and scheduled time |

Per-turn context is injected by the context-prepare hook. It does not become ordinary conversation history on later turns. Origin and principal identity are distinct from trigger provenance.

`Run` returns `ID`, `TurnID`, `State`, `Response`, optional `Suspend`, model `Usage`, and `Err`. Examine both the returned error and outcome. `Snapshot(ctx, runID)` reads saved status without starting a turn. A snapshot response can be a draft from history while completion checks are pending.

Start refuses a waiting run with `ErrRunWaiting`. Resume refuses a non-waiting run with `ErrNotWaiting`. Both refuse retired runs with `ErrRunRetired`. Concurrent execution of one run through one Runner fails with `ErrRunActive`. Use `errors.Is` for sentinel errors.

Start and Resume execute synchronously for the caller, but their execution context is detached from caller cancellation and deadlines. An HTTP disconnect must not destroy durable work. Stop execution through runtime controls; do not assume cancelling the caller's context stops its turn. A host must arrange its own drain and cancellation policy.

## Human input

`AskTool()` registers `ask_human`. `ApprovalTool()` registers `request_approval`. They return `kit.ToolOutput` with `Halt: true` and a `SuspendRequest` in `FinalValue`. The runner records the request and parks in waiting state.

`SuspendRequest` contains `TurnID`, `Kind`, `Prompt`, optional `Options`, and `ToolCallID`. Built-in kinds are `SuspendQuestion` (`question`) and `SuspendApproval` (`approval`). A custom tool can use the same suspension contract.

`InputResponse` contains `TurnID`, `Text`, and optional `Approved *bool`. Nil means no explicit approval verdict; true approves; false rejects. `Approve(note)` and `Reject(note)` construct the pointer values. The runtime renders the verdict as `approved` or `rejected`, with the note, for the model.

```go
answer := runtime.Approve("Use staging only.")
answer.TurnID = observedTurnID
run, err := runner.Resume(ctx, runID, []runtime.InputResponse{answer})
if err != nil {
    return fmt.Errorf("bonnie: resume approval: %w", err)
}
```

A non-empty response TurnID must match the current suspension. Use scoped answers for delayed clients. A legacy unscoped text answer cannot identify the question the person saw. Channels can also validate the halting tool call ID; see [HTTP](/channels/http).

**Halt stops the turn after the current step, not sibling tools in that step.** A sensitive action called in the same step as `request_approval` can still execute before suspension. Approval gates a later step. Enforce authorization in the action tool rather than relying only on model sequencing. The halt behavior requires Kit v0.113.3 or newer; do not lower the dependency pin below that release.

## Controls

| API | Contract |
| --- | --- |
| `Steer(runID, message)` | Injects a steering message into an active local agent; not a new durable user turn |
| `Cancel(runID)` | Cooperative stop of an active turn on this Runner; idle runs return `ErrRunNotActive` |
| `RequestCancel(ctx, runID, expectedTurnID)` | Durable cancellation command for active or waiting work, optionally scoped to an observed turn |
| `Clear(ctx, runID)` | Removes prior history from model context but keeps ID, address, journal, and working files; refuses active or retired runs |
| `Compact(ctx, runID)` | Uses a capable agent to summarize older context and journals the compaction |
| `Retire(ctx, runID, reason)` | Cancels active work, records the reason, and permanently closes the run |
| `IsActive(runID)` | Reports this Runner's in-process execution, not distributed ownership |

`RequestCancel` returns `CancelResult` with `requested`, `not_active`, or `stale`. Requested means the command is durable, not that execution has finished. Repeated requests do not add another command for the same turn. The cancelled state event confirms completion. Unknown and idle runs are harmless no-ops.

Cancellation withdraws a parked request. A late Resume then fails rather than approving later work. If the process stops after saving the command but before the cancelled checkpoint, the next Start or Resume applies the command first. Snapshot already reports cancelled with no pending request in that interval.

Cancellation is cooperative. Model calls, tools, and callbacks must observe their execution context. It is not a forced process kill and does not undo external effects. Retiring does not itself change a transport's address map; the channel unbinds its address after retirement.

## Durability boundaries

BONNIE uses four public Kit APIs:

| Need | Mechanism |
| --- | --- |
| Save conversation messages | `kit.SessionManager` implemented by `Session` |
| Checkpoint a finished step | `Kit.OnStepFinish` |
| Add replayed/per-turn context | `Kit.OnContextPrepare` |
| Park for input | `ToolOutput{Halt, FinalValue}` |

`Session` journals messages before adding them to its in-memory tree. It implements `kit.StepAppender`; a `StepJournal` commits the tool call and results atomically. Completed step data is written even when the execution context has been cancelled. Replay preserves typed calls, results, files, media, and reasoning from `Record.Payload`.

Restore reconstructs branch selections, compaction, extension data, model changes, and clears. An incomplete trailing tool step is removed from the restored context and a repair record is appended. Unanswered calls in the middle of history fail with `ErrCorruptConversation`; BONNIE does not silently rewrite damaged history. See [Journal](/reference/journal).

Saved steps do not need to be executed again to reconstruct context. This does **not** make a tool's external effect and its journal write one transaction. If an effect succeeds before its result is committed, recovery can repeat work. Use external idempotency keys, checks, or transactions for sensitive actions.

## Completion and recovery

Root `WithCompletionHook` supplies an execution-scoped factory. Low-level hosts can install a Runner hook or implement `CompletionAgent`. Empty `CompletionFeedback.ContinueWith` accepts a normal response; non-empty feedback requests another prompt in the same conversation. The runner saves feedback before continuation and charges the continuation before its prompt. Rejected candidates are not published as final responses.

`RecordCompletion` has durable phases:

| Phase | Recovery action |
| --- | --- |
| `pending` | Initial or charged continuation prompt can run |
| `check` | Candidate is saved; run its completion check |
| `continue` | Feedback is saved; charge and start the next prompt |
| `accepted` | Publish the accepted candidate without another check |

Completion metadata keeps the candidate, feedback, count, prompt, known usage, and message watermark. A saved running or cancelled logical turn restores this state. Start then uses the saved work, not replacement text or files. Resume supplies the human answer and retains the count. Completed, failed, retired, or cleared runs end that budget.

Recovery limits:

- An interrupted check can repeat. Hook effects must tolerate retries.
- A final non-empty assistant text after the watermark can prove completion. Tool messages, empty responses, and unsaved results cannot; the model call can repeat.
- Usage not yet saved in completion metadata cannot be recovered, so reported totals can be lower.
- Initial file attachments not yet saved by Kit cannot be recovered from completion metadata. New Start attachments do not replace them during recovery.
- Response publication and the completed checkpoint are separate. A crash between them can repeat the accepted response event.
- Setup and factories run again, even for an accepted candidate. Configure the same policy and limit after restart.
- An orchestration write failure leaves running state and returns an error so later execution can restore saved work. Hook or model failure ends the turn as failed.

Completion delays the final response event, not live model activity or saved draft messages. See [Events](/reference/events).

## Ownership and automatic recovery

A Runner serializes its own turns only. Two Runners or processes executing one run can interleave the conversation even when SQLite protects database integrity. Controls and cleanup must reach the execution owner. Shared storage is not a distributed lock or cancellation router.

Opening a journal or restarting a generic HTTP server does not automatically execute every saved running run. Start and Resume are the recovery entry points; transports and schedule workers apply their own retry rules. Core NATS can lose tasks and result delivery. JetStream retries are at least once and can create a new attempt or execute on another worker. Schedules save prepared dispatches and retry delivery, but channel posts can repeat after a crash. See [Channels](/channels/overview) and [Scheduling](/guides/scheduling).

Keep journal storage, working files, configured backends, and worker identity stable. Cleanup is opt-in and local to one owner. A journal survives removal of a sandbox, but it cannot recreate lost working output. See [Troubleshooting](/guides/troubleshooting) before retrying an external action.
