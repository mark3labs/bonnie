# Durable workflows

Package `workflow` runs a normal Go function with durable operations. It does
not need a model, Kit types, or an implementation of `runtime.Agent`.

A workflow function replays from its start after a wait or restart. A saved
step outcome is read from the journal. Its callback does not run again.

## Define and start a workflow

```go
import (
    "context"
    "time"

    "github.com/mark3labs/bonnie/runtime"
    "github.com/mark3labs/bonnie/workflow"
)

type Order struct {
    ID string `json:"id"`
    Amount int `json:"amount"`
}

var processOrder = workflow.Define("process-order", "1",
    func(ctx context.Context, order Order) (string, error) {
        receipt, err := workflow.Step(ctx, "charge",
            func(ctx context.Context, attempt workflow.Attempt) (string, error) {
                // The payment API must deduplicate this key.
                return charge(ctx, order, attempt.IdempotencyKey)
            },
            workflow.RepeatSafe(),
            workflow.WithRetry(workflow.RetryPolicy{
                MaxAttempts: 3,
                InitialDelay: time.Second,
                MaxDelay: time.Minute,
            }),
            workflow.WithAttemptTimeout(30*time.Second),
        )
        if err != nil {
            return "", err
        }
        if err := workflow.Sleep(ctx, "settlement", 24*time.Hour); err != nil {
            return "", err
        }
        approved, err := workflow.Wait[bool](ctx, "approval")
        if err != nil {
            return "", err
        }
        if !approved {
            return "rejected", nil
        }
        return receipt, nil
    })
```

`charge` is an application function, not a BONNIE function. Construct an
embeddable service with a host-owned journal:

```go
journal, err := runtime.OpenSQLiteJournal(".bonnie")
// Check err. Close journal only after the service has stopped.
service, err := workflow.NewService(journal, processOrder)
// Check err.
handle, err := workflow.Start(ctx, service, processOrder,
    "order-request-123", Order{ID: "123", Amount: 500})
// Check err.
```

Run `service.Run(serviceCtx)` in a goroutine and handle its returned error.
Admission does not start an execution goroutine. `Run` drives admitted runs
and recovers old runs. Hosts with their own loop can call `Tick` instead.
A service holds no execution goroutine for a parked timer, retry, or signal.
The poll loop checks readiness every 20 milliseconds. It can run different
runs concurrently, but runs only one callback at a time in each run. There
is no global callback concurrency limit.

An admission request ID is required. It is unique across this journal's
workflow namespace, not just within one definition. Repeating the same
request, version, types, and JSON input returns the same run ID, also after a
restart or completion. Different content returns `workflow.ErrConflict`.
Do not use a new request ID to retry an admission whose response was lost.
The caller's context controls admission, not execution.

## Signals, status, and results

```go
err := handle.Signal(ctx, "approval", "approval-event-456", true)
// Check err. The signal can arrive before Wait.
status, err := handle.Status(ctx)
// Check err. status.State is Pending, Running, Waiting, Blocked,
// Completed, Failed, or Cancelled.
receipt, err := handle.Result(waitCtx)
// Check err. Cancelling waitCtx stops only this wait.
```

Use `workflow.Open[ResultType](ctx, service, runID)` to reconstruct a typed
handle. It checks the saved result type. A definition need not be registered
to read status or a completed result.

A signal ID is unique within its run. The same ID, key, and JSON value can
be sent again with no second delivery. Different content is a conflict.
Signals are saved before the send returns. `Wait[T]` saves signal consumption
and its result in one snapshot before it returns. It chooses the first
unconsumed signal with its key. A decoding error is saved as an operation
error and returned to the workflow. It does not silently discard the error
or wait for a different value. Identical signal resends are accepted after
completion; new signals are rejected for terminal runs.

Each operation key must be unique within a run. To wait repeatedly, use
stable keys such as `approval-0`, `approval-1`, and so on. Do not use a timer
or attempt count to make keys. Use the exact same keys when sending signals.
Mailbox values and deduplication IDs are retained in the journal.

`handle.Cancel(ctx)` saves execution cancellation, then cancels the active
callback context. Cancellation is terminal and survives restart. It cannot
undo an external effect. An already terminal run is unchanged. A callback
which ignores cancellation can remain active after cancellation is saved.
`Result` returns `ErrCancelled` for a cancelled run.

## Effects, retries, and unknown outcomes

`Step` saves an attempt as pending before it calls the callback. It saves
the JSON result before it lets the workflow continue. An ordinary callback
error is also saved. The workflow can handle that error. A callback error
is not retried by default.

Retries require both `RepeatSafe()` and `WithRetry(...)`. `MaxAttempts`
includes the first attempt. `InitialDelay` doubles after each failed attempt,
up to `MaxDelay`. A zero maximum delay means no cap. A zero initial delay
means immediate retry. All retry deadlines and attempt counts are durable.
`workflow.Permanent(err)` stops retries for that error. A callback panic is
an unknown outcome, not a normal returned error.

`Attempt.IdempotencyKey` stays the same for one step across all attempts and
restarts. `Attempt.Number` starts at one and increases each time a callback
starts. **Do not use the attempt number as an external idempotency key.**
`RepeatSafe` is an application promise, not automatic deduplication. Supply
the stable key to the external service, or use actions which are safe to
repeat. This is not an exactly-once external-effects guarantee.

