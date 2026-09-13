# Sandboxing

BONNIE runs the tool calls a model chooses. Without a sandbox those calls run
as the host process, with its files, its network, and its credentials.

This is not a theoretical risk. BONNIE's own live-model test once ran with
Kit's core tools in the repository working directory and the prompt "deploy the
app". The model answered the question, then wrote a `Dockerfile`, a
`terraform/` directory, `deploy.sh`, and three deployment documents into the
checkout. Nothing failed. Nothing warned. The test passed. See `docs/SPEC.md`
§4.9.

The `sandbox` package is the fix.

## Quick start

```go
provider := sandbox.Docker(sandbox.WithDockerImage("python:3.12-slim"))

runner := runtime.NewRunner(journal, sandbox.Agent(provider,
    kit.WithModel("anthropic/claude-sonnet-4-5"),
))
```

`sandbox.Agent` replaces Kit's core tools with a sandboxed `bash`,
`read_file`, `write_file`, and `list_files`. BONNIE's human-in-the-loop tools
stay, because they run in the BONNIE process and never touch the sandbox.

From the CLI:

```sh
bonnie serve --sandbox docker --sandbox-image python:3.12-slim
```

## Backends

| Backend | Isolation | User installs | New Go deps | Network policy |
|---|---|---|---|---|
| `Local()` | **none** | — | 0 | none |
| `Docker()` | container namespaces | Docker | 0 | allow-all, deny-all |
| `Microsandbox()` | microVM, guest kernel | `msb` | 0 | allow-all, deny-all, allow-list |

All three are in the main module and add no dependency: they drive a CLI
through `os/exec`.

`Local()` provides **no isolation**. It exists so a developer can work without
Docker and so the seam is testable with no daemon. Do not use it in production.

> **microsandbox network policy is fixed at create time.** Every mode is
> enforced — `--no-net` for deny-all, `--no-net --net-rule allow@<host>` for
> an allow-list — and verified with real egress on Linux with KVM (msb
> 0.6.18). But `msb modify` cannot change network rules, so a sandbox that
> already exists keeps the policy it was created with. When a host
> reconfigures its policy and reattaches, `Open` returns `ErrPolicyMismatch`
> instead of silently running under the old rules. The operator then decides:
> restore the matching policy, or delete the sandbox and lose the workspace.

### Choosing at runtime

```go
provider, err := sandbox.Select(ctx,
    sandbox.Microsandbox(),
    sandbox.Docker(),
)
```

`Select` returns the first backend that works here. It never falls back to
`Local`, because dropping from isolation to none is a security decision that
must be written down. Pass `sandbox.Local()` as the last candidate when that is
what you want.

## The design

### One path namespace

Every backend roots the agent's files at `/workspace`. A relative path resolves
from there; an absolute path is used unchanged. A conversation that resumes on
a different backend still finds its files.

### Lifetimes are decoupled

The sandbox opens on the **first tool call that needs it**, not when the run
starts. A run that parks for human input holds no sandbox compute, and a model
that never calls a tool never starts a container. `Stop` releases compute and
keeps the workspace, so a run can wait a week for a human at no cost and still
find its files.

This mirrors eve, which puts it well: *"the durable workflow can park or
restart independently, while the app runtime opens or reuses sandbox compute
only when code needs it."*

### Tools proxy in; the model never holds a handle

The tools run in the BONNIE process. The model drives work through tool calls
and reads results — it never holds a sandbox handle and never sees a
credential. Every sandbox call therefore travels the same journalling,
approval, and event path as any other tool.

### Seeding: `sandbox.Seeded` wraps any provider

[sandbox.Seeded](../sandbox/seed.go) is a `Provider` decorator that mirrors a
local directory into every sandbox it opens. It is how an agent manifest's
`workspace:` seed reaches the run — the mirror travels over the `Sandbox`
interface alone (`ReadFile` probe, then `WriteFile`), so it works identically
on local, Docker, and microsandbox, and a new backend gets it for free.

Two semantics are deliberate:

