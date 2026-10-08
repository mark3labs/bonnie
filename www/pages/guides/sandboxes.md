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

Container and microVM commands use `/workspace`. Host-mapped backends report their actual per-run host path. Use relative paths in instructions and skills. Local and Landlock file operations reject working-directory escapes with `ErrOutsideWorkDir`, including symlink escapes. Docker and Microsandbox can access absolute paths inside their guest filesystem; their boundary is the container or microVM, not only `/workspace`.

The shell tool detects Bash on each call and uses `sh` when Bash is absent. Results name the selected shell. A failed command is not retried under a second shell. For portable commands, use POSIX shell syntax or select an image with Bash. Standard tool output is bounded; save large artifacts to files and publish them through a trusted mechanism.

`context/` seeds are copied without overwriting existing run files. They are not shared mutable output. A low-level host can use `sandbox.Seeded(provider, dir)` for the same copy behavior.

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
