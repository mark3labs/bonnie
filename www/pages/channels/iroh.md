---
title: Iroh channel
description: Serve the BONNIE HTTP API over Iroh with approved peers, stable identities, and programmatic pairing.
---

# Iroh channel

BONNIE can serve its `/bonnie/v1` HTTP API on Iroh QUIC streams. It uses
[`github.com/tmc/go-iroh`](https://go-iroh.tmc.dev/) and the `bonnie/http/1`
ALPN. The wire format is HTTP/1.1, including the NDJSON event stream. No TCP
HTTP route is added by this channel. See the [HTTP API](/channels/http) for
run routes, request fields, answers, controls, and stream cursors.

`bonnie.WithIroh` adds this channel beside the default TCP HTTP channel; it
does not disable or authenticate that listener. Bind TCP HTTP to a trusted
interface and configure `bonnie.WithHTTPAuthenticator` when needed. Iroh peer
approval does not protect TCP HTTP.

## Configure the channel

Add `bonnie.WithIroh(irohchannel.Config{...})` in the agent tree's `main.go`.
There are no Iroh environment fallbacks. Your host must load keys and approved
peer IDs from trusted storage, outside prompts, journals, and sandbox settings.

| Config field | Behavior |
| --- | --- |
| `EndpointOptions []iroh.Option` | Library options for keys, bind addresses, relays, and discovery. Use `iroh.WithSecretKey` for a stable server identity. |
| `Peers []key.EndpointID` | Initial approved operator peers. An empty list denies every peer. |
| `TurnPolicy channel.TurnPolicy` | `steer` by default; `queue` is also supported. Unknown policies fail startup. |

The root agent starts and shuts down the channel. A low-level host can use
`irohchannel.New(runner, cfg)`, then `Start(ctx)` and `Shutdown(ctx)` with bounded
contexts. A channel can start only once. Shutdown drains accepted requests until
the context expires, then closes active streams.

## Enable the channel and pair a peer

Pairing is a local, programmatic approval of an authenticated endpoint ID.
Verify the ID with the operator through a separate, trusted path before you
approve it. A ticket supplies an address and server identity, not permission.
There is no open enrollment endpoint, shared pairing code, or CLI pairing
command. This release provides the programmatic pairing option.

```go
serverKey, err := key.ParseSecretKey(serverSecret) // Keep this value secret.
if err != nil {
    return err
}
peerID, err := key.ParseEndpointID(approvedPeerID)
if err != nil {
    return err
}
agent := bonnie.New(bonnie.WithIroh(irohchannel.Config{
    EndpointOptions: []iroh.Option{iroh.WithSecretKey(serverKey)},
    Peers: []key.EndpointID{peerID},
}))
agent.Serve()
```

Set the model provider key, then run `bonnie dev --addr 127.0.0.1:8081 --tui=false` or build the tree with `bonnie build`.

Imports: `github.com/mark3labs/bonnie`,
`github.com/mark3labs/bonnie/channel/iroh` as `irohchannel`,
`github.com/tmc/go-iroh/iroh`, and `github.com/tmc/go-iroh/key`.

An empty peer list denies all requests, including health checks. Each paired
peer has operator access to **all runs** in the journal. This is not tenant
isolation. The verified peer ID supplies the principal; the API does not trust
an identity in the request body. Use a separate agent and journal when peers
must not share access.

## Pair or revoke a peer while running

For live pairing, use `bonnie.WithChannel` to construct and retain the
`*irohchannel.Channel` with `irohchannel.New`. Call `Pair(peerID)` or
`Unpair(peerID)` on that channel. Pair is idempotent. Unpair also blocks later
requests on an existing connection, but does not cancel requests already
accepted. These changes are process-local. Your application must persist the
approved IDs and provide them in `Config.Peers` after restart. The channel
writes no configuration file and does not store keys. Keep both endpoints'
secret keys stable across restarts to retain their identities.

```go
// Additional imports: github.com/mark3labs/bonnie/runtime.
// Construct once during startup. Do not also call WithIroh for this channel.
var transport *irohchannel.Channel
agent := bonnie.New(bonnie.WithChannel(func(r *runtime.Runner) (bonnie.Channel, error) {
    ch, err := irohchannel.New(r, cfg)
    if err != nil {
        return nil, err
    }
    transport = ch
    return ch, nil
}))
// Host code may use transport only after channel construction and startup.
// After out-of-band approval: err := transport.Pair(peerID)
// To revoke: transport.Unpair(peerID)
// To share the public address: ticket, err := transport.Ticket()
agent.Serve()
```

Run approval code on the host, not as an unprotected network route or a tool
that the model can use to approve itself. Coordinate startup before another
goroutine reads `transport`; `Pair` and `Unpair` themselves are safe to call
concurrently.

## Share the server ticket

After the channel starts, `Ticket()` returns the server's public endpoint
address. Share this ticket with clients. Do not log the secret key. Configure
relay and address lookup options through `Config.EndpointOptions` for peers
that cannot connect directly. See the library's
[relay and discovery guide](https://go-iroh.tmc.dev/docs/relays-and-discovery).
A ticket with only a loopback address works only on that machine.

## Connect from Go

```go
ep, err := iroh.Bind(ctx, iroh.WithSecretKey(clientKey))
if err != nil {
    return err
}
// Keep ep open while requests and event streams use it.
hc, err := irohclient.NewHTTPClient(ep, serverTicket)
if err != nil {
    return err
}
c := client.New("http://iroh", client.WithHTTPClient(hc))
// Use c's ordinary Ensure, Send, Respond, Snapshot, and Stream methods.
```

Additional imports: `github.com/mark3labs/bonnie/client` and
`github.com/mark3labs/bonnie/client/iroh` as `irohclient`.

The HTTP client's URL host is ignored; every request goes to the ticket's
endpoint. Its identity is checked by the Iroh handshake. The client has no
overall timeout because streams can stay open during human-input waits. Use
bounded request contexts for short operations. Call `hc.CloseIdleConnections()`
and `ep.Shutdown` with a bounded context when done.

Address resolution uses the `iroh` namespace, separate from TCP HTTP. Exact
run IDs still refer to the same journal. Failed delivery does not prove that a
turn did not run. Use the HTTP API's verified `operation_id` contract for
[idempotent starts](/channels/http#idempotent-starts) where required; do not blindly retry uncertain mutations.

## Troubleshoot a connection

| Symptom | Check and action |
| --- | --- |
| HTTP 401 with `unpaired peer` | Verify the client endpoint ID through a trusted path, then approve it with `Config.Peers` or `Pair`. A server ticket alone does not grant access. |
| Access fails after a restart | Check that both secret keys are stable and that the host loaded the saved peer IDs into `Config.Peers`. Live pairing changes are not saved by the channel. |
| `Ticket()` reports `not running` | Wait for successful channel startup before requesting a ticket. A stopped channel cannot supply a ticket. |
| Connection times out | Check the ticket's addresses and the endpoint's relay and discovery options. A loopback-only ticket cannot connect from another machine. Client dialing is bounded to 30 seconds; a shorter request context can end it sooner. |
| TCP HTTP works but Iroh fails | These are separate listeners. TCP health does not test Iroh reachability or peer approval. Use the Iroh HTTP client to check `/bonnie/v1/health`. |

`curl` and `bonnie chat --addr` connect over TCP HTTP, not Iroh. Use the Go
client above for Iroh requests. There are no built-in Iroh flags or pairing
commands in the developer CLI. See the [CLI reference](/reference/cli) and
[Agent Commands](/guides/agent-commands) for custom commands in a compiled
agent. A custom pairing command must verify the peer ID and save approvals in
trusted host storage; it must not grant access from model input.

## Validation limits

Tests use real loopback Iroh connections and a scripted agent, without relay
services or model credentials. They check peer refusal, approval, revocation,
verified principals, and HTTP run operations. The shared channel conformance
suite checks run controls, suspension, turn policies, and address resolution.
Internet relay connectivity and interoperability with Rust clients are not
proven by these tests.

## Source and tests

[Iroh channel godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/channel/iroh)
and [Iroh client godoc](https://pkg.go.dev/github.com/mark3labs/bonnie/client/iroh).
Verified against `channel/iroh/iroh.go`, `client/iroh/iroh.go`, and `options.go`.
Tests in `channel/iroh/iroh_test.go` and `client/iroh/iroh_test.go` check the
transport and client. `options_iroh_test.go` checks root channel setup.
