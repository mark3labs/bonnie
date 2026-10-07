# Cancel a turn

Cancellation stops current work. It keeps the conversation, committed steps,
working files, and run ID. It does not undo external effects. Model calls and tools
must observe their execution context. Cancellation is not a forced process kill.

## Controls

| Surface | Control |
|---|---|
| HTTP | `POST /bonnie/v1/runs/{id}/cancel` |
| Go client | `RequestCancel(ctx, runID, expectedTurnID)` |
| Terminal chat and dev | `/cancel` or Ctrl+W, also while waiting for input |
| Slack messages | Send `/cancel` as text in the bot DM or bound thread |
| Slack registered command | Configure its Request URL as the event path plus `/cancel`; supply the thread timestamp as command text, or omit it in a DM |
| Discord | Register `/cancel` with no options; `Config.CancelCommand` changes its name. `/ask` with message `/cancel` also works |
| Telegram | `/cancel` or `/cancel@botname`, in a private chat or group/topic |
| GitHub | Comment `@bot /cancel` in the issue, PR, or review conversation; the comment admission rules still apply |
| NATS | Worker-specific command subject, or `client.CancelTurn(ctx, target, turnID)` |

Slack does not supply thread identity in slash commands. A public-channel
command must supply a thread timestamp. BONNIE never guesses the latest thread.
The signed command's channel bounds its target. Unsigned controls are refused.

## Acknowledgement and completion

HTTP accepts an optional JSON body: `{"turn_id":"observed-turn"}`.
The response is a `runtime.CancelResult`:

- `202`, `status: requested`: the command was written to the journal.
- `200`, `status: not_active`: there is no current work to stop.
- `200`, `status: stale`: the supplied turn ID is no longer current.

An unknown HTTP run is a harmless no-op and is never created. NATS still
requires a valid, admitted task-attempt target. Authentication and platform
admission rules are unchanged.

A request acknowledgement does not mean execution has stopped. Chat reports
“Cancellation requested.” The runtime stream confirms completion with
`run_state` and `state: cancelled`. Active chat work also delivers its cancelled
outcome. Repeated requests do not add another cancellation record for that turn.

`run_turn` carries `turn_id`. Run responses, snapshots, suspension requests, and
state events expose the same identity. Use it to prevent a delayed request from
stopping a later turn. Omit it only when “stop whatever is current” is intended.

## Human input and recovery

Cancellation of a parked run withdraws its pending question or approval. A late
answer is refused because the run is no longer waiting. A new message can start
another turn. Structured responses can carry `turn_id`; an answer with an old
identity cannot authorize a new request. Legacy unscoped text answers cannot
identify which question the user saw; use scoped responses for delayed clients.

The cancellation command is durable before acknowledgement. If the process dies
before the final cancelled checkpoint, the next Start or Resume applies the saved
command first. Snapshot shows the cancelled state and no pending input in this
interval. A later Start begins a new turn; a stale Resume does not approve work.

One Runner must own execution of a run. Commands do not route to a different
process executing the same journal. NATS directs controls to the owning worker;
HTTP deployments must route controls to the execution owner. A shared SQLite
file is not a distributed cancellation coordinator.
