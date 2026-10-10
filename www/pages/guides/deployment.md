---
title: Deployment
description: Build and operate a BONNIE agent with persistent storage, authentication, sandbox controls, and safe recovery.
---

# Deployment

BONNIE is experimental, pre-1.0 software. APIs can change without notice, and no release is proven in production. Test recovery and external effects before you deploy. Do not use a release for work whose loss would cause harm.

A deployment needs more than the static executable. It needs persistent journal storage, sandbox working files, model credentials, a compatible Linux host, and a protected inbound API.

## Build the agent

From the agent tree:

```bash
go mod tidy
go test ./...
bonnie build --dry-run
bonnie build --output bin/report-agent
```

Build regenerates tool wiring, embeds instructions, skills, and context files, and disables CGO. The target does not need Go or the BONNIE CLI. Build for the target architecture; official release platforms are Linux amd64 and arm64.

Pin released dependencies and inspect `go.mod` and `go.sum`. Do not ship local replacement directives. Test any custom dependencies against the CGO-free build. Never embed secrets in instructions, skills, or context files.

The target still needs the selected sandbox's prerequisites. Landlock needs a compatible kernel and host shell. Docker needs its CLI, daemon, and images. Microsandbox needs a working runtime and the verified Linux KVM setup. See [Sandboxes](/guides/sandboxes).

## Set stable storage and an address

Use explicit paths in `main.go` for deployment. For example, add:

```go
// Options inside bonnie.New(...):
bonnie.WithJournal("/var/lib/bonnie/report-agent"),
bonnie.WithAddr("127.0.0.1:8080"),
```

The default address for a compiled agent is `:8080`, not loopback-only. The default journal is `.bonnie`, relative to the process working directory. Set the working directory deliberately. Start only one execution owner for the journal and working files.

The compiled agent reads `--addr`, `--model`, and `--sandbox` operator flags. Address and model flags override their Go options. Sandbox selection is limited to providers declared in code. Run `./report-agent --help` to inspect them. A compiled agent does not inherit all the flags of `bonnie serve`; set its journal in code.

The disk instructions file takes precedence over embedded instructions when present. A populated authored skill directory also takes precedence over embedded skills. Use a controlled working directory with no unintended tree files. Embedded skills are unpacked beside the journal when no authored set is available; embedded context files seed working data without overwriting it.

Protect the journal directory with permissions for the service account only. Journal records contain conversation data and tool results. Persist both the journal and the backend's working-file storage. Docker containers and microVM resources are not contained in `journal.db`.

## Authenticate HTTP callers

Without `WithHTTPAuthenticator`, the HTTP API authenticates nobody. Its routes can start runs, read history, answer approvals, cancel work, and retire conversations. Keep an unauthenticated server on loopback behind a trusted access layer, or configure an authenticator. Do not expose the raw port to untrusted callers.

This complete example uses one shared service identity and fails closed when its token is absent:

```go
package main

import (
    "crypto/subtle"
    "net/http"
    "os"

    "github.com/mark3labs/bonnie"
    "github.com/mark3labs/bonnie/channel"
    bonniehttp "github.com/mark3labs/bonnie/channel/http"
)

func main() {
    token := os.Getenv("BONNIE_API_TOKEN")
    bonnie.New(
        bonnie.WithJournal("/var/lib/bonnie/report-agent"),
        bonnie.WithAddr("127.0.0.1:8080"),
        bonnie.WithHTTPAuthenticator(func(r *http.Request) (*channel.Principal, error) {
            expected := "Bearer " + token
            if token == "" || subtle.ConstantTimeCompare(
                []byte(r.Header.Get("Authorization")), []byte(expected),
            ) != 1 {
                return nil, bonniehttp.ErrUnauthenticated
            }
            return &channel.Principal{
                Authenticator: "service-token",
                Kind:          "service",
                ID:            "report-operator",
            }, nil
        }),
    ).Serve()
}
```

Use a secret store to supply the token, and TLS at the access layer. This token gives all holders the same identity; it is not a multi-user authorization system. For a multi-user service, verify distinct identities and enforce the access rules your application needs. Do not trust a caller-supplied identity header unless a trusted proxy validates it and direct access is blocked.

