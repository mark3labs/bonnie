# Iroh channel

BONNIE can serve its `/bonnie/v1` HTTP API on Iroh QUIC streams. It uses
[`github.com/tmc/go-iroh`](https://go-iroh.tmc.dev/) and the `bonnie/http/1`
ALPN. The wire format is HTTP/1.1, including the NDJSON event stream. No TCP
HTTP route is added by this channel.

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

Imports: `github.com/mark3labs/bonnie`,
`github.com/mark3labs/bonnie/channel/iroh` as `irohchannel`,
`github.com/tmc/go-iroh/iroh`, and `github.com/tmc/go-iroh/key`.

An empty peer list denies all requests, including health checks. Each paired
peer has operator access to **all runs** in the journal. This is not tenant
isolation. The verified peer ID supplies the principal; the API does not trust
an identity in the request body. Use a separate agent and journal when peers
must not share access.

For live pairing, use `bonnie.WithChannel` to construct and retain the
`*irohchannel.Channel` with `irohchannel.New`. Call `Pair(peerID)` or
`Unpair(peerID)` on that channel. Pair is idempotent. Unpair also blocks later
requests on an existing connection, but does not cancel requests already
accepted. These changes are process-local. Your application must persist the
approved IDs and provide them in `Config.Peers` after restart. The channel
writes no configuration file and does not store keys. Keep both endpoints'
secret keys stable across restarts to retain their identities.

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
idempotent starts where required; do not blindly retry uncertain mutations.

## Validation limits

Tests use real loopback Iroh connections and a scripted agent, without relay
services or model credentials. They check peer refusal, approval, revocation,
verified principals, and HTTP run operations. The shared channel conformance
suite checks run controls, suspension, turn policies, and address resolution.
Internet relay connectivity and interoperability with Rust clients are not
proven by these tests.
