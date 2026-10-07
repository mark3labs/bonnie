# Completion checks

Use `bonnie.WithCompletionHook` to check a normal model response before BONNIE
publishes its final outcome. A check can accept the response or request another
model turn in the same conversation.

Use `bonnie.WithKitSetup` to register public Kit hooks before the first prompt.
Both APIs receive a `bonnie.RunScope`. Its `Exec` function uses the same lazy
sandbox handle as the agent's tools. Callback code itself runs on the host.

## Example

This agent checks the working files before it accepts a response. If a required
file is absent, it asks the model to create the file. Put the agent's prompt
in `instructions.md`, as in other agent trees.

```go
package main

import (
    "context"
    "fmt"

    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/sandbox"
)

func main() {
    bonnie.New(
        bonnie.WithCompletionHook(bonnie.CompletionPolicy{
            MaxContinuations: 2,
            NewHook: func(_ context.Context, scope bonnie.RunScope) (bonnie.CompletionHook, error) {
                return func(ctx context.Context, candidate bonnie.CompletionCandidate) (bonnie.CompletionFeedback, error) {
                    result, err := scope.Exec(ctx, sandbox.Shell("test -s report.md"))
                    if err != nil {
                        return bonnie.CompletionFeedback{}, fmt.Errorf("bonnie: check report: %w", err)
                    }
                    if result.ExitCode != 0 {
                        return bonnie.CompletionFeedback{
                            ContinueWith: "Create a non-empty report.md in the work directory, then give your final response.",
                        }, nil
                    }
                    return bonnie.CompletionFeedback{}, nil
                }, nil
            },
        }),
    ).Serve()
}
```

The example uses BONNIE's default sandbox. `sandbox.Local` is for development
only: it provides no isolation from the host filesystem or network.

## Setup and lifetime

- Setup callbacks run in registration order after BONNIE installs its durable
  session and hooks, but before the first prompt.
- The completion factory runs after all setup callbacks. It creates one hook
  for each managed Start or Resume execution, including recovery.
- `RunScope.RunID` identifies the run. `RunScope.Exec` opens the sandbox only
  when needed. Setup, completion, and model tools share that handle and its
  working files. BONNIE owns cleanup. Do not retain `Exec` after execution ends.
- Closure state is local to the execution. It is not durable. Store durable
  application state outside the closure when needed.
- Observe context cancellation in callbacks. Use `scope.Exec` for sandbox
  commands; a direct host command does not gain sandbox protection.

## Acceptance and continuation

`CompletionCandidate.Response` is the response to check.
`ContinuationsUsed` is the number of additional model turns already charged
for this logical turn.

An empty `CompletionFeedback.ContinueWith` accepts the candidate. A non-empty
value becomes a new model prompt, not a steer message. The hook runs again on
the next normal response. `MaxContinuations` bounds these additional turns;
zero allows acceptance but no additional turn. A request beyond the limit
fails the run with `bonnie.ErrContinuationLimit`. Use `errors.Is` to test it.
BONNIE does not publish the rejected response as the final outcome.

Checks do not run on human-input suspension or model failure. Hook errors
fail the run. The final outcome contains the accepted response and all known
model usage across the initial turn and continuations. Hook work is not part
of model usage.

Completion delays the final response event, not live model events or saved
conversation messages. A snapshot can still show a draft.

## Validation

A second `WithCompletionHook`, a nil factory, or a negative limit is a
configuration error. A nil setup callback, a failed setup or factory, or a
factory that returns a nil hook prevents execution.

Neither `WithKitSetup` nor `WithCompletionHook` can be combined with
`WithAgentFactory`: a custom factory owns its agent. Low-level hosts can use
the completion interfaces in `runtime` instead.

## Recovery and limits

BONNIE saves completion feedback before it starts a continuation and charges
the continuation before its prompt. Suspension, cancellation, and recovery
do not reset the budget. A completed or failed logical turn starts a new
budget on its next turn.

An interrupted check can run again. External effects must tolerate retries;
this API does not provide exactly-once execution. Configure the same policy
and limit after restart. Usage can be lower if the process stops after a
model response but before its usage is saved. Recovery also cannot restore
initial attachments that Kit has not saved. An accepted response event can
repeat if the process stops before the completed state is saved.

The sandbox and root completion wrappers forward file prompts and event
subscriptions to Kit, including the unsubscribe function. A wrapped agent
without file support returns `runtime.ErrFilesUnsupported`; an agent without
event support returns a no-op unsubscribe function.

For record phases and detailed recovery limits, see
[Durable completion checks](../runtime/completion.md).
