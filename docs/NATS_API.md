# Run API over NATS

`channel/natsapi` is an optional **Core NATS** transport for the complete existing
`/bonnie/v1` HTTP API. It reuses `channel/http.HandlerWithOutbound`; there is no
second set of operation handlers. `client/natsapi.Transport` implements
`http.RoundTripper`, so existing Go client methods work unchanged, including
snapshot, same-run messages, explicit suspension answers, turn cancellation,
submissions, children, and NDJSON cursor streams.

This is separate from `channel/nats` and `client/nats`, which still deliver
independent tasks/results and optional JetStream controls. A task is not a
same-run message or an approval. Neither transport defines application workflows.

## Agent setup

Connections are required and **caller-owned**. Configure TLS and credentials when
connecting to NATS; BONNIE never takes over connection handlers or closes/drains
those connections. Keep them open until channel/host shutdown completes.

```go
verify := func(r *http.Request) (*channel.Principal, error) {
    // Compare against your securely supplied token, or verify an OIDC token.
    // Never trust a caller-supplied user/identity header.
    return verifyOperatorBearer(r.Header.Get("Authorization"))
}
agent := bonnie.New(
    bonnie.WithNATSAPI(natsapi.Config{
        Conn: nc,
        Subject: "bonnie.api.agent.worker-1",
        Authenticator: verify,
    }),
    bonnie.WithHTTPListenerDisabled(),
)
err := agent.Run(ctx)
```

`WithNATSAPI` passes host information and the outbound registry to the reused
handler. Standalone `natsapi.New(runner, cfg)` accepts `Info` and `Outbound`.
Addresses have namespace `nats-api`, separate from HTTP and task addresses.

`WithHTTPListenerDisabled` is explicit and optional; ordinary hosts still bind
HTTP by default. It rejects injected listeners, the web UI, and mounted channels
with TCP routes rather than dropping them. Internal scheduler and schedule clock
execution remains available; external schedule HTTP triggers are unavailable.
Lifecycle channels stop before the journal closes. No TCP endpoint is advertised.

## Existing client

```go
transport, err := natsapi.NewTransport(natsapi.Config{
    Conn: nc,
    Subject: "bonnie.api.agent.worker-1",
})
if err != nil { return err }
defer transport.Close()
c := client.New("http://bonnie-over-nats",
    client.WithHTTPClient(&http.Client{Transport: transport}),
    client.WithBearerToken(operatorToken))
info, err := c.Info(ctx)
```

Use request contexts for ordinary deadlines, **not an overall HTTP client timeout**
for streams. Close response bodies and the transport. Opening an observation
stream flushes headers before the first event. Closing it cancels the remote HTTP
context, **not the agent turn**. Explicit `/runs/{id}/cancel` remains the durable
turn-control operation. A synchronous mutation may be interrupted according to
existing HTTP behavior when its request context ends.

An application may serve its own HTTP/SSE gateway in front of this client; no
BONNIE UI or browser NATS credentials are needed. The generic package example is
compiled in the main module. The API is released in v0.27.0; a scaffolded tree
must pin v0.27.0 or later to use it.

## Routing, authentication, and broker ACLs

Assign one literal subject to each **journal-owning agent**, for example
`bonnie.api.agent.worker-1`. Exactly one bridge owns that subject; duplicate
owners are a deployment error, not load balancing. Do not use shared worker
queues. Clients select their explicit agent; BONNIE does not discover run owners.

The bridge requires an HTTP authenticator that returns a nonnil verified
principal; nil identity is unauthorized. Existing HTTP error mapping applies.
Health remains public to broker-authorized callers. API access grants operator
access to all runs; **per-run ownership authorization is not supplied**. Put any
application-specific access policy in front of the bridge. Verifiers relying
on TCP/TLS peer certificates cannot work without a different credential scheme.

Broker authentication does not establish publisher identity to the subscriber.
Use both API credentials and restrictive broker ACLs/TLS. Separate API permission
from task submission permission: API access exposes private conversation content
and mutations. With subject `S` and inbox prefix `P`:

- API client: publish only `S` and `S.control`; subscribe only `P.*`.
- Agent: subscribe only `S` and `S.control`; publish only authorized `P.*`.
- Do not grant other users publish access to private inboxes or subscribe access
  to API/control subjects. Frames are not signed independently of broker ACLs.

Default inbox prefix is `_INBOX.bonnieapi`. For distinct security domains set
`InboxPrefix` on both ends to an explicitly allowed per-client prefix and
restrict ACLs accordingly (use separate bridges/agent subjects where different
prefixes are needed). Broad inbox read permissions expose private responses;
API/control eavesdropping exposes bearer credentials and cancellation secrets.

