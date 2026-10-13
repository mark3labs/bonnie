---
title: NATS run API
description: Use the existing run client over Core NATS with streaming, authentication and bounded observations.
---

# Run API over Core NATS

`channel/natsapi` and `client/natsapi` expose **every current `/bonnie/v1` HTTP
route**, including submissions, children, cancel-owned and NDJSON streams.
They reuse the HTTP handler, not the asynchronous `channel/nats` task protocol.
Task/answer/result/status subjects and JetStream behavior are unchanged.

## Host and client

The host supplies a connected, caller-owned `*nats.Conn`, one exact subject per
journal-owning agent, and an HTTP authenticator verifying a bearer token or
another allowed credential header:

```go
// Imports: bonnie, channel/http as bonniehttp, channel/natsapi as natsapi.
agent := bonnie.New(
    bonnie.WithNATSAPI(natsapi.Config{
        Conn: agentConn,
        Subject: "bonnie.api.agent.worker-1",
        Authenticator: verifyOperatorCredential,
    }),
    bonnie.WithHTTPListenerDisabled(), // Optional: explicitly no TCP listener.
)
return agent.Run(ctx)
```

`verifyOperatorCredential` has type `bonniehttp.Authenticator`. It must return
an independently verified nonnil principal, or `bonniehttp.ErrUnauthenticated`.
There is no default open mode. Body identity claims cannot override verification.
Health remains public within the broker permission boundary. Operator access
covers all runs: **there is no per-run ownership authorization**. Authenticators
requiring TLS peer certificates or HTTP network identity cannot be reused here.

Without `WithHTTPListenerDisabled`, the default TCP listener is unchanged and
needs its own authentication. Disabling it rejects injected listeners, web UI,
and any channel with TCP routes; it does not silently drop custom webhooks.
Internal schedules, scheduler, clock and presence still run. Schedule HTTP
triggers are unavailable. The NATS API receives host metadata and the outbound
registry before Start. Standalone `New(runner, cfg)` accepts `Info` and `Outbound`.
Shutdown cancels observations before the host closes its journal.

```go
// Imports: client and client/natsapi as natsclient, net/http.
transport, err := natsclient.NewTransport(natsclient.Config{
    Conn: clientConn,
    Subject: "bonnie.api.agent.worker-1",
})
if err != nil { return err }
defer transport.Close()
c := client.New("http://bonnie-over-nats",
    client.WithHTTPClient(&http.Client{Transport: transport}),
    client.WithBearerToken(operatorToken))
// Use existing Ensure, Lookup, Send, RespondWith, Snapshot and Stream methods.
```

Connections stay caller-owned; neither component drains/closes them or replaces
connection handlers. Use request contexts for bounded operations, **not an
overall HTTP timeout** for streams. No broker credentials are loaded by these
packages. There are no CLI flags or discovery mechanisms. The package example
compiles against this checkout; no release-pinned agent example tree uses these
not-yet-published APIs.

## Broker security and routing

Use one ordinary subscriber per exact agent subject; **never queue/load-balance
multiple journals on a subject**. Duplicate owners are misconfiguration, not HA.
Give distinct agents distinct API/control subjects. There is no automatic retry:
lost replies can hide committed mutations. Existing verified `operation_id`
idempotency remains the HTTP handler's contract, not transport deduplication.

For each operator, configure an explicit inbox prefix, e.g.
`_INBOX.bonnieapi.operator1`, on both client and server. The reply must be that
prefix followed by one lowercase hex 128-bit random token, with no extra tokens.
A deployment allowing multiple operators with different prefixes should use
separate bridges/subjects per ACL policy. Broker ACL examples (adapt to your NATS
account/user setup):

- Client publishes only `bonnie.api.agent.worker-1` and
  `bonnie.api.agent.worker-1.control`; subscribes only
  `_INBOX.bonnieapi.operator1.*`.
- Agent subscribes only to its API and control subjects; publishes only to
  `_INBOX.bonnieapi.operator1.*`.
- Deny clients publication to all reply inboxes and access to other operators'
  inboxes. Restrict task subjects independently.

**TLS and broker ACLs are part of the security boundary.** Broad inbox access
exposes private transcript content and cancellation capabilities. Frames are
not independently signed. Do not log bodies, credentials or capability secrets.

## Version 1 exchange

All envelopes are strict JSON with unknown fields rejected. Body/data bytes use
JSON base64. A request is published to the exact API subject with a NATS reply:

