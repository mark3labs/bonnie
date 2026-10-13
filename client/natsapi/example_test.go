package natsapi_test

import (
	"net/http"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/client/natsapi"
	"github.com/nats-io/nats.go"
)

// The application obtains a connection and operator credential from trusted
// configuration, never from agent input. No overall HTTP timeout is set.
func ExampleNewTransport() {
	connect := func(conn *nats.Conn, token string) (*client.Client, *natsapi.Transport, error) {
		transport, err := natsapi.NewTransport(natsapi.Config{Conn: conn, Subject: "bonnie.api.agent.worker-1"})
		if err != nil {
			return nil, nil, err
		}
		c := client.New("http://bonnie-over-nats", client.WithHTTPClient(&http.Client{Transport: transport}), client.WithBearerToken(token))
		return c, transport, nil
	}
	_ = connect
}
