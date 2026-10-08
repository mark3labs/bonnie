---
title: Troubleshooting
description: Diagnose BONNIE startup, sandbox, waiting-run, schedule, and recovery problems without losing saved work.
---

# Troubleshooting

Start with the error and the durable run state. Do not delete the journal, prune a waiting run, or retry an external action merely to remove an error. Cancellation and retries do not undo side effects.

## Collect a small diagnostic set

```bash
bonnie version
./report-agent --help
curl -sS http://127.0.0.1:8080/bonnie/v1/health
bonnie runs list --journal .bonnie
bonnie runs show --journal .bonnie RUN_ID
```

Use the actual journal path and listen address. `bonnie runs` reads storage directly, so it works with the server stopped. Health reports liveness only; it does not read the journal or test the provider or sandbox.

Record the BONNIE version, Linux kernel version, selected backend, journal path, run ID, turn ID, state, and error code. Keep credentials, private prompts, and transcripts out of public bug reports. `runs show --json` can expose full records, including typed message payloads.

## Startup and tree problems

| Symptom | Check and action |
|---|---|
| Init refuses existing files | This is overwrite protection. Use a new directory or inspect the existing tree. Do not delete authored files without a backup. |
| Discovery says no Go module | Run from the tree with `go.mod`, or pass its directory to dev/build. Use `go mod tidy` after scaffolding. |
| Tool does not export `Tool()` | Each `tools/<name>/` package needs `func Tool() kit.Tool` in a non-test Go file. Check imports and the compiler error. |
| Duplicate tool name | Use unique runtime names and keep each name aligned with its directory. Do not register generated tools a second time. |
| Instructions cannot be read | Check `instructions.md`, the process working directory, and whether build embedded it. A tree's missing prompt is refused, not silently omitted. |
| Provider or model error | Check the selected `provider/name`, provider credentials, and Kit provider settings. An exported variable wins over `.env`. |
| Channel credential error | Set the named variables for the mounted channel. Do not put credentials in Go source or sandbox seed files. |
| Bind failure | Check another process on the port and the `--addr` value. Use loopback unless an access layer protects the service. |

Print discovery without changing or building the tree:

```bash
bonnie dev --dry-run
bonnie build --dry-run
```

Dev chooses a free loopback port from 8080 upward when no address is specified. Read its output rather than assuming 8080. `bonnie chat --addr 127.0.0.1:PORT` must use that port.

Dev watches Go wiring, instructions, skills, and module files. It does not watch `context/`. Restart after a seed change and use a new run to test it. Seeding does not overwrite existing run files. Do not edit `bonnie_gen.go`; the next generation replaces it.

## Sandbox startup and policy errors

### Landlock is unavailable

Check the kernel and its enabled security modules. Landlock needs Linux 5.13 or newer and can be disabled or omitted by the host. BONNIE checks the kernel capability instead of assuming it exists.

Use a declared Docker or Microsandbox backend when Landlock is unavailable. Do not select Local as a production workaround: Local has no isolation. Compiled agents only permit backends declared in their Go options; `--sandbox docker` cannot add one by itself.

### Docker is unavailable

Check both the CLI and daemon:

```bash
docker --version
docker info
```

Also check the configured CLI path, image access, image architecture, and user permissions. A present CLI with an unreachable daemon is a separate failure. A container with no network cannot install packages; prepare the image first.

Do not give a Landlock service access to the Docker socket to solve a permissions issue. That makes the socket reachable from model-selected commands.

### Microsandbox does not start

Check `msb --version`, the configured binary path, Linux KVM availability, and the runtime's host requirements. An installed executable alone does not prove that a microVM can start.

If installation is automatic, startup needs network access and a writable journal directory. Existing installations are not upgraded. `WithSandboxDownload(false)` disables downloads, and a custom binary path is not replaced. Preinstall and test the runtime for offline deployments.

### Policy is unsupported or does not match

- `ErrPolicyUnsupported`: the backend cannot enforce the policy. Landlock and Local do not support configurable network policies. Docker supports allow-all and deny-all, not a domain allow-list.
- `ErrPolicyMismatch`: an existing Microsandbox has rules different from the requested rules. Rules are fixed at creation. Save required artifacts and plan a replacement sandbox; do not silently reopen it with weaker rules.
- Unknown or undeclared sandbox name: inspect the compiled agent's help and `WithSandboxes` configuration. Availability failure does not select a fallback.