If a process stops after an unsafe callback starts but before its result is
saved, the effect is unknown. On recovery the service records `Blocked` and
does not call the callback again. Inspect `Status.BlockedKey`, verify the
external outcome, and supply the result:

```go
err := workflow.Resolve(ctx, service, runID, "charge", verifiedReceipt, nil)
// Check err. verifiedReceipt must have the exact saved Go step result type.
```

To record a verified failure, pass a non-nil error as the last argument.
The workflow replays and receives that saved result or error. Resolve does
not perform or repeat the action. Alternatively, cancel the run. There is
no API that automatically repeats an unsafe unknown action.

For a repeat-safe unknown action, recovery schedules another attempt after
the saved policy's delay, if an attempt remains. It keeps the stable key.
If the attempt limit is exhausted, the step returns a saved error. The last
external effect can still be unknown; a failed workflow is not proof that
the external system did nothing.

An attempt timeout bounds the callback context. It does not stop a Go
function forcibly or undo an external action. Callbacks must honor their
context. A timeout is eligible for retry only when repeat-safe retries are
configured. During service shutdown, an interrupted pending callback stays
pending for recovery rather than becoming a durable execution cancellation.

## Deterministic code and immutable versions

Within a version, the workflow function, operation order, operation keys,
result types, retry options, durations, and serialization schemas must stay
unchanged. Register a **new version** when these change. The admitted
version is immutable. Keep old definitions registered until their runs end.
Runs for unregistered versions stay unchanged and do not execute. They are
not moved to the newest version. Restoring the old definition permits them
to continue.

The executor checks operation kind, order, key, type, duration, and options.
It also rejects a function that returns before all saved operations. These
checks catch many code changes, but they cannot prove that code is the same.
Do not change callback meaning or branching rules under the same version.

Rules for a workflow function:

- Put all external effects, clock reads, random values, network calls, and
  mutable state reads in `Step` callbacks.
- Use only saved input and saved operation results to choose a branch.
- Use deterministic iteration order. Sort map keys before choosing operations.
- Run workflow operations from one goroutine. Do not start goroutines in a
  workflow, and do not call workflow operations from a step callback.
- Do not recover panics around workflow operations. Parking uses an internal
  panic to unwind the function. It is caught by the service.
- Do not put effects in deferred functions. They run whenever the function
  parks, not only when the run completes. In-memory locals are rebuilt on
  replay, not preserved across waits.
- Do not use `context.WithTimeout` around a workflow to express a durable
  deadline. Use a durable `Sleep` or an attempt timeout as applicable.

Input, workflow result, step results, reconciliation values, and signals use
`encoding/json`. Values must support JSON encoding and decoding. The first
successful step result also passes through JSON decoding, so it has the
same shape as a replayed result. For example, JSON numbers decoded into
`any` are `float64`. Prefer concrete structs with stable JSON tags. Error
identity and error types do not survive JSON; saved errors return their
text, including on the first return from a step. Do not branch on
`errors.Is` for a saved callback error. Return a structured step result for
business outcomes that need typed branch decisions.

## Root BONNIE integration

```go
bonnie.New(
    bonnie.WithWorkflows(processOrder),
    bonnie.WithWorkflowSetup(func(ctx context.Context, service *workflow.Service) error {
        // Admit initial work or give service to application handlers.
        // Do not wait for workflow results in this callback.
        _, err := workflow.Start(ctx, service, processOrder,
            "order-request-123", Order{ID: "123", Amount: 500})
        return err
    }),
).Serve()
```

`Agent.Run` starts the service only when `WithWorkflows` supplies definitions.
Setup callbacks run after the listener is bound and all channels have started,
but before the workflow loop starts. All setup callbacks are checked for nil
before any callback runs. A setup failure stops the service and waits for its
active callbacks. No workflow service starts for a normal
agent with no workflow configuration. Setup without definitions is an error.
The service uses the same journal as the agent. Workflow runs use reserved
`bonnie.workflow.` IDs and their own record kind; the conversation executor
and normal run listings exclude them. Use the workflow service for status.
This feature adds no HTTP routes or authentication policy. Expose application
handlers only with the authorization your application requires.

Service errors stop the agent. Shutdown cancels callbacks and waits for them
before closing the journal. A callback that ignores its context can delay
shutdown; Go cannot safely kill it. In an embedded host, `Shutdown(ctx)` can
return a timeout while callbacks remain active. Do not then close the journal
or start a replacement service against it. Wait for the callbacks to return.

## Storage and ownership

The workflow service appends full snapshots to the existing journal. Each
snapshot is one atomic record. Admission and request deduplication are one
snapshot. Signal delivery, deduplication, and consumption also use snapshots.
A failed write stops that service from making more writes or starting more
callbacks: the commit may have succeeded despite the error. Construct a new
service after the old service stops, then recover from the journal.

Use the default SQLite durability policy for power-loss durability.
`runtime.MemoryJournal` is useful in tests but cannot survive a process exit.

There must be **one journal owner and one workflow service** for this
namespace. There are no distributed locks, claims, or leases. Multiple
processes or separate service instances must not execute this namespace
concurrently. Sharing the same journal object does not coordinate separate
workflow services. Run replicas require application-level ownership outside
this API; do not infer distributed safety from SQLite transactions.

This implementation retains full snapshots, results, and mailbox history.
It has no snapshot compaction, retention policy, or distributed scheduler.
Large values and long histories increase storage use. Do not store secrets
without a suitable storage and access policy.