Only GET, HEAD, POST and DELETE, origin-form paths under `/bonnie/v1/`, and
Authorization, Content-Type, Accept headers are accepted. Optional
`AuthenticationHeaders` permits additional credential headers, not Host,
hop-by-hop, proxy, forwarded/identity, or NATS headers. Invalid paths, dot
segments, CR/LF and invalid reply routes are rejected; an invalid inbox receives
no reply. The HTTP mux retains normal 404/405 operation routing. Encoded address
slashes and cursor queries are preserved. This is not an arbitrary HTTP proxy.

## Version 1 framing

All messages are strict JSON. Unknown fields and trailing JSON are rejected.

Request fields: `version` (1), `request_id` (random 128-bit hexadecimal token),
`method`, `path` (including query), `headers`, `body` (base64 bytes), optional
`timeout_ms`, and `secret` (random 256-bit hexadecimal capability). NATS reply
subject must be exactly `InboxPrefix + "." + random128BitHexToken`.

Frames contain `version`, `request_id`, `sequence` (uint64 starting at zero),
`kind`, and optional `status`, `headers`, `data`, `error`. Headers also negotiate
`lease_ms`, `renew_ms`, and `window`. Kinds are `headers`, `data`, `heartbeat`,
`end`, `error`. One final headers frame commits the status; data is incremental,
end alone means successful EOF. Heartbeats can precede headers during synchronous
operations. Informational HTTP statuses are suppressed; HEAD and no-body status
semantics are maintained. Response headers are limited to Content-Type,
Cache-Control, Allow, WWW-Authenticate, Retry-After.

Protocol rejection is terminal `error` at sequence zero. Post-header failures
terminate with an error when publication remains possible. Stable error codes:
`invalid_request`, `unsupported_version`, `overloaded`, `too_large`,
`lease_expired`, `slow_consumer`, `transport_failure`. No sensitive input is
included in protocol errors. Wrong IDs, missing/duplicate/reordered frames,
malformed frames and missing end fail the body; they never become successful EOF.
Frame sequences are **not journal cursors**.

Controls publish only to `S.control`: `version`, `request_id`, `inbox`, `secret`,
strictly increasing `sequence`, `kind` (`cancel`, `renew`, `ack`) and cumulative
frame `ack`. Initial request capability permits cancellation before headers.
Capabilities are compared in constant time; guesses, unknown IDs and replayed
control sequences are silently ignored. Active `(inbox, request_id)` duplicates
are rejected without invoking another handler. There is no durable deduplication.

## Resource and failure behavior

Defaults (override through `Limits`): 64 ordinary requests, 16 stream requests,
64 client requests, 1 MiB raw request body, 8 KiB path, 16 KiB/64 headers,
64 KiB encoded frame, 32 KiB data chunk, eight unacknowledged frames (at most
256 KiB data), 10-second ACK/setup waits, 30-second lease and 10-second renewal.
No admission queue. Encoded requests and frames must also fit `Conn.MaxPayload()`;
JSON/base64 overhead means a near-1-MiB body can fail under a 1-MiB broker limit.
Broker payloads below 1 KiB are rejected. Large response snapshots are chunked,
not reassembled by the transport. Oversized headers fail boundedly.

Clients ACK consumed data, and headers/heartbeats as observed. Server credit
waits are cancellable and bounded; non-reading consumers lose their observation.
Local frame queues and NATS subscription pending limits are bounded. This channel
opts into runtime event observer budgets of 256 queued events / 4 MiB encoded
live events; overflow, individually oversized events, and replay failures end the
observation. Default HTTP observers retain their existing semantics. The journal
Replay interface itself returns a record slice; this transport does not change
journal storage or claim constant-memory historical replay.

Every request has a renewable lease, including synchronous turns. A vanished
client eventually releases its handler; heartbeats/liveness detect vanished
agents and missing completion. Shutdown stops admission, cancels requests and
waits within its context, without closing caller-owned connections. Disconnects
fail affected bodies; reconnect enables **new calls only**. Clients explicitly
reopen a stream with their last journal cursor. No request is retried
transparently, especially mutations. Core NATS reply loss does not prove an
operation failed to execute. Existing verified `operation_id` idempotency is
still delegated to the HTTP handler.

`Client.Stream` has no terminal error channel: callers see closure and explicitly
reconnect. Raw response-body reads expose transport failure rather than clean EOF.

## Durability boundary

The local journal, not the API connection, provides run durability. Snapshots
retain message content parts, including tool calls/results. Live reasoning and
mid-turn activity are not durable snapshots. Cursor replay recovers durable
events but cannot recreate missed live-only activity. **This is not full
historical trace storage, exactly-once execution, or durable API delivery.**
