package natsapi_test

import (
	"net/http"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/client/natsapi"
	"github.com/nats-io/nats.go"
)

// Supply a caller-owned authenticated connection, an explicit agent target,
// and an independently verified API bearer credential. The URL is logical.
func ExampleNewTransport() {
	var nc *nats.Conn // connect with deployment-specific TLS/credentials first
	if nc == nil {
		return
	}
	transport, err := natsapi.NewTransport(natsapi.Config{Conn: nc, Subject: "bonnie.api.agent.worker-1"})
	if err != nil {
		return
	}
	defer func() { _ = transport.Close() }()
	c := client.New("http://bonnie-over-nats", client.WithHTTPClient(&http.Client{Transport: transport}), client.WithBearerToken("securely-supplied-operator-token"))
	_ = c
}