- **Skip-if-exists.** A file the sandbox already has wins, so a resumed run
  never has the model's edits reverted. A seed file the model deleted
  reappears — that is what a seed is, and it is stated on the function.
- **A failed seed fails the open.** The tool call that triggered it reports
  the error and the model retries; the seed is never silent and never
  permanent.

The wrapper forwards the optional provider interfaces — `Networked`,
`ExistenceChecker`, `RunDeleter` — so wrapping never widens what a caller can
request, and a backend that cannot enforce a policy still refuses one through
the wrapper.

### A non-zero exit is a result, not an error

`Exec` returns a `*Result` with an `ExitCode`. Only a failure to run the
command **at all** is a Go error. This matters in an agent loop: "the build
failed" is information the model must see and act on, not a transport fault.

## Two contracts an adapter must honour

### Every call must actually execute

A backend that memoizes is a correctness hazard, because a cached tool result
is indistinguishable from a real one. Measured against Dagger:

```
call 1: ledger="CHARGE CUSTOMER 1789226793"
call 2: ledger="CHARGE CUSTOMER 1789226793"   ← ran once, agent told twice
```

The agent believes it charged the customer twice. It charged once. No error, no
warning, and a 2500× speedup that comes from not executing.

Dagger's own docs flag the hazard from the other direction. They provide
`withVolatileVariable`, which sets an env var *without* invalidating the exec
cache, and warn:

> ⚠️ expert-only escape hatch … If that assumption is wrong, Dagger may reuse
> stale or incorrect cached results.

So ordinary `withEnvVariable` **does** invalidate, and a per-call nonce is the
sanctioned way to defeat the cache. Any future Dagger adapter must do that by
default, not as an option.

`TestEveryCallExecutes` in the conformance suite checks this for every backend.

### A backend that cannot enforce a policy must say so

`Docker` refuses a domain allow-list with `ErrPolicyUnsupported` rather than
running with open egress. A policy that silently does nothing is worse than no
policy, because the operator believes they are protected.

BONNIE broke this rule once, in its own code. The microsandbox adapter used to
accept `deny-all`, store it in a field, and never read that field again: the
caller got a `nil` error and full network access. It was found while writing
the docs, and the same rule now governs the reattach path: a sandbox that
exists under a different policy than the host configured produces
`ErrPolicyMismatch`, not a silent reattach. The rule applies to this
repository exactly as much as to a third-party adapter.

## The exit-code problem

`docker exec` and `msb exec` both exit with the **guest command's** code. So
exit 42 is ambiguous: the command failed, or the CLI did. Reporting the second
as the first tells the model its command failed when the truth is "the daemon
is down", and sends it off fixing code that was never broken.

BONNIE has the guest print its own exit code on a marked line:

```sh
sh -lc '"$@"; printf "\n__bonnie_exit_<nonce>:%d\n" $?' bonnie prog arg1
```

Passing the argv *after* the script means the shell quotes it, so an argument
holding a space or a quote is safe with no escaping. A per-call nonce means
output that merely looks like a marker cannot be mistaken for one. No marker
means the CLI failed before the guest ran, which is a Go error.

File reads skip the marker entirely, because it would corrupt binary content.

## Why the CLI and not the SDKs

**microsandbox** publishes a Go SDK, but it is a CGO wrapper around an embedded
Rust library. Measured:

```
CGO_ENABLED=0 go build   →  build constraints exclude all Go files in .../internal/ffi
CGO_ENABLED=1 go build   →  ok, 32 MB for a hello-world
```

Linking it would force `CGO_ENABLED=1`, need a cross C toolchain per release
target, and drop darwin/amd64. BONNIE would stop being a single static binary,
which is the property it is built around. The CLI costs the user one install
and keeps the release trivial. eve reaches microsandbox the same way.

**Dagger's** Go SDK is pure Go and cross-compiles cleanly, so it stays a
candidate — but as a separate module, because it adds 9 dependencies. It is
third in priority behind msb and Docker: its container IDs are session handles,
not content addresses (`grep -c FromID dagger.gen.go` → `0`), so suspend and
resume means exporting a workspace, the same shape as `docker commit`. It has
the cache hazard the others do not.

