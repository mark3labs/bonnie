# Durable work

BONNIE stores tool intents, submitted inputs, and child ownership in its
existing journal. No second database or configuration file is needed.

## Workspace skill versions

`WithWorkspaceSkills(WorkspaceSkills{})` enables bounded repository discovery
inside each managed run's sandbox. It is off by default. Selected catalogues
are full append-only `workspace_skills` records with schema version, monotonic
revision, exact source/scope/base, qualified identity, original bytes and hash,
and parsed description/body. They have no entry ID and are not messages.
Clear and branch changes retain the current run-scoped catalogue.

A selection commits before its metadata reaches a model request. That revision
stays fixed for the request and its tool batch. Activation reads saved content,
not a live source file. Existing tool intent/outcome durability stores the
activation result (including its version hash) before the atomic message step
and step checkpoint. The next catalogue transition occurs at the following
request boundary. Identical selections do not append another snapshot.

Restore verifies schema, content hashes, identities and parsed content, with a
64 MiB decode ceiling. Journal errors and corrupt content stop execution.
The catalogue is restored before tool recovery. The first resumed request uses
the saved versions even if sources changed or disappeared. Current filesystem
changes can become active at the following boundary. An interrupted activation
is not silently executed against a new body; ordinary uncertain-tool recovery
still applies. Message payloads and earlier results remain lossless.

Only skill files are versioned. Bundled assets remain live sandbox data and
can be missing or changed. Protect journals as instruction-content storage;
full snapshots can grow after repeated edits and no history deletion is added.
See the workspace-skills guide for trust, exclusion, scan-limit and guest-helper
requirements. Progressive disclosure is not permission to execute scripts.

## Tool recovery

A Kit agent made with BONNIE's factories stores a tool intent before execution
and a text outcome after execution. On restore, calls absent from the saved
transcript receive a complete tool-call/result pair. A known result is restored.
An unknown result says that the external effect is unknown. Calls with the same
tool and canonical JSON arguments are blocked until a host verifies the effect.
This is not exactly-once execution. An agent can propose different arguments;
the host must still restrict dangerous tools and require approval.

No tool is replay-safe by default. Opt in only for tools whose implementation
is safe to repeat:

```go
bonnie.New(bonnie.WithReplaySafeTools("lookup_status")).Serve()
```

A runtime host can call `session.SetReplaySafeTools` from `KitSetup`.
Both the saved policy and the current policy must permit replay. Recovery calls
the tool implementation directly, not interactive Kit approval hooks. A safe
tool must enforce its own authorization and external idempotency.

To resolve an uncertain action, a trusted host can call
`session.ResolveInterruptedTool(ctx, callID, verifiedResult)` from `KitSetup`.
Do not expose this method to the model as an approval tool.

The public Kit result hook provides text. Normal completed steps retain their
full typed messages, including media. Recovery of an outcome stored before its
step retains only the hook's text projection. Custom `AgentFactory` values
that do not use BONNIE's Kit factories must provide their own tool recovery.

## Durable inputs

For runtime hosts:

```go
submission, err := runner.Submit(ctx, runID, "request-123",
    runtime.Input{Text: "Review this change"}, runtime.BusyQueue)
```

Run `runner.RunScheduler(ctx)` in a separate goroutine. The root BONNIE service
starts it automatically. `WaitSubmission` waits without cancelling execution
when its wait context ends. `Submissions` finds inputs after restart.
`AbortSubmission` withdraws an input that has not started.

Policies:

- `BusyQueue`: execute after the current turn.
- `BusyReject`: refuse an active or human-waiting run.
- `BusySteer`: store text before injection. The saved user message contains a
  submission marker. If a crash prevents placement, deliver it as a follow-up.
  Files and per-turn context are not supported for steering.

Request IDs deduplicate admission within a run, not external service actions.
A submission is done when its turn reaches a boundary, including a human wait.
Use the existing `Resume` API to answer that wait. Queued work stays parked
until the human wait ends. Failed work is not retried automatically.

The scheduler resumes running and interrupted submissions after a crash or a
restart; stopping the scheduler interrupts its turns and leaves their
submissions running for the next process. Runs that have no submission record
are continued by `Runner.Recover`, which the root BONNIE service calls at
startup. Atomic delivery markers require `runtime.StepJournal`.

## Child runs

`SpawnChild(ctx, parentID, stableKey, input, background)` stores child ownership
and its first submission atomically. The same parent and key return the same
child after restart. A stable tool-call identity is a suitable key.

The runner's factory constructs the child's agent from its session. This API
does not use Kit's process-local subagent implementation. `Children` lists the
relationship. A parent does not finish successfully before its foreground
children finish. `WaitChildren` also lets a host wait explicitly. The scheduler
runs different conversations concurrently so a waiting parent does not block
its children. A human-waiting child remains live.

Parent failure and cancellation stop foreground descendants. `CancelOwned`
also withdraws queued inputs and can include background descendants when its
last argument is true. Cancellation is cooperative; it cannot undo external
effects. `CancelOwned` also closes later child admission and submission
execution on that owner. Use a new run for new owned work. Background children
do not keep the parent busy. Child creation requires `runtime.StepJournal`.

## HTTP

All routes use the existing authentication guard and target existing run IDs:

- `POST /bonnie/v1/runs/{id}/submissions`: admit
  `{"request_id":"r1","text":"Review","policy":"queue"}`; returns 202.
- `GET /bonnie/v1/runs/{id}/submissions`: list inputs and their states.
- `DELETE /bonnie/v1/runs/{id}/submissions/{submission}`: withdraw queued input.
- `POST /bonnie/v1/runs/{id}/children`: create with
  `{"key":"reviewer-1","text":"Review","background":false}`; returns 202.
- `GET /bonnie/v1/runs/{id}/children`: list owned runs.
- `POST /bonnie/v1/runs/{id}/cancel-owned`: stop owned work. Set
  `{"include_background":true}` to include background descendants.

## Ownership and shutdown

Use one process and one scheduler per journal. This release does not add
cross-process execution leases. SQLite transaction locks protect records, not
external tool execution. Do not point two service instances at the same journal.
Do not mix direct `Start` calls with scheduler delivery to the same run.

Stopping the scheduler stops admission and cooperatively cancels its active
turns. A process crash leaves running submissions for the next scheduler.
Waiting human requests require no running agent.
