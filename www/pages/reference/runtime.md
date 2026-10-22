---
title: Runtime
description: Durable run states, execution APIs, human input, cancellation, completion checks, and limits of crash recovery.
---

# Runtime

`github.com/mark3labs/bonnie/runtime` is the durable execution layer. A run contains a conversation, control identity, state transitions, and metadata. A turn is one execution within that run. A waiting run holds no agent compute and can wait indefinitely while its journal remains available.

The code and exported godoc define the contract. BONNIE is experimental, pre-1.0 software. Recovery tests do not establish exactly-once external effects or production suitability.

For normal hosting, use `bonnie.New(...).Serve()` or `Agent.Run(ctx)`. They install the managed sandbox, SQLite storage, and HTTP channel. See [Options](/reference/options), [Deployment](/guides/deployment), and [Channels](/channels/overview).

Clients should prefer executor-independent `wire` types for shared transport
contracts. Runtime compatibility aliases such as `runtime.CancelResult`,
`runtime.RunState`, and `runtime.Event` remain valid.

## Scoped agent operations

The root `bonnie.Agent` exposes resources for custom commands without starting a server:

| API | Contract |
| --- | --- |
| `Agent.WithJournal(ctx, func(context.Context, runtime.Journal) error)` | Opens and closes the configured SQLite journal. No dotenv, agent files, factory, provider, or server setup. Opening can create storage and apply schema setup; this is not a read-only connection. |
| `Agent.WithRuntime(ctx, func(context.Context, *bonnie.Runtime) error)` | Prepares the same files, factory, journal, and runner as `Agent.Run`. No HTTP listener, channels, scheduler, or background cleanup starts. |
| `Runtime.Journal()` | Returns the callback's journal. Do not close it. |
| `Runtime.Runner()` | Returns the callback's configured runner. |

Do not retain these resources after the callback. Wait for all operations and goroutines before returning. Callback, context cancellation, and close errors are returned together. Nil callbacks fail without opening resources. Hosts outside `Serve` must supply their own signal-aware context.

The caller manages any `Runner.RunScheduler` explicitly: start it, cancel it, and wait for it within the callback. Only one scheduler may own a journal at a time. Do not drive runs in a journal already owned by a serving process; use the HTTP API for live-server operations. Start and Resume have the cancellation rules described below.

See [Agent Commands](/guides/agent-commands) for command registration and a complete inspection example.

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

Runner options include `WithEventBuffer`, `WithActivityLogger`, `WithObserver`, `WithCompletionHook(hook, limit)`, and `WithCompletionLimit(limit)`. These are runtime options, not root `bonnie.Option` values. A Runner completion hook takes precedence over an agent's completion capability. The default continuation limit is zero; a negative runtime limit becomes zero.

## Run states

| Constant | Wire value | Meaning and next action |
| --- | --- | --- |
| `RunPending` | `pending` | Recorded, no executed step yet |
| `RunRunning` | `running` | Saved execution state; use `IsActive` to determine whether this Runner is executing it now |
| `RunWaiting` | `waiting` | Suspended for external input; answer with `Resume` |
| `RunInterrupted` | `interrupted` | The host stopped the turn to continue it later (shutdown, connection loss); `Continue` or `StartRetry` continues it |
| `RunCompleted` | `completed` | Normal outcome; another `Start` can add a turn |
| `RunFailed` | `failed` | Execution error; another `Start` can add a turn after investigation |
| `RunCancelled` | `cancelled` | Operator stopped work; completed steps remain; continue with `Start` |
| `RunRetired` | `retired` | Permanently closed; history remains readable |

