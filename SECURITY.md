# Security Policy

## Reporting a vulnerability

Do not open a public issue for a security vulnerability.

Use GitHub private security advisories: visit
https://github.com/mark3labs/bonnie/security/advisories and click
"Report a vulnerability" at the top.

The maintainers will reply as soon as they can. Do not assume a fix is in
progress until a maintainer confirms it.

## Supported versions

BONNIE is pre-1.0 and follows semantic versioning.

Only the latest tag receives security fixes. If you run an older version, upgrade.

| Version | Supported |
|---------|-----------|
| 0.1.x   | ✅ Yes    |
| 0.0.x   | ❌ No     |

## Security caveats

⚠️ **Important: read before deployment.**

### No sandbox in v0.1.0

BONNIE executes tool calls that the language model chooses.

**Sandboxing exists but is opt-in.** Without it, tool calls run as the BONNIE
process, with its files, its network, and its credentials.

Turn it on:

```sh
bonnie serve --sandbox docker --sandbox-deny-network
```

Or, in an agent tree's `main.go`:

```go
bonnie.New(
	bonnie.WithSandbox(sandbox.Docker()),
	bonnie.WithNetwork(sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}),
).Serve()
```

```go
runner := runtime.NewRunner(journal, sandbox.Agent(sandbox.Docker(), opts...))
```

See `docs/SANDBOX.md`. Three backends ship: `Local` (no isolation, for
development only), `Docker` (container namespaces), and `Microsandbox`
(microVM with a guest kernel).

Even with a sandbox, the host is responsible for the rest:

- Docker isolates with namespaces and cgroups, not a guest kernel. Use
  microsandbox when the threat model includes hostile code.
- Sandbox egress is open unless you set a policy. Use
  `--sandbox-deny-network`, or an allow-list on microsandbox, or
  `bonnie.WithNetwork` in a tree. They are the same control with the same
  refusals: a mode a backend cannot enforce is an error, never a silent
  default, and a policy with no sandbox to enforce it is refused at startup.
- Run BONNIE itself in a container or VM that limits system calls, file
  access, and network.
- Do not give BONNIE credentials that your application does not also hold.

This is a real risk, not a hypothetical one. BONNIE's own live-model test once
ran without a sandbox and the model wrote a `Dockerfile`, a `terraform/`
directory, and deployment scripts into the repository working directory.
Nothing failed and nothing warned. See `docs/SPEC.md` §4.9.

### HTTP channel: no authentication verification in v0.1.0

When you use `channel/http`, the HTTP channel carries a Principal for audit and
access control. **The channel does not verify it.**

You must authenticate requests **before they reach the HTTP channel**. Examples:

- Run the server behind a load balancer that checks OAuth tokens or API keys.
- Require authentication in your reverse proxy or gateway.
- Use a service mesh that enforces identity.

Failing to do so means any client can send requests on behalf of any principal.

### Chat channels: platform signatures are verified, users are asserted

The Slack, Discord, and Telegram channels verify every webhook request
against the platform's scheme — Slack's v0 HMAC over the raw body with a
five-minute replay window, Discord's Ed25519 over the timestamped body,
Telegram's shared-secret header. A channel whose verification credentials
are missing refuses to serve at startup; it will not run wide open.

This verifies **the platform, not the person**. A user ID inside a verified
Slack event is Slack's word about who typed. The `Principal` recorded on the
run carries that assertion, and a tool that makes per-tenant decisions from
it should treat it as such.

### Journal security

The journal directory holds the full conversation content in plain text:
- User messages
- Model outputs
- Tool call arguments
- Tool results

**Protect the journal directory with file permissions.** Example:

```sh
mkdir -p .bonnie
chmod 700 .bonnie
```

Do not commit the journal to version control.

### Run ownership

The journal can no longer be corrupted by two writers. It still does not
coordinate them.

The SQLite journal serialises write transactions — inside this process and
across processes — and its `(run_id, seq)` primary key makes a reused
sequence number a constraint violation rather than silent corruption. Two
processes appending to one run therefore produce a dense, correct record
sequence. The `flock` per run that earlier versions took, and the
`ErrRunOwnedElsewhere` refusal it produced, are gone: the problem they
mitigated no longer exists.

Reads are never blocked. WAL mode lets any number of readers work while a
writer commits, which is what keeps `bonnie runs show` usable against a busy
server.

Limits, stated plainly:

- **Journal integrity is not turn coordination.** Two servers that both
  execute a turn for the same run write a well-formed journal holding an
  interleaved conversation. Nothing in BONNIE stops them. Route a run to one
  owner.
- **SQLite locking is per host.** It relies on working POSIX advisory locks.
  On a network filesystem that does not provide them, the database can be
  corrupted. Put the journal on local storage, or write a `Journal` backed
  by a networked database.

Safer patterns:

- Route every request for a run to the instance that owns it.
- Or use a distributed lock (etcd, Redis, a database) to elect a single
  owner.
- Or use `MemoryJournal` for ephemeral test runs only.
