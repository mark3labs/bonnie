---
title: NATS run API
description: Inspect runs and follow NDJSON events through the existing Go client over agent-specific Core NATS subjects.
---

# NATS run API

The optional `channel/natsapi` transport exposes every existing `/bonnie/v1`
operation through Core NATS, reusing the authenticated HTTP handler. It is
separate from [NATS tasks](/channels/nats): tasks start independent work;
same-run messages and suspension answers use the run API.

## Host and client

Mount `bonnie.WithNATSAPI(natsapi.Config{Conn: nc, Subject:
"bonnie.api.agent.worker-1", Authenticator: verifyOperatorBearer})`.
The verifier must return a nonnil verified principal. Broker login is not
publisher identity or API authentication. Host metadata and outbound dispatch
are passed to the shared handler; addresses use the `nats-api` namespace.

Add `bonnie.WithHTTPListenerDisabled()` for an agent with no TCP HTTP port.
This explicit option rejects web UI, injected listeners and TCP webhook routes.
Internal scheduling still works; external schedule HTTP triggers are unavailable.
Default HTTP serving remains unchanged. Connections are caller-owned and must
remain open until channel shutdown finishes.

```go
transport, err := natsapi.NewTransport(natsapi.Config{
    Conn: nc, Subject: "bonnie.api.agent.worker-1",
})
if err != nil { return err }
defer transport.Close()
c := client.New("http://bonnie-over-nats",
    client.WithHTTPClient(&http.Client{Transport: transport}),
    client.WithBearerToken(operatorToken))
info, err := c.Info(ctx)
```

Import `client/natsapi` for the transport and `channel/natsapi` for the host
configuration. Request contexts bound ordinary calls. Do not use an overall HTTP
client timeout for long-lived streams. Existing snapshot, message, explicit
respond, cancellation, submissions and child-run methods keep their HTTP behavior.
An application can put its own HTTP/SSE gateway in front; browsers need no NATS
credentials and BONNIE adds no UI or application workflow.

## Deployment security and routing

Use exactly one owner per literal agent subject, never a shared worker queue.
Select the agent owning the journal explicitly. Separate API permissions from
task submission. API access grants operator access to all runs, not per-run
ownership authorization. Health remains public to broker-authorized callers.

Clients publish only to their assigned agent subject and its `.control` subject,
and subscribe only to authorized inboxes. Agents subscribe to their exact
API/control subjects and publish only to allowed inboxes. Default reply prefix is
`_INBOX.bonnieapi`; configure `InboxPrefix` on both ends for per-client ACL domains.
Use restrictive ACLs and TLS: broad inbox reading exposes private conversations,
and API/control eavesdropping exposes bearer credentials and control capabilities.
Frames are not independently signed. Do not trust caller-supplied identity headers.

## Streams and failures

Version-one JSON framing sends ordered headers, bounded data chunks, heartbeats
and a terminal end or error. Random cancellation capabilities, replay-resistant
controls, renewable leases, bounded observer queues and ACK credit protect
resources. Oversized encoded envelopes (including base64/JSON overhead) fail;
large snapshots are chunked. Slow consumers lose their observation and reconnect.
Frame sequences are not journal cursors. Missing or reordered frames fail reads,
not clean EOF. Closing an observation cancels its HTTP context, not the agent turn;
use the explicit turn-cancel operation to stop work.

Disconnects fail existing requests. Reconnection permits new calls, never retries
mutations or resumes a damaged HTTP body. A lost reply can hide an executed
mutation. `Client.Stream` exposes failure as channel closure, so callers reopen
explicitly with their last durable cursor. Caller-owned NATS handlers/connections
are not replaced or drained.

The local journal supplies durability. Snapshots retain message content parts,
including tool calls/results. Live reasoning and tool activity are not a full
historical trace; durable cursor replay cannot recreate missed live-only activity.

See [the protocol and limits specification](https://github.com/mark3labs/bonnie/blob/master/docs/NATS_API.md)
for wire fields, default budgets, leases, header restrictions and ACL details.