`ErrUnauthenticated` becomes HTTP 401. Other verifier errors become 500. A nil principal with a nil error allows unattributed access. Avoid that result when identity is required. `GET /bonnie/v1/health` is exempt from HTTP authentication.

`operation_id` needs a proven owner. An access proxy alone does not make this field usable if the BONNIE HTTP channel still has no authenticator. Supply identity to BONNIE through a verifier when you need owner-scoped idempotency.

Chat adapters verify platform signatures or secrets. That proves the platform delivery, not every user's right to invoke a dangerous host tool. Apply resource and user authorization in trusted code. Schedule trigger authorization is a separate callback; see [Scheduling](/guides/scheduling).

## Manage credentials and sandbox permissions

Set model and channel credentials through the service environment or secret store. BONNIE also reads `.env` in the working directory; exported values win. Protect that file and keep it out of version control.

Do not run as root. With Landlock, use an account that cannot reach privileged daemon sockets, including the Docker socket. The default boundary confines files, not the network or kernel. Use Docker or Microsandbox with an explicit egress policy when the threat model needs it.

Host provider keys do not automatically enter the sandbox command environment. `WithSandboxEnv` explicitly injects values, but commands can print or transmit them. Supply only the minimum sandbox credentials. Custom Go tools still have host access and need their own controls.

Approval tools do not automatically authorize external actions. A halting tool does not block another tool in the same step. Enforce approval in the action tool, not only in instructions.

## Supervise and stop the process

Run the executable under a service manager with a fixed working directory, persistent storage, protected environment, and a restart policy. Send SIGTERM for a normal stop. `Agent.Serve` owns signal handling and waits for in-flight work to reach checkpoints; the default shutdown timeout is 30 seconds. Configure `WithShutdownTimeout` when necessary.

Model calls, custom tools, and callbacks must observe their contexts. A callback that ignores cancellation can prevent a timely stop. Budget the service manager's stop timeout for BONNIE's shutdown phases. A forced kill can lose the in-progress step and can leave an external action with no saved result.

A host that owns its own process lifecycle uses `Agent.Run(ctx)`. That method does not parse flags, install signal handling, or exit the process.

**SQLite integrity is not turn coordination.** SQLite serializes writes and rejects duplicate record sequence numbers, but two servers executing the same run can interleave work. Keep one owner. Schedule locking adds exclusive scheduler ownership; it does not make the rest of the runtime a distributed agent coordinator. Use local storage with working POSIX locks, not an unsafe network filesystem.

## Advertise agent presence

Presence is optional current-state discovery. It lets a caller find an agent by identity, labels, state, and endpoint capabilities. It does not assign work, coordinate run execution, or provide failover. Hosts without `WithPresence` keep their existing behavior.

Create a registry, then pass it to the host. This example uses a caller-owned authenticated NATS connection `nc` and a host context `ctx`:

```go
// Import time, github.com/mark3labs/bonnie/presence, and
// presencenats "github.com/mark3labs/bonnie/presence/nats".
registry, err := presencenats.New(ctx, nc, presencenats.Config{
    Bucket: "agents",
    TTL:    30 * time.Second,
    Create: true,
})
if err != nil {
    return err
}
agent := bonnie.New(
    bonnie.WithPresence(bonnie.PresenceConfig{
        Registry:        registry,
        AgentID:        "report-agent-1",
        Labels:          map[string]string{"region": "eu-west"},
        RefreshInterval: 10 * time.Second,
        Endpoints: []presence.Endpoint{{
            Channel:  "http",
            Address:  "https://agent.example.com/bonnie/v1/runs",
            Input:    true,
            Delivery: true,
        }},
    }),
)
return agent.Run(ctx)
```

The host does not close `nc`. Keep the connection available through shutdown. JetStream must be enabled on the broker. `Bucket` is required and defines the discovery scope. `Create: true` permits creation of a missing file-backed KV bucket; otherwise provision it first. TTL defaults to 30 seconds, must be positive, and must match an existing bucket. Existing buckets keep their storage type and history; the store does not validate these settings. Restrict bucket access with NATS permissions. Labels are metadata, not authorization claims.

`AgentID` is the stable agent name. Omit `InstanceID` to generate a random ID for each host run. A different live instance cannot claim the same agent name: registration returns `presence.ErrConflict`. Use `errors.Is` to test this error. A refresh updates the same instance's record and renews its TTL. An old instance cannot unregister its replacement. Expiry permits a new instance to register, but does not prove that the old process or its tools stopped.