```json
{"version":1,"request_id":"<128-bit hex>","method":"GET","path":"/bonnie/v1/info","headers":{"Authorization":["Bearer ..."]},"control_secret":"<256-bit hex>","timeout_ms":10000}
```

Supported methods: GET, HEAD, POST, DELETE. Paths must be origin-form under
`/bonnie/v1/`; absolute URLs, fragments, dot segments, malformed escapes and
controls are refused. Encoded address slashes and cursor queries are preserved.
Unknown routes use HTTP 404/405. Allowed request headers are Authorization,
Content-Type, Accept plus configured `AuthenticationHeaders`; Host, hop-by-hop,
proxy, forwarded/identity and NATS headers are rejected, including CR/LF.
Response headers are Content-Type, Cache-Control, Allow, WWW-Authenticate,
Retry-After. No custom proxy handler is supported.

Response frames have `version`, `request_id`, `sequence` (starting zero), and
`kind`. A normal response has one `headers` frame (`status`, `headers`,
`lease_ms`, `window`), incremental `data` frames, optional `heartbeat` frames,
and one `end`. Heartbeats may also keep a synchronous pre-header request alive;
the initial header wait remains bounded. `error` is terminal and includes only
one bounded code: invalid_request, unsupported_version, overloaded, too_large,
lease_expired, slow_consumer, transport_failure. Protocol rejection may be error
at sequence zero. HTTP non-2xx is still an HTTP response with its JSON body.
End alone means EOF. Missing end, invalid version/ID/order, duplicate frames or
terminal error fail Body.Read, not successful EOF. Sequence is **not** a journal
cursor.

Controls publish only to `<subject>.control`:

```json
{"version":1,"request_id":"<id>","reply_inbox":"<inbox>","secret":"<256-bit capability>","sequence":1,"kind":"ack","acknowledged_sequence":0}
```

Kinds are ack (cumulative), renew, cancel. Capability comparison is constant-time;
unknown/incorrect capabilities and replayed control sequences are ignored.
Secret is sent only in request/control envelopes, never responses. Duplicate
active inbox/request-ID keys do not invoke a second handler. There is no durable
deduplication. Observation cancellation closes the HTTP context, **not** the
explicit durable `/runs/{id}/cancel` API. It may interrupt synchronous work under
the existing HTTP context contract.

## Bounds and recovery

Defaults in Config.Limits: raw body 1 MiB, path 8 KiB, headers 16 KiB/64 values,
JSON frame 64 KiB, data chunk 32 KiB, 8 outstanding frames (at most 256 KiB data),
64 ordinary server slots, 16 separate stream slots, 64 client slots. No admission
queue. Encoded frames/requests are additionally capped by `Conn.MaxPayload()`;
base64 overhead means a near-1-MiB request can exceed a 1-MiB broker limit.
Brokers with less than 4096-byte payloads are refused. Snapshots are chunked, not
reassembled as whole responses by the transport.

ACKs for data advance only when consumed by Body.Read; heartbeats cannot ACK
past unread data. Credit waits expire after 10 seconds. Live runtime observers
are opt-in bounded at 256 events/4 MiB; overflow or oversized events terminate
that observer, never the run. Default HTTP streaming remains unchanged. Pending
subscription/local frame queues are bounded; detected loss is failure.

Every request has a renewable 30-second lease, renewed every 10 seconds while
open; no fixed stream lifetime. Framed heartbeats and 30-second client liveness
expiry detect agent loss. Setup/flush/header waits default to 10 seconds, bounded
by context. Disconnect fails old bodies; reconnect allows only new requests.
Neither mutations nor streams are automatically replayed. Cancellation/Close
release subscriptions and request slots; shutdown leaves connections usable.

`Client.Stream` has no terminal error channel; transport failure appears as event
channel closure. Reconnect explicitly using the last journal cursor. Durable
journal events replay; missing live-only reasoning/tool activity cannot be
recovered. Events sharing one journal anchor remain distinct. Use raw HTTP body
reads if the application needs the precise transport failure.

## Current validation gap

The pinned nats.go v1.47.0 `RemoveStatusListener` removes all listeners on a
connection, not only the supplied listener. This transport therefore uses
periodic connection status/liveness checks instead of registering listeners
which it cannot safely remove. A disconnect followed by reconnect between
checks can escape detection. The approved requirement to fail every old body
on observed connection transitions is not fully implemented. Do not treat this
implementation as ready until isolated status observation and the complete
failure/route acceptance matrix are verified.
