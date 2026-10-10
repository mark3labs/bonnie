# Durable completion checks

A Runner can use `WithCompletionHook(hook, limit)`. An agent can instead
implement `CompletionAgent`. A Runner hook takes precedence. Use
`WithCompletionLimit(limit)` to set or override the limit. The default limit
is zero. Negative limits become zero.

The hook receives the response and the number of continuations already used.
Empty `ContinueWith` accepts the response. Non-empty `ContinueWith` becomes a
new model prompt on the same agent and session. The Runner records feedback
before it starts that prompt. It charges one continuation before the prompt.
A request beyond the limit fails with `ErrContinuationLimit`. It does not
publish the rejected response. Hooks do not run for model errors or human
input suspension. Returned usage includes all known model usage, not hook
usage.

`RecordCompletion` is metadata. It does not add conversation messages. Its
payload contains the candidate, feedback, count, prompt, known usage, phase,
and message sequence watermark:

- `pending`: the initial model prompt or a charged continuation can run.
- `check`: the candidate is durable; the check can run.
- `continue`: feedback is durable; charge and start the next prompt.
- `accepted`: the accepted response can be published without another check.

A completed, failed, retired, or cleared run ends this completion budget.
Running and cancelled runs restore the latest completion state. `Start` uses
that stored prompt or candidate, not its new text or files. `Resume` supplies
the human answer as a prompt and keeps the count and known usage. A durable
resume record also keeps this answer if the process stops before the next
completion record.

When a pending prompt is already durable, recovery uses `ContinuationAgent`
to generate from the saved conversation without adding user input. Kit
v0.126.0 implements this with `ContinueResult`. A custom agent without this
interface returns `ErrContinuationUnsupported`. Factory tool recovery still
runs before generation; completed external effects must not be repeated.

An orchestration write error leaves the run running and returns an error.
This lets a later Runner restore pending work instead of silently starting
a new budget. Hook errors and model errors fail the turn. Cancellation
records the cancelled state and keeps the completion state for `Start`.

## Limits of recovery

- A check can run again if the process stops before its feedback is durable.
  Hooks must tolerate repeated calls. This is not exactly-once execution.
- A final, non-empty assistant text message on the active branch after the
  pending watermark can prevent a repeated model call. Tool-call messages,
  tool-result messages, and empty responses cannot prove completion. In those
  cases the model call can run again. External tool effects are not made
  exactly-once by these records.
- Message recovery cannot restore model usage that was not yet saved in a
  completion record. Reported usage can be lower after this crash window.
- Files are not copied into completion metadata. If the process stops before
  Kit saves the initial file input, automatic completion recovery cannot
  restore those attachments. Recovery does not use new Start attachments.
- Response publication and the completed state are not one transaction. A
  stop between them can cause the accepted response event to be sent again.
- Factory setup still runs on recovery, even for an accepted candidate. The
  same hook or completion agent and limit should be configured in each process.
- Live model events still stream while a candidate is being generated.
  Completion checks delay `EventResponse`, not these live events or the
  journal's conversation messages. `Snapshot` can show a draft from history.
- Runner ownership is local to the process. These records do not add a lock
  between two Runners that execute the same run at the same time.
