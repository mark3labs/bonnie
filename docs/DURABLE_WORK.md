# Durable work

BONNIE stores tool intents, submitted inputs, and child ownership in its
existing journal. No second database or configuration file is needed.

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

The scheduler resumes running submissions after a crash. It does not retry
older runs that have no submission record. Existing synchronous channel routes
keep their existing semantics; use the new submission route for durable input.
Atomic delivery markers require `runtime.StepJournal`.

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

### Workspace skills (opt-in)

Enable repository skills in the sandbox separately from authored tree skills:

```go
bonnie.New(
    bonnie.WithSkills(""), // no authored tree skills
    bonnie.WithWorkspaceSkills(bonnie.WorkspaceSkills{
        Exclude: []string{"archives/**", "**/fixtures"},
    }),
).Serve()
```

Omit `WithWorkspaceSkills` to disable workspace discovery. Repeated options
replace the policy. Negative limits, invalid exclusion globs, and combination
with `WithAgentFactory` are rejected before resources open. An enabled scan
opens sandbox compute at startup, even without a model tool call.

Only `<scope>/.agents/skills/<bundle>/SKILL.md` and
`<scope>/.kit/skills/<bundle>/SKILL.md` are candidates. Plain `skills/`, loose
Markdown, and nested assets are not skills. Directory and file symlinks are
not followed. Mandatory exclusions at every depth are `.git`, `node_modules`,
`vendor`, `.direnv`, `.venv`, `venv`, `__pycache__`, `.cache`, and `.bonnie`.
`Exclude` adds root-relative directory globs: `*`, `?`, and classes match
within a segment; a whole `**` segment matches zero or more segments. Matching
prunes descendants. Absolute paths, negation, empty/dot/dot-dot segments and
malformed classes are invalid. Ignore exceptions cannot undo exclusions.

`.gitignore` applies to tracked and untracked paths. Ignored parents are
pruned. A regular `.git` file or directory starts an independent repository
boundary; gitdir pointers are not followed. Inherited Git rules reset there,
but explicit exclusions remain. Outside repositories, rules start at workspace
root. No host ancestors, Git config, global excludes, or `.git/info/exclude`
are read. Scope is the nearest repository root, or the owner of `.agents` or
`.kit` outside a repository.

Defaults are depth 12 (root depth 0; skill parent depth is bounded), 100 active
skills, 128 KiB original bytes per skill, and 32 KiB exact escaped workspace
metadata. Internal bounds are 10,000 visited entries, 30 seconds per scan,
64 KiB per ignore file, 1 MiB aggregate ignore bytes, and bounded diagnostics.
An over-budget candidate is skipped; later candidates remain eligible. Entries
are selected in exact source-path order. No names, descriptions or bodies are
truncated. Invalid edits remove live entries; old durable versions remain.
Incomplete scans retain the previous complete catalogue. Cancellation aborts
before a new request. Diagnostics name relative paths, not file contents.

Names include readable components capped at 24 bytes and the full SHA-256 of
the exact source path, for example `workspace/repo-a/review--<64 hex digits>`.
The suffix is always present. Edits preserve identity; a declared-name change
or move changes it. Workspace entries cannot replace authored names.

Metadata is untrusted model input. Bodies arrive only through `activate_skill`;
`when: always` does not enable eager workspace bodies. Activation is not
approval and does not grant tools or execution permissions. Its output names
the selected identity, version hash, source, scope, base directory, and body.
Use normal sandbox tools for assets relative to that base. Durable bodies do
not snapshot assets: assets are live and may change or disappear.

Catalogue records contain instruction content and need the same access controls
and retention as conversation history. A selected version is journalled before
metadata or activation is available. A resumed execution first uses the last
recorded catalogue, even if source files changed or vanished. Its following
request boundary can select current files. Two stable reads detect unstable
writes, not an atomic snapshot: a valid temporarily stable intermediate file
can be selected. No watcher or public manual registration API is installed.

Remote discovery uses a versioned static Linux guest helper, installed lazily
outside the workspace into private backend storage. Release archives contain
`bonnie-discovery-amd64` or `bonnie-discovery-arm64`. Place it beside BONNIE.
A custom static agent can use its own fixed-operation helper entry point.
Guest and helper architecture must match. No guest Go, Python, or Git is
required. Unsupported platforms/protocols fail explicitly; discovery does not
fall back to host paths. Custom sandbox providers must supply DiscoveryReader.