`IsTerminal()` is true for completed, failed, cancelled, and retired. Terminal does not mean permanently closed except for retired. `running` and `interrupted` are not terminal: the turn has no stop reason. A `running` state that a crash left has no executor until the next start of a served agent, which continues it. See [Ownership and automatic recovery](#ownership-and-automatic-recovery).

## Start and resume

`Runner.Start(ctx, runID, Input)` begins a run or restores an existing non-waiting run before execution. `Runner.Resume(ctx, runID, []InputResponse)` answers a waiting run, including one parked by another process.

Nonempty `Start` input is always a new command, even when its text equals an earlier message. If saved work is interrupted or has unclosed completion metadata, Start returns a nil Run and `ErrInputDeferred`. This includes files-only, context-only, title, origin, trigger, and whitespace input. Empty slices and zero-value metadata do not count.

The refusal does not restore or repair the session, write input metadata, build an agent, or publish a response. A previously durable cancellation command is applied first and can write cancellation bookkeeping. Waiting, retired, and active errors keep precedence.

The caller must retain the command as undelivered: BONNIE does not queue it. A served agent continues interrupted work by itself at startup, so the refusal is usually short. A host that owns the runner recovers with `Continue`, or with `StartRetry` using the original admitted input when needed. Submit the new command with Start only after recovery closes the logical turn. If recovery parks, answer through Resume first. Do not use RequestCancel unless cancellation is intended.

`StartRetry` preserves cancelled snapshots and continues saved input without duplication. It is not generic idempotency after a completed turn. `Start(ctx, runID, Input{})` keeps legacy recovery behavior; it is not an explicit admission signal. There is no request-ID or exactly-once guarantee for successful Start calls.

| `Input` field | Purpose |
| --- | --- |
| `Text` | User conversation input |
| `Files` | `[]kit.LLMFilePart` passed to a file-capable agent |
| `Context` | `[]string` facts for this turn only; journalled separately, not added as user history |
| `Title` | First non-empty title is retained for operator listings |
| `Origin` | First supplied conversation location is retained |
| `Trigger` | Structured turn provenance, such as schedule, occurrence, dispatch, and scheduled time |
| `Trace` | W3C trace context (`traceparent`, `tracestate`) of the request that delivered the input. An observer makes the turn a child of it. Submissions journal it. Trace alone does not start a turn |

Per-turn context is injected by the context-prepare hook. It does not become ordinary conversation history on later turns. Origin and principal identity are distinct from trigger provenance.

`Run` returns `ID`, `TurnID`, `State`, `Response`, optional `Suspend`, model `Usage`, and `Err`. Examine both the returned error and outcome. `Snapshot(ctx, runID)` reads saved status without starting a turn. A snapshot response can be a draft from history while completion checks are pending.

Start refuses a waiting run with `ErrRunWaiting`. Resume refuses a non-waiting run with `ErrNotWaiting`. Both refuse retired runs with `ErrRunRetired`. Concurrent execution of one run through one Runner fails with `ErrRunActive`. Use `errors.Is` for sentinel errors.

## Observers

`WithObserver(o Observer)` reports each executed turn to `o`. `Observer.StartTurn` receives a `TurnStart`: run ID, turn ID, kind (`new`, `recovery`, or `resume`), title, origin, prompt, the input's trace context, the trace carrier of the run's last turn, and the answers that resume a waiting run. It returns the context the turn runs in and a `TurnObservation`. The runtime calls `Attach` when the turn's agent exists and `End` when the turn reaches a boundary or fails.

`Carrier` returns the trace context to journal with the turn record. The next turn receives it as `TurnStart.Previous`, also in another process. An observer sees live turns only: replay, snapshots, and rebuilt event streams never reach it. It runs on the turn's goroutine and must return promptly. Package `observe` implements it with OpenTelemetry; see [Observability](/guides/observability).

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

Completion metadata keeps the candidate, feedback, count, prompt, known usage, and message watermark. A saved running or interrupted logical turn restores this state through StartRetry (or zero-value Start). Recovery uses saved work, not replacement text or files. Resume supplies the human answer and retains the count. Completed, failed, cancelled, retired, or cleared runs end that budget.

Recovery limits:

- An interrupted check can repeat. Hook effects must tolerate retries.
- A final non-empty assistant text after the watermark can prove completion. Tool messages, empty responses, and unsaved results cannot; the model call can repeat.
- Usage not yet saved in completion metadata cannot be recovered, so reported totals can be lower.
- Initial file attachments not yet saved by Kit cannot be recovered from completion metadata. Replacement attachments do not replace them during completion recovery.
- Response publication and the completed checkpoint are separate. A crash between them can repeat the accepted response event.
- Setup and factories run again, even for an accepted candidate. Configure the same policy and limit after restart.
- An orchestration write failure leaves running state and returns an error so later execution can restore saved work. Hook or model failure ends the turn as failed.

Completion delays the final response event, not live model activity or saved draft messages. See [Events](/reference/events).

## Ownership and automatic recovery

A Runner serializes its own turns only. Two Runners or processes executing one run can interleave the conversation even when SQLite protects database integrity. Controls and cleanup must reach the execution owner. Shared storage is not a distributed lock or cancellation router.

A served agent (`Agent.Serve`, `Agent.Run`) calls `Runner.Recover` at startup, after its channels start: each run that is `running` or `interrupted` continues through `Runner.Continue` with no new input, and the result goes to the channel of its origin (`channel.Recoverer`). At shutdown, `Runner.BeginDrain` and `Runner.Drain` interrupt each turn so the next start continues it. Continue closes a turn whose answer or human-input wait is already durable without a model call, sends a durable unseen answer, and otherwise continues the saved conversation through `ContinuationAgent`; a run whose input never became durable fails with `ErrRecoveryInputLost`. Opening a journal alone executes nothing: a host that owns the runner calls Recover itself. Submissions belong to RunScheduler, scheduled dispatches to the schedule engine, and JetStream tasks to broker redelivery (`channel.RecoveryOwner`). Core NATS can lose tasks and result delivery. JetStream retries are at least once and can create a new attempt or execute on another agent. Schedules save prepared dispatches and retry delivery, but channel posts can repeat after a crash. See [Channels](/channels/overview) and [Scheduling](/guides/scheduling).

Keep journal storage, working files, configured backends, and agent identity stable. Cleanup is opt-in and local to one owner. A journal survives removal of a sandbox, but it cannot recreate lost working output. See [Troubleshooting](/guides/troubleshooting) before retrying an external action.
