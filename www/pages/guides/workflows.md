---
title: Durable Go workflows
description: Run typed Go workflows with durable steps, timers, and signals.
---

# Durable Go workflows

Package `github.com/mark3labs/bonnie/workflow` runs typed Go functions without
a model or Kit types. It shares BONNIE's journal, but keeps its records in a
reserved namespace, separate from conversation runs.

```go
var greet = workflow.Define("greet", "1",
    func(ctx context.Context, name string) (string, error) {
        greeting, err := workflow.Step(ctx, "greeting",
            func(context.Context, workflow.Attempt) (string, error) {
                return "Hello, " + name, nil
            })
        if err != nil {
            return "", err
        }
        if err := workflow.Sleep(ctx, "delay", time.Minute); err != nil {
            return "", err
        }
        return greeting, nil
    })

bonnie.New(
    bonnie.WithWorkflows(greet),
    bonnie.WithWorkflowSetup(func(ctx context.Context, service *workflow.Service) error {
        _, err := workflow.Start(ctx, service, greet, "request-123", "Ada")
        return err
    }),
).Serve()
```

The example uses `context`, `time`, `bonnie`, and `workflow`. The setup
callback must return without waiting for a workflow result. Applications
can keep the service to admit more work or send signals from their own
handlers. BONNIE adds no workflow HTTP routes or authorization policy.

For an embedded service, use `workflow.NewService(journal, definitions...)`
and drive it with `service.Run(ctx)`. Start work with
`workflow.Start(ctx, service, definition, requestID, input)`. Reuse a request
ID only for identical input and the same version. Repeated admission returns
the same handle, including after a restart. Read status with `handle.Status`
and wait with `handle.Result`. Cancelling the result wait does not cancel
execution; `handle.Cancel` does.

Send a durable, deduplicated value with
`handle.Signal(ctx, key, signalID, value)`. The value can arrive before
`workflow.Wait[T](ctx, key)`. Signal IDs are unique within a run. Conflicting
resends return `ErrConflict`.

## Recovery rules

A workflow replays from its start. A saved successful step callback does not
run again. A pending unsafe callback whose result was not saved blocks for
reconciliation, because its external effect is unknown. Inspect
`Status.BlockedKey`, verify the external outcome, then use typed
`workflow.Resolve` to save the verified result or error. Do not repeat the
action just to determine its outcome.

Retries require `workflow.RepeatSafe()` and `workflow.WithRetry(policy)`.
Send `Attempt.IdempotencyKey` to an external API that supports deduplication.
The key is stable across attempts and restarts. The attempt number is not
an idempotency key. `Permanent(err)` stops retries. `WithAttemptTimeout`
bounds the callback context; callbacks must honor cancellation.

## Constraints

- Keep code and JSON schemas immutable within a version. Register a new
  version for changes. Retain old versions for unfinished runs.
- Put effects, clocks, randomness, and network access in step callbacks.
- Choose branches from saved input and saved results only. Sort map keys
  before choosing operations. Operation keys must be unique and stable.
- Use one workflow goroutine. Do not call workflow operations from callbacks.
- Do not recover operation panics or put effects in deferred functions.
  Parking unwinds the function and runs its deferred functions.
- All values use JSON. Saved errors preserve text, not Go error identity.
- Use one journal owner and one workflow service. SQLite transactions do not
  provide distributed execution claims or exactly-once external effects.
- Stop all callbacks before closing the journal. An uncooperative callback
  can delay shutdown.

The [full workflow guide](https://github.com/mark3labs/bonnie/blob/master/docs/WORKFLOWS.md)
contains retry limits, reconciliation examples, storage details, and the
complete ownership contract.