See [Sandboxes](/guides/sandboxes) for the supported combinations.

## File and skill problems

### The model cannot open a file

`context/` is seed data, not prompt text. Confirm that the file was present when the agent was built or started. Ask the model to list relative paths in its working directory. Container and microVM work roots use `/workspace`; Landlock and Local report their actual host-mapped paths.

Local and Landlock file operations reject a path that leaves the run root, including a symlink escape, with `ErrOutsideWorkDir`. Docker and Microsandbox permit absolute guest paths; these paths refer to the guest filesystem, not the host. Correct the path or supply the required file through `context/`. Do not widen host access to read one missing input.

A file in a skill's `references/`, `scripts/`, or `assets/` can be named by activation text but still exist only on the host. Put essential text in the skill body and required readable files in `context/`.

### A skill is missing

Check YAML `name` and `description` frontmatter, supported file layout, and the discovery plan. Use a `.md`/`.txt` skill file or `skills/<name>/SKILL.md`. The tree's authored skill directory is the intended set; host skills beside the server are not inherited.

The model selects when to activate a skill. Its body is not always in the prompt. Restart or rebuild after adding it. Check whether a populated disk skill directory is overriding embedded skills in the compiled agent's working directory.

### Working files disappeared

Check manual prune activity, automatic cleanup policy, backend deletion, changed working-file roots, and backend selection. The journal and working files are different resources. Restoring `journal.db` does not restore a container or microVM's files.

BONNIE notes a recorded sandbox's absence in the conversation when the backend can prove it. It does not reconstruct lost artifacts from display text. Restore files from a backup or provide new input. A later turn on a cleaned-up completed run legitimately starts without its old files.

A shared-directory “already in use” error means another run has the provider open. The guard rejects overlap rather than queuing it. Use isolated per-run storage or finish the earlier work. Do not start another provider or process to bypass the guard.

## Inspect and answer a waiting run

A `waiting` run has parked for input. It is not holding a model loop open and is not a failed run. Read its current snapshot:

```bash
curl -sS http://127.0.0.1:8080/bonnie/v1/runs/RUN_ID
```

Add the deployment's authorization header when required. Examine the suspension prompt and current turn identity before answering:

```bash
curl -sS -H 'Content-Type: application/json' \
  http://127.0.0.1:8080/bonnie/v1/runs/RUN_ID/respond \
  -d '{"responses":[{"turn_id":"CURRENT_TURN_ID","text":"Use staging only."}]}'
```

Put `turn_id` inside each response object. For an approval, also supply `"approved": true` or `"approved": false` explicitly, with a note in `text`. Approval still needs enforcement in the action tool.

Use identities from the current snapshot, not an old UI. A cancelled run no longer accepts the pending approval. Unscoped legacy text answers cannot prove which question the user saw; scoped responses are safer for delayed clients.

Restarting does not answer a question. `WithoutHumanInput` omits built-in human-input tools; it does not automatically approve actions or stop a custom tool from parking.

If an action ran before approval, examine the tool calls in that step. Halting approval stops later steps, not sibling calls in the same step. Enforce approval in trusted action code.

## HTTP errors and identity

HTTP errors from the run channel include stable codes. Branch on the code, not the changing message text.

| Code or status | Meaning and action |
|---|---|
| `run_not_found` | The exact run ID is absent. Check storage and ID. An exact-run POST does not create it. |
| `invalid_run_id` | Correct the ID; do not use reserved runtime IDs. |
| `run_not_waiting` | Read the current state before sending another answer. |
| `run_active` | A conflicting operation found active work. Wait or cancel deliberately. |
| `run_retired` | Retirement is permanent. Use a new run or a freed channel address. |
| `unknown_turn_policy` | Use a supported policy such as `steer` or `queue`. |
| `bad_request` / `too_large` | Correct JSON, required fields, or body size. |
| 401 | Supply accepted credentials. The authenticator refused the caller. |
| 500 / `internal` | Inspect protected server logs, verifier failures, storage, and provider errors. |

