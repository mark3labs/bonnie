---
title: Sandboxes
description: Select a BONNIE sandbox, restrict network access, and manage working files without weakening the security boundary.
---

# Sandboxes

`bonnie.New()` uses Landlock by default. `WithSandbox` selects a backend; it does not switch sandboxing on. There is no `none` backend. Explicit `sandbox.Local()` still exists for development and provides no isolation.

The sandbox boundary applies to BONNIE's standard command and file tools. Custom Go tools, setup callbacks, and completion callbacks run on the host unless they explicitly use sandbox execution. See [Tools and Skills](/guides/tools-and-skills).

## Select the boundary you need

| Backend | Boundary | Requirements | Network policies |
|---|---|---|---|
| Landlock, default | Filesystem containment; shared host kernel and namespaces | Linux 5.13+ with Landlock enabled; host shell | No configurable policy |
| Docker | Container namespaces and cgroups; shared host kernel | Docker-compatible CLI and reachable daemon | `allow-all`, `deny-all` |
| Microsandbox | MicroVM with its own guest kernel | Working `msb` runtime; Linux KVM for the verified deployment path | `allow-all`, `deny-all`, `allow-list` |
| Local | None | Host commands and filesystem | No configurable policy |

BONNIE's supported release platform is Linux. A backend's availability on another operating system does not make that system a supported BONNIE deployment.

Landlock commands can read the run's directory and read-only system paths needed to execute commands. They receive a minimal environment, not the server's full environment. Landlock does not restrict the network, Unix socket connections, process table, or host kernel.

**Do not run a Landlock agent as a user in the `docker` group.** A command can connect to a daemon socket whose path it knows. Access to `/var/run/docker.sock` permits a full host escape. Check other privileged daemon sockets too. Use an unprivileged account with no such access.

For hostile code, prefer Microsandbox. Docker provides a stronger boundary than Landlock, but not a separate kernel. No backend removes the need for host security updates, resource limits, and careful credential handling.

## Configure Docker with no network

This complete `main.go` selects a richer image and a memory cap:

```go
package main

import (
    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/sandbox"
)

func main() {
    bonnie.New(
        bonnie.WithSandbox(sandbox.Docker(
            sandbox.WithDockerImage("python:3.12-slim"),
            sandbox.WithDockerMemory("512m"),
        )),
        bonnie.WithNetwork(sandbox.NetworkPolicy{
            Mode: sandbox.NetworkDenyAll,
        }),
    ).Serve()
}
```

Set the provider key on the host. The sandbox policy applies to sandbox commands, not the host model connection or a custom host tool. Prepare the image before deployment; commands cannot download packages with `deny-all`. Pin an image version or digest appropriate to your release process. Docker's default image is `alpine:3.19`; the command user defaults to the image's user. Use `WithDockerUser` when the image supports a suitable non-root account.

Docker cannot enforce a domain allow-list. It returns `ErrPolicyUnsupported` instead of changing an allow-list to open egress.

## Configure a microVM allow-list

Add these options to `bonnie.New(...)`; import the `sandbox` package:

```go
bonnie.WithSandbox(sandbox.Microsandbox(
    sandbox.WithMicrosandboxImage("python:3.12-slim"),
    sandbox.WithMicrosandboxMemory(2048), // MiB
    sandbox.WithMicrosandboxCPUs(2),
)),
bonnie.WithNetwork(sandbox.NetworkPolicy{
    Mode:  sandbox.NetworkAllowList,
    Allow: []string{"api.example.com"},
}),
```

Microsandbox sets network rules when it creates the sandbox. On reattachment, BONNIE verifies that the live policy matches. A changed policy returns `ErrPolicyMismatch`; the old rules are not silently accepted. Preserve required output and plan a new sandbox before you change the policy for existing runs.

At startup, BONNIE looks for `msb` on `PATH`, then in the journal directory at `msb` or `microsandbox/bin/msb`. If none is available, it can download the tracked release, check its pinned SHA-256, and install the runtime under `<journal>/microsandbox`. Existing installations are not upgraded. A custom `WithMicrosandboxBinary` path is not downloaded or replaced.

