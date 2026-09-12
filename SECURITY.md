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
  `--sandbox-deny-network`, or an allow-list on microsandbox.
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

One process must own a run at a time.

The file journal takes no cross-process lock. Two processes that write the same
run interleave their records and give the same sequence number to different
records. The file stays readable, but the replay is then wrong, and the run can
repeat work it already did.

Examples of unsafe patterns:

- Two load-balanced instances each resuming the same run.
- One process resuming while another is still in a turn.

Safer patterns:

- Use a distributed lock (etcd, Redis, database) to elect a single owner.
- Or, make sure only one process can reach the journal directory.
- Or, use `MemoryJournal` for ephemeral test runs only.