The host registers `ready` after listener binding and channel startup, before HTTP serving starts. Each refresh reads channel endpoint readiness again. Registration or refresh errors stop `Agent.Run` and return an error. During shutdown, the host stops refresh, advertises `draining` with endpoints not ready, shuts down lifecycle channels, and unregisters. These cleanup operations share a context bounded by `WithShutdownTimeout`; errors are returned. A forced kill leaves the KV record until TTL expiry.

### Endpoint capabilities

When `Endpoints` is omitted, the host collects endpoints from mounted `channel.PresenceProvider` implementations, including its built-in HTTP channel. A non-empty override replaces the complete list, not only HTTP. Supply public URLs explicitly: the host does not convert bind addresses or webhook paths to public URLs. Override readiness follows host state, not remote health.

| Channel | Advertised address | Readiness and delivery |
| --- | --- | --- |
| HTTP | `/bonnie/v1/runs` | Local input and response delivery |
| Slack, Discord, Telegram | Configured webhook path, or its default | Local input; delivery when a bot token is configured |
| GitHub | `/github/events` | Local input; delivery when App ID and private key are configured |
| NATS | Task subject; with targeted tasks, `Subject + ".agent." + AgentID` | Input and delivery; ready when connected and not stopped |

`Input` and `Delivery` are independent capability flags. `Ready` reports current input readiness, not guaranteed execution or remote platform health. Built-in providers do not publish credentials. Keep secrets out of custom labels and endpoint overrides too. A channel's non-empty `channel.AgentIdentity()` must match `PresenceConfig.AgentID`; use the same agent ID in `WithNATS`.

### Discover and watch agents

The NATS store implements `presence.Registry`, `presence.Discoverer`, `presence.Watcher`, and `presence.TTLStore`. Discover returns live records in agent, then instance, order. Empty filter fields impose no restriction; labels select exact values.

```go
agents, err := registry.Discover(ctx, presence.Filter{
    State:  presence.Ready,
    Labels: map[string]string{"region": "eu-west"},
})
if err != nil {
    return err
}
_ = agents
snapshot, changes, err := registry.Watch(ctx, presence.Filter{})
if err != nil {
    return err
}
_ = snapshot // Initialize the caller's view before processing changes.
for event := range changes {
    if event.Err != nil {
        return event.Err // Start a new watch to restore the current view.
    }
    // Add or update event.Record by Identity.
    // If event.Deleted, remove event.Record.Identity from the view.
}
```

Watch polls at `min(TTL/3, one second)`. It emits additions and timestamp updates, including refreshes. When a record expires, is removed, or stops matching the filter, a deletion carries its last matching record. Cancel `ctx` to close the stream. A discovery error produces a terminal `Event.Err` and closes the NATS watch. This is not a durable history: short-lived changes between polls can be missed. Do not use a deletion event to reassign tasks automatically.

For process-local tests, `presence.NewMemoryStore()` provides registration, discovery, and a polled watch. It stores an explicitly supplied `Record.ExpiresAt`, but has no default TTL or lease renewal. Host registration does not set expiry for this store. It is not cross-process discovery.