Use `bonnie.WithSandboxDownload(false)` for controlled or offline deployment. Preinstall the runtime and verify its system requirements. A successful download does not provide KVM or correct host permissions.

## Declare operator choices

A compiled agent can permit a fixed list of configured providers:

```go
// Options inside bonnie.New(...), with sandbox imported:
bonnie.WithSandboxes(
    sandbox.Microsandbox(sandbox.WithMicrosandboxMemory(2048)),
    sandbox.Docker(sandbox.WithDockerMemory("512m")),
),
```

```bash
./report-agent --help
./report-agent --sandbox docker
```

The first provider is the default. Selection preserves that provider's options. Only the selected provider must be available. A failure does not select another backend. An unknown name is refused.

The list must be nonempty, with non-nil providers and unique nonempty names. Do not combine `WithSandboxes` with `WithSandbox` or `WithAgentFactory`, or set it twice. `Agent.Run(ctx)` does not parse flags and uses the first provider. Network and environment options apply to the selected provider; selecting Landlock with a network policy fails.

The standalone `bonnie serve` CLI also supports `--sandbox auto`, which uses availability-based selection. It is not a compiled agent's permission to select an undeclared provider. Prefer an explicit backend when deployment needs a fixed boundary.

## Working files and shell behavior

Sandboxes open lazily at the first tool call that needs one. A model-only turn does not start a container. A waiting run keeps durable state without holding sandbox compute. The files remain available for later turns unless they are deleted.

Container and microVM commands use `/workspace`. Host-mapped backends report their actual per-run host path. Use relative paths in instructions and skills. Landlock file operations reject working-directory escapes with `ErrOutsideWorkDir`, including symlink escapes. Local provides no isolation and permits other host paths. Docker and Microsandbox can access absolute paths inside their guest filesystem; their boundary is the container or microVM, not only `/workspace`.

The shell tool detects Bash on each call and uses `sh` when Bash is absent. Results name the selected shell. A failed command is not retried under a second shell. For portable commands, use POSIX shell syntax or select an image with Bash. Standard tool output is bounded; save large artifacts to files and publish them through a trusted mechanism.

`context/` seeds are copied without overwriting existing run files. They are not shared mutable output. A low-level host can use `sandbox.Seeded(provider, dir)` for the same copy behavior.

### Search and targeted edits

The built-in tool names are `shell`, `shell_status`, `shell_wait`, `shell_cancel`,
`read_file`, `write_file`, `list_files`, `grep`, `find`, and `edit`. Do not reuse
these names for extra tools; Kit's existing collision behavior applies.
Host core tools stay disabled. All file operations use the selected sandbox.

| Tool | Arguments and defaults |
|---|---|
| `shell` | Required `command`; optional `dir`; `wait_seconds` defaults to 30 (0–30). |
| `shell_status` | Required `job_id`. |
| `shell_wait` | Required `job_id`; `wait_seconds` defaults to 30 (0–30). |
| `shell_cancel` | Required `job_id`. |
| `read_file` | Required `path`. |
| `write_file` | Required `path` and full `content`; can create files. |
| `list_files` | `path` defaults to `/workspace`. |
| `find` | Required `pattern`; directory `path` defaults to `/workspace`; `limit` defaults to 100 (1–1000, zero uses default). |
| `grep` | Required nonempty `pattern`; file or directory `path` defaults to `/workspace`; optional `glob`; `literal` defaults to false; `context` defaults to 0 (0–10); `limit` defaults to 100 matching lines (1–1000, zero uses default). |
| `edit` | Required existing-file `path`; either `old_text` and `new_text`, or `replacements` (1–100 objects with both fields). Do not mix forms. |

Relative paths start at `/workspace`. Absolute paths use the backend's namespace
and restrictions. Local maps `/workspace` to the run directory, but **does not
provide isolation** and permits other host paths. Landlock rejects paths outside
its work directory, including symlink escapes. Guest backends use guest paths.

Search returns JSON text with `results`, `truncated`, and a `reason` when partial.
Paths are relative to the selected directory, with slash separators. Single-file
`grep` uses the basename. Structured records preserve filenames with newlines.
Traversal is lexical, excludes `.git` directories, includes other dotfiles,
ignores non-regular files, and does not follow descendant symlinks. The selected
root follows the backend's normal path policy. **These tools do not honor
`.gitignore`.** Narrow the path or glob for large trees.