## Adding a backend

Implement `Provider` and `Sandbox`, then add the backend to `backends()` in
`conformance_test.go`. It inherits 18 cases covering exit codes, binary file
round trips, workspace persistence, reattachment after `Stop`, per-run
isolation, argv quoting, and the no-caching contract.

You implement nothing for seeding: `sandbox.Seeded` rides the `Sandbox`
interface. Do keep the optional interfaces (`Networked`, `ExistenceChecker`,
`RunDeleter`) out of `Provider` itself — they exist so a caller can type-assert
for capability, and the seeding wrapper forwards them by assertion.

A backend that cannot run on the test machine must **skip**, not fail.

## Testing

```sh
go test -race ./sandbox                      # local + any backend present
go test -race -tags integration ./sandbox    # live model, needs a key
```

The live suite runs against Docker by default. Set
`BONNIE_TEST_SANDBOX=microsandbox` to run it inside microVMs; `local` is not
on the menu because a backend that shares the host filesystem cannot test the
isolation claims. The model is `opencode/kimi-k2.5` when an opencode key is
present, falling back to Anthropic, OpenAI, and Google; `BONNIE_TEST_MODEL`
overrides all of it, and the suite skips — rather than fails — when the
credential or the backend is missing.

The live tests cover the claims that matter:

| Test | Claim |
|---|---|
| `TestLiveAgentWorksInsideTheSandbox` | a real model does real work, in the sandbox |
| `TestLiveAgentCannotReachTheHost` | it cannot read a host file — the §4.9 regression |
| `TestLiveSandboxSurvivesSuspendAndResume` | park, release compute, resume, files intact |
| `TestLiveParkedRunHoldsNoCompute` | no tool call means no container |

## Limits

- **`Local` is not a sandbox.** It is named honestly and documented loudly.
- **Docker is namespaces, not a kernel.** Use microsandbox when the threat
  model includes hostile code.
- **microsandbox is verified on Linux/KVM only.** All 18 conformance cases
  pass with `msb` 0.6.18 (2026-09-12), every network policy mode is enforced
  with real egress, and the live suite — a real model working, suspending,
  and resuming inside the microVM — passes. It has not been run on macOS with
  Apple Silicon; that verification (T-013) is deferred for lack of hardware.
- **microsandbox network policy is fixed at create time.** `msb modify`
  cannot change network rules, so a reattached sandbox keeps its create-time
  policy; `Open` reports a mismatch with `ErrPolicyMismatch` rather than
  silently reattaching.
- **Network is open by default.** Set a policy explicitly for untrusted work.
- **No resource limits by default.** Pass `WithDockerMemory` or the
  microsandbox equivalents.
- **The workspace is not a volume, deliberately.** Docker sandboxes keep
  their files in the container's writable layer (`docker run --detach`),
  microsandbox sandboxes in the rootfs `msb` manages, and `Local` in a host
  directory. Durability comes from reattach, not from a volume: `Stop` is
  `docker stop`, and the next `Open` starts the same container with the
  same files — verified live. What destroys a workspace is deliberate
  reclamation (`bonnie sandbox prune`, `docker rm`), `docker system
  prune`, or a daemon data-root migration — and the journal notes the loss
  into the conversation rather than handing the model a silent empty
  directory. A named volume would not change this honestly: `docker volume
  prune` kills volumes too, a volume is daemon-local (no multi-host gain),
  and it would double the lifecycle `prune` must reconcile. The durable
  artifact is the conversation; the workspace is best-effort and
  re-derivable. If a user's runs hold genuinely expensive state, the right
  shape is an opt-in `sandbox.persist` manifest key — a candidate task,
  not a default.
- **Sandbox lifecycle is journalled.** A sandbox that opens for a run writes
  a record naming the backend and the sandbox, so `bonnie runs show` can say
  what held its compute, a resumed run is told when its workspace was
  pruned, and `bonnie sandbox prune` can delete the sandboxes of terminal
  runs. The sweep in `serve` does not exist yet — reclaiming is manual.
