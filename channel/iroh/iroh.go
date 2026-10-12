// Package iroh serves the BONNIE HTTP API over authenticated Iroh QUIC streams.
// Peers must be paired by the host before they can use any route. Pairing grants
// operator access to all runs, not per-run ownership. Pair and Unpair are local
// operations: no network request can grant itself access.
package iroh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tmc/go-iroh/endpointticket"
	goiroh "github.com/tmc/go-iroh/iroh"
	"github.com/tmc/go-iroh/key"

	"github.com/mark3labs/bonnie/channel"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
)

// ALPN identifies HTTP/1.1 over an Iroh bidirectional stream.
const ALPN = "bonnie/http/1"

// Config configures the endpoint and its initial paired operator peers.
type Config struct {
	// EndpointOptions configure keys, bind addresses, relays, and discovery.
	// Supply WithSecretKey to keep the same identity across restarts.
	EndpointOptions []goiroh.Option
	// Peers are authenticated endpoint IDs, approved out of band by the host.
	// An empty list denies every peer. The caller must persist pairing changes
	// and supply them here on restart; this channel writes no configuration file.
	Peers []key.EndpointID
	// TurnPolicy defaults to steer.
	TurnPolicy channel.TurnPolicy
}

type peerContextKey struct{}

// Channel implements the inbound and lifecycle contracts. Start can run once.
// Its HTTP routes are served only on Iroh, not the host's TCP listener.
type Channel struct {
	api      *bonniehttp.Channel
	cfg      Config
	mu       sync.RWMutex
	peers    map[key.EndpointID]bool
	ep       *goiroh.Endpoint
	server   *http.Server
	started  bool
	stopped  bool
	done     chan struct{}
	serveErr error
}

var (
	_ channel.Channel   = (*Channel)(nil)
	_ channel.Inbound   = (*Channel)(nil)
	_ channel.Lifecycle = (*Channel)(nil)
)

// New constructs a channel without opening network resources.
func New(r *runtime.Runner, cfg Config) (*Channel, error) {
	if cfg.TurnPolicy != "" && cfg.TurnPolicy != channel.PolicySteer && cfg.TurnPolicy != channel.PolicyQueue {
		return nil, channel.ErrUnknownTurnPolicy
	}
	c := &Channel{cfg: cfg, peers: make(map[key.EndpointID]bool)}
	c.cfg.EndpointOptions = append([]goiroh.Option(nil), cfg.EndpointOptions...)
	for _, id := range cfg.Peers {
		if err := c.Pair(id); err != nil {
			return nil, err
		}
	}
	policy := cfg.TurnPolicy
	if policy == "" {
		policy = channel.PolicySteer
	}
	c.api = bonniehttp.New(r, bonniehttp.WithNamespace("iroh"), bonniehttp.WithTurnPolicy(policy),
		bonniehttp.WithInfo(bonniehttp.Info{Channels: []string{"iroh"}}), bonniehttp.WithAuthenticator(c.authenticate))
	return c, nil
}

// Name identifies the transport.
func (c *Channel) Name() string { return "iroh" }

// Routes returns no TCP HTTP routes.
func (c *Channel) Routes() []channel.Route { return nil }

// From resolves an Iroh-local address.
func (c *Channel) From(address string) channel.SessionRef { return c.api.From(address) }

// Attach targets one exact durable run.
func (c *Channel) Attach(id string) channel.SessionRef { return c.api.Attach(id) }

// Pair approves an operator peer after the host has verified its ID out of band.
// Approval is idempotent and process-local. All paired peers can access all runs.
func (c *Channel) Pair(id key.EndpointID) error {
	if id.IsZero() {
		return errors.New("bonnie: channel/iroh: empty peer ID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peers[id] = true
	return nil
}

// Unpair revokes access, including requests on existing connections. A request
// already accepted is not cancelled. Revocation is process-local.
func (c *Channel) Unpair(id key.EndpointID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.peers, id)
}

func (c *Channel) authenticate(r *http.Request) (*channel.Principal, error) {
	id, ok := r.Context().Value(peerContextKey{}).(key.EndpointID)
	c.mu.RLock()
	paired := c.peers[id]
	c.mu.RUnlock()
	if !ok || !paired {
		return nil, bonniehttp.ErrUnauthenticated
	}
	return &channel.Principal{Authenticator: "iroh", Kind: "user", ID: id.String()}, nil
}

// Ticket returns the endpoint's public address after successful Start. A ticket
// provides reachability only; it does not grant access. It contains no secret key.
func (c *Channel) Ticket() (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.ep == nil || c.stopped {
		return "", errors.New("bonnie: channel/iroh: not running")
	}
	return endpointticket.Encode(c.ep.Addr()), nil
}

// Start binds the Iroh endpoint and starts the HTTP server. Startup respects ctx.
func (c *Channel) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.stopped {
		return errors.New("bonnie: channel/iroh: already started or stopped")
	}
	c.started = true
	if err := ctx.Err(); err != nil {
		return err
	}
	opts := append([]goiroh.Option(nil), c.cfg.EndpointOptions...)
	opts = append(opts, goiroh.WithALPNs(ALPN))
	ep, err := goiroh.Bind(ctx, opts...)
	if err != nil {
		return fmt.Errorf("bonnie: channel/iroh: bind: %w", err)
	}
	listener, err := ep.ListenStreams()
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(err, ep.Shutdown(closeCtx))
	}
	c.ep = ep
	handler := c.api.Handler()
	c.server = &http.Server{
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 1 << 20,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			if peer, ok := conn.(interface{ RemoteID() key.EndpointID }); ok {
				return context.WithValue(ctx, peerContextKey{}, peer.RemoteID())
			}
			return ctx
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health must also be private on this transport.
			if _, err := c.authenticate(r); err != nil {
				http.Error(w, "unpaired peer", http.StatusUnauthorized)
				return
			}
			handler.ServeHTTP(w, r)
		}),
	}
	c.done = make(chan struct{})
	go func() {
		err := c.server.Serve(listener)
		c.mu.Lock()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.serveErr = err
		}
		c.mu.Unlock()
		close(c.done)
	}()
	return nil
}

// Shutdown stops input and drains accepted requests until ctx expires. On expiry
// it closes active streams. It is safe before Start and after failed startup.
func (c *Channel) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	c.stopped = true
	server, ep, done := c.server, c.ep, c.done
	c.mu.Unlock()
	if server == nil {
		return nil
	}
	err := server.Shutdown(ctx)
	if err != nil {
		err = errors.Join(err, server.Close())
	}
	err = errors.Join(err, ep.Shutdown(ctx))
	select {
	case <-done:
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return errors.Join(err, c.serveErr)
}