See [Options](/reference/options#agent-presence) for all host fields, [NATS](/channels/nats#targeted-tasks) for targeted submission, and the [presence godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/presence) for custom stores.

## Migrate worker identity names

The worker-to-agent rename changes public API names, presence identity fields, JSON fields, NATS subjects, and presence keys. There are no compatibility aliases. Rename `WorkerID` to `AgentID`, `channel.WorkerIdentity` to `channel.AgentIdentity`, `presence.Identity.Worker` to `presence.Identity.Agent`, and `presence.Filter.Worker` to `presence.Filter.Agent`. Change JSON `worker_id` to `agent_id` and `worker` to `agent`. Update targeted subject publishers from `.worker.<id>` to `.agent.<id>`, and provision the new `.agent.*` stream filters and matching ACLs.

Old persisted NATS result, admission, and status JSON is not decoded compatibly. Drain and stop the old deployment, back up each local journal and NATS task cache, then register presence again under the new identity. Provision stream subjects and permissions, and upgrade publishers and clients together before resuming traffic. Use a clean local NATS task cache/journal or an explicit offline migration that preserves run data. Do not blindly delete a journal; it can hold durable run history. New presence keys use the `a_` prefix instead of `w_`.

## Verify health and recovery

```bash
curl -fsS http://127.0.0.1:8080/bonnie/v1/health
curl -fsS -H "Authorization: Bearer $BONNIE_API_TOKEN" \
  http://127.0.0.1:8080/bonnie/v1/info

curl -sS -H "Authorization: Bearer $BONNIE_API_TOKEN" \
  -H 'Content-Type: application/json' \
  http://127.0.0.1:8080/bonnie/v1/runs \
  -d '{"address":"deployment-check","text":"Reply with a short greeting. Do not run tools."}'
```

Health is a liveness response with no journal read. It does not prove that the provider, disk, or sandbox works. Run a separate harmless sandbox test and a restart test before accepting traffic. Verify that a waiting run can be answered after restart.

Inspect state even while the server is stopped:

```bash
bonnie runs list --journal /var/lib/bonnie/report-agent
bonnie runs list --journal /var/lib/bonnie/report-agent --state waiting
bonnie runs show --journal /var/lib/bonnie/report-agent RUN_ID
```

Completed steps replay with full typed tool-call data. The journal cannot guarantee exactly-once external effects when a process stops between an external action and its saved result. Use service idempotency keys and receipts for important effects. Ordinary runs do not become an automatic retry queue merely because the journal is durable.

NDJSON streams can reconnect with their last `cursor`. Durable events replay across restart; live-only deltas do not. A restarted waiting run still requires an explicit answer. Cancellation keeps saved work and files; retirement permanently prevents new turns on that run.

## Back up and upgrade

The SQLite journal is `<journal>/journal.db`, uses WAL, and fsyncs commits by default. Do not copy only the database file from a live process and assume you have a consistent backup. Use SQLite's supported online backup procedure, or stop the owner and take a consistent backup of the journal directory and associated working files. Secure backups as private conversation data.

Before an upgrade:

1. Inspect active, waiting, and scheduled work.
2. Stop the owner cleanly and back up storage and working files.
3. Record the executable, dependency versions, sandbox configuration, and schedule revisions.
4. Test the new binary against a backup in an isolated environment with no real external effects.
5. Start one owner and verify health, sandbox access, and parked-run recovery.

Older JSONL journals are imported on first open. Source files keep their sequence numbers and are renamed with `.jsonl.imported`; import is idempotent. Keep an untouched backup before migration. Do not assume an older executable can read a database changed by a newer release.

## Retain or remove working files

Automatic cleanup is opt-in for compiled agents:

```go
// Import time; add inside bonnie.New(...).
bonnie.WithRunSandboxCleanup(bonnie.SandboxCleanupPolicy{
    CompletedAfter: 24 * time.Hour,
    RetiredAfter:   24 * time.Hour,
}),
```

Zero keeps files for that state. You can also set `FailedAfter` and `CancelledAfter`. Negative durations are refused. Cleanup runs at startup and once per minute, keeps pending/running/waiting runs, and preserves journal history and channel addresses. Publish output before completion: a later turn on a cleaned-up run starts without old files.

Cleanup cannot be combined with shared storage or `WithAgentFactory`. Providers must support deletion. Each deletion has a 30-second timeout; errors are logged and retried. Cleanup locks do not coordinate different processes. `bonnie serve` does not run this automatic sweep; manual `bonnie sandbox prune` requires the owning server to be stopped.

## Logs and operating limits

Activity logging is off by default. Enable it with:

```go
bonnie.WithActivityLogger(bonnie.NewActivityLogger(nil)),
```

Info logs include final responses and run activity. Debug logs can include prompts, tool arguments, results, and reasoning. Protect and limit log retention. Logging is synchronous, so a slow writer delays work. Journal replay does not log old activity again.

Monitor disk space, failed runs, waiting approvals, sandbox resources, provider errors, and schedule delivery failures. Core NATS does not retain offline tasks or retry result delivery; durable run state alone does not make it a durable broker queue. If you deploy NATS, read the adapter's current Core NATS and JetStream contracts separately.

See [Troubleshooting](/guides/troubleshooting) and the [runtime godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/runtime) for recovery controls and limits.