Globs use slash-separated segments: `*`, `?`, and character classes match within
one segment. A segment exactly `**` matches zero or more directories. Thus
`*.go` matches only root files; `**/*.go` also matches nested files. Absolute,
malformed, blank, and `.` or `..` segment patterns are rejected. `grep` uses Go
regular expressions by default, or an exact case-sensitive substring with
`literal: true`. It searches each line, not across lines. Each matching line is
one result, with `path`, one-based `line`, `text`, and `before`/`after` arrays of
line/text records. CR is removed only as part of CRLF; a final line needs no LF.
No matches is a successful empty result.

Search stops at 10,000 visited entries, 30 seconds (or the caller's earlier
deadline), the requested result limit, or the 32 KiB response limit, including
JSON framing. Count and byte limits stop after evidence of an omitted result.
NUL-containing binary files are skipped. Files above 1 MiB are skipped and make
results partial. A very long result line can exhaust the response limit without
being included. Visit/size limits state the partial reason; filesystem errors,
cancellation, and deadlines are tool errors, not complete search results.
Existing `ReadFile` operations read whole files: these ceilings bound accepted
input, work, and model output, **not backend allocation or process memory**.

For a single exact edit:

```json
{"path":"main.go","old_text":"return false","new_text":"return true"}
```

For several regions validated against the original file:

```json
{"path":"main.go","replacements":[{"old_text":"oldName","new_text":"newName"},{"old_text":"obsolete comment","new_text":""}]}
```

`edit` accepts UTF-8 files without NUL, at most 1 MiB. Empty `new_text` deletes;
omitted `new_text` is an error. Empty `old_text` is an error. Each old text must
occur exactly once, including overlapping occurrences. Regions must not
overlap; adjacent regions are valid. Inserted text is never searched by later
replacements in the request. All regions are validated before one write.
Validation errors do not write, and a missing file is not created. Success
reports the requested path, replacement count, and resulting byte count.

A context-cancellable gate serializes `edit` and `write_file` within one tool
set. It does not coordinate shell commands, custom tools, separate handles or
processes, or external writers. Reads and enumeration are not a snapshot.
Writes are not compare-and-swap transactions. A write failure does not promise
rollback; read and check current content before retrying an uncertain edit.
There is no automatic edit approval policy, and no exactly-once guarantee
across a crash between a write and journal persistence. Custom providers can
keep the required `Sandbox` interface unchanged; enumeration uses optional
`DirectoryReader`, or a POSIX `sh` fallback through sandbox `Exec`.

## Inject environment values carefully

`bonnie.WithSandboxEnv` injects fixed values into sandbox commands. Repeated calls merge values; later keys replace earlier ones. An injected value wins over a per-command value with the same name.

This is not secret hiding. A model-selected command can print any environment value it receives, send it over an allowed network connection, or write it to a result file. Give the sandbox only narrowly scoped, short-lived credentials when necessary. Keep model provider and channel credentials on the host.

## Shared files and cleanup

`WithSharedDirectory("./working-files")` is an explicit single-user development mode. It uses one directory directly for all runs. The default Landlock boundary remains, but run files are no longer separate. Explicit Landlock and Local providers are supported; Docker and Microsandbox are rejected.

Do not combine shared storage with `WithContextFiles` or automatic sandbox cleanup. Overlapping opens through the same provider are rejected, not queued. Separate providers or processes are not coordinated. Run pruning never deletes the shared directory.

For isolated runs, cleanup is opt-in. Stop the owning server before manual pruning:

```bash
bonnie sandbox prune --journal .bonnie --sandbox docker --dry-run
bonnie sandbox prune --journal .bonnie --sandbox docker
```

Choose the backend the runs used. Pruning keeps pending, running, and waiting runs. It deletes eligible finished-run sandboxes, not the journal or channel address. A later turn on a completed run can start without its earlier files. Publish output before cleanup. See [Deployment](/guides/deployment) for retention-based automatic cleanup.

Read each provider's [godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/sandbox) before deployment. If a backend cannot enforce your policy, fix the backend or policy. Do not replace it with Local merely to remove the error.
