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
# or, from an agent manifest (bonnie serve --agent):
#   sandbox:
#     kind: docker
#     network:
#       mode: deny-all
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
  `--sandbox-deny-network`, or an allow-list on microsandbox. The manifest's
  `sandbox.network` key is the same control and the same refusals: a mode a
  backend cannot enforce is an error, never a silent default.
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

The file journal enforces this on one host. The first write to a run takes an
exclusive `flock` on `<root>/runs/<run-id>.lock`; another process — or another
journal instance in the same process — that writes the same run is refused
with `ErrRunOwnedElsewhere` instead of being allowed to interleave records and
give the same sequence number to different ones. The kernel releases the lock
when a process dies, so a crash needs no lock recovery.

Reads are never locked: any process can list runs or replay a run it does not
own, which is what keeps `bonnie runs show` working everywhere.

Limits:

- The lock is per host. On a network filesystem without working `flock`
  support it enforces nothing.
- Refusing a write is not coordination. Two load-balanced instances that both
  need to write the same run still need one owner in front.

Examples of what the lock now prevents, and what it does not:

- Two load-balanced instances each resuming the same run — the second is
  **refused loudly** with `ErrRunOwnedElsewhere`. Route the request to the
  instance that owns the run, or share a journal that can coordinate.
- A shared network filesystem — the lock says nothing there; use a journal
  backed by a database or a coordinator (etcd, Redis) to elect one owner.
- One process resuming while another is still in a turn — refused, as above.

Safer patterns:

- Use a distributed lock (etcd, Redis, database) to elect a single owner.
- Or, make sure only one process can reach the journal directory.
- Or, use `MemoryJournal` for ephemeral test runs only.