The unauthenticated HTTP channel refuses `operation_id`. It needs a proven principal to scope the key. Configure a BONNIE authenticator; an unexamined principal in request input is not proof.

A platform webhook can be rejected for signature failure, missing secrets, timestamp checks, or admission rules. Verify platform configuration and the adapter's contract. Do not disable signature verification to make a test pass.

## Cancel safely

Request cancellation for the turn you observed:

```bash
curl -sS -H 'Content-Type: application/json' \
  http://127.0.0.1:8080/bonnie/v1/runs/RUN_ID/cancel \
  -d '{"turn_id":"CURRENT_TURN_ID"}'
```

A 202 result with `status: requested` means the command was saved, not that execution has stopped. A 200 result can report `not_active` or `stale`. Watch for the stream's `run_state` event with state `cancelled`. Omitting `turn_id` means “stop whatever is current.”

Cancellation is cooperative, not a forced process kill. Custom tools and callbacks must observe their execution contexts. It keeps committed steps, conversation, run ID, and working files; it does not reverse effects. Cancellation of a parked run withdraws its question. A later message can start another turn.

After a crash between the saved cancellation command and final checkpoint, recovery applies the saved command before new work. Keep one execution owner: a shared SQLite file does not route controls to another running process.

`/retry` in terminal chat starts a new turn from the last user message. It can repeat external actions. Clear and compact change conversation state, not working files or external effects. Reset retires the old run and frees its address; it is not rollback.

## Stream and restart problems

Reconnect with the last event sequence you received:

```bash
curl -sSN 'http://127.0.0.1:8080/bonnie/v1/runs/RUN_ID/stream?cursor=12'
```

Include authentication when needed. Durable events are journal-anchored and replay after restart. Live-only deltas do not replay. A missing live text fragment does not prove that a committed step was lost. Check the snapshot and records.

If two processes executed the same run, stop the duplicate owner. SQLite protects record integrity, not turn coordination. Do not repeatedly retry a sequence conflict while both owners remain active.

For `ErrCorruptConversation`, stop execution and preserve a backup. BONNIE can repair an incomplete trailing tool step, but an unanswered tool call in the middle of history is treated as damage. Do not manually rebuild messages from `Record.Text`: it is a display projection, while `Record.Payload` contains the typed message, including tool calls. Investigate storage or custom journal code.

## Schedule problems

- **No cron fires in dev:** use `bonnie dev --schedule-clock`. Check `WithScheduleClock`, cron fields, zone, and the next occurrence time.
- **No initial catch-up:** `latest` needs prior cron history. It is not a full backlog replay.
- **Occurrences are skipped:** inspect unfinished work. A waiting run blocks new occurrences with default `Overlap: "skip"`.
- **Unfinished work fails after deployment:** compare saved and configured `Revision`. A revision mismatch prevents execution under changed definitions.
- **Trigger route returns 404:** it is mounted only with `WithScheduleTriggerAuthorizer`. Check the job name too.
- **External trigger is rejected:** supply stable `id`, `scheduled_at`, and `kind: "external"`; pass both normal HTTP and trigger authorization checks.
- **Destination is refused:** mount a tracked receiver. NATS is not a supported scheduled channel destination.
- **Duplicate platform posts:** delivery is at least once. A crash can occur after posting but before saving the receipt.
- **Delivery errors repeat:** BONNIE retries the saved result with backoff, not a new model turn. Correct channel permissions or availability.
- **Journal already owned:** another scheduler holds the lock. Stop that owner; do not delete the lock file to bypass it.

See [Scheduling](/guides/scheduling) for occurrence states, limits, and examples.

## Before you remove data

Stop the owning server, preserve a consistent journal and working-file backup, and inspect a prune dry run:

```bash
bonnie sandbox prune --journal .bonnie --sandbox docker --dry-run
```

Use the backend the runs actually used. A real prune deletes finished-run working files and cannot recover them from journal history. Waiting, pending, and running runs are kept. Do not use pruning as a generic recovery command.

Report reproducible defects with sanitized errors, versions, configuration, and a minimal test. Read the [runtime](https://pkg.go.dev/github.com/mark3labs/bonnie/runtime), [sandbox](https://pkg.go.dev/github.com/mark3labs/bonnie/sandbox), and channel godoc before changing a recovery boundary.
