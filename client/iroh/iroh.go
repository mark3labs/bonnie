// Package iroh connects the BONNIE HTTP client to an Iroh channel.
package iroh

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/tmc/go-iroh/endpointticket"
	goiroh "github.com/tmc/go-iroh/iroh"

	irohchannel "github.com/mark3labs/bonnie/channel/iroh"
)

// NewHTTPClient returns an HTTP/1.1 client which sends every request to ticket.
// The URL host is ignored. Use http://iroh as the BONNIE client base URL.
// The caller owns ep, must keep its secret key stable for pairing, and must
// close idle connections and shut down ep when done. Request contexts bound
// operations; dialing is bounded to 30 seconds. No overall timeout is set because event streams can
// stay open indefinitely. The ticket pins the server's authenticated identity.
func NewHTTPClient(ep *goiroh.Endpoint, ticket string) (*http.Client, error) {
	if ep == nil {
		return nil, errors.New("bonnie: client/iroh: nil endpoint")
	}
	addr, err := endpointticket.Decode(ticket)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:        nil,
		MaxIdleConns: 8, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 32,
		IdleConnTimeout: time.Minute,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			conn, err := ep.Connect(dialCtx, addr, irohchannel.ALPN)
			if err != nil {
				return nil, err
			}
			stream, err := conn.OpenStreamConn(dialCtx)
			if err != nil {
				return nil, errors.Join(err, conn.Close())
			}
			return &ownedStream{Conn: stream, peer: conn}, nil
		},
	}
	return &http.Client{Transport: transport}, nil
}

type ownedStream struct {
	net.Conn
	peer *goiroh.Conn
}

func (s *ownedStream) Close() error {
	return errors.Join(s.Conn.Close(), s.peer.Close())
}
