// Package natsapi serves the existing authenticated run API over Core NATS.
// Each subject has exactly one owner, never a worker queue. Connections are
// caller-owned; Shutdown does not drain or close them.
package natsapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mark3labs/bonnie/channel"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	wire "github.com/mark3labs/bonnie/internal/natsapi"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
)

// Limits sets admission, frame, flow-control and lease budgets. Zero uses defaults.
type Limits = wire.Limits

// Config requires explicit API authentication independently of broker login.
type Config struct {
	Conn                  *nats.Conn
	Subject               string
	InboxPrefix           string
	Authenticator         bonniehttp.Authenticator
	AuthenticationHeaders []string
	TurnPolicy            channel.TurnPolicy
	Info                  bonniehttp.Info
	Outbound              channel.Outbound
	Limits                Limits
}

// Channel exposes the HTTP run API without a TCP listener.
type Channel struct {
	cfg               Config
	api               *bonniehttp.Channel
	handler           http.Handler
	version           string
	mu                sync.Mutex
	started, stopped  bool
	subs              []*nats.Subscription
	active            map[string]*request
	ordinary, streams chan struct{}
	wg                sync.WaitGroup
}

type request struct {
	ctx     context.Context
	cancel  context.CancelFunc
	inbox   string
	req     wire.Request
	mu      sync.Mutex
	control uint64
	ack     uint64
	hasAck  bool
	sent    uint64
	hasSent bool
	renewed time.Time
	wake    chan struct{}
	status  chan nats.Status
}

// New validates configuration without opening subscriptions.
func New(r *runtime.Runner, cfg Config) (*Channel, error) {
	if cfg.Conn == nil || cfg.Authenticator == nil || !wire.Subject(cfg.Subject) {
		return nil, errors.New("nats-api: connection, literal subject and authenticator required")
	}
	if cfg.InboxPrefix == "" {
		cfg.InboxPrefix = wire.DefaultInboxPrefix
	}
	if !wire.Subject(cfg.InboxPrefix) || cfg.Conn.MaxPayload() < 1024 {
		return nil, errors.New("nats-api: invalid inbox prefix or broker payload limit")
	}
	var err error
	cfg.Limits, err = cfg.Limits.Defaults()
	if err != nil {
		return nil, err
	}
	if err = wire.Headers(nil, cfg.AuthenticationHeaders); err != nil {
		return nil, err
	}
	if cfg.TurnPolicy != "" && cfg.TurnPolicy != channel.PolicySteer && cfg.TurnPolicy != channel.PolicyQueue {
		return nil, channel.ErrUnknownTurnPolicy
	}
	version := cfg.Info.Version
	if version == "" {
		version = "(devel)"
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, d := range bi.Deps {
				if d.Path == "github.com/mark3labs/bonnie" {
					version = d.Version
					break
				}
			}
		}
	}
	c := &Channel{version: version, cfg: cfg, active: make(map[string]*request), ordinary: make(chan struct{}, cfg.Limits.MaxRequests), streams: make(chan struct{}, cfg.Limits.MaxStreams)}
	policy := cfg.TurnPolicy
	if policy == "" {
		policy = channel.PolicySteer
	}
	c.api = bonniehttp.New(r, bonniehttp.WithTurnPolicy(policy), bonniehttp.WithNamespace("nats-api"), bonniehttp.WithInfo(cfg.Info), bonniehttp.WithBoundedStreams(256, 4<<20), bonniehttp.WithAuthenticator(func(req *http.Request) (*channel.Principal, error) {
		p, err := cfg.Authenticator(req)
		if err == nil && p == nil {
			err = bonniehttp.ErrUnauthenticated
		}
		return p, err
	}))
	c.handler = c.api.HandlerWithOutbound(cfg.Outbound)
	return c, nil
}

// Name identifies the separate API address namespace.
func (c *Channel) Name() string { return "nats-api" }

// Routes is empty: this channel has no TCP routes.
func (c *Channel) Routes() []channel.Route { return nil }

// From resolves an API-local address.
func (c *Channel) From(a string) channel.SessionRef { return c.api.From(a) }

// Attach targets an existing durable run.
func (c *Channel) Attach(id string) channel.SessionRef { return c.api.Attach(id) }

// PrepareHost binds host metadata and outbound dispatch before Start.
func (c *Channel) PrepareHost(out channel.Outbound, info channel.HostInfo) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return errors.New("nats-api: already started")
	}
	if info.Version == "" {
		info.Version = c.version
	}
	bonniehttp.WithInfo(bonniehttp.Info{Agent: info.Agent, Version: info.Version, Channels: info.Channels})(c.api)
	c.handler = c.api.HandlerWithOutbound(out)
	return nil
}

// Start installs exact, non-queue API/control subscriptions once.
func (c *Channel) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.stopped {
		return errors.New("nats-api: already started or stopped")
	}
	c.started = true
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.cfg.Conn.IsConnected() {
		return nats.ErrDisconnected
	}
	sub, err := c.cfg.Conn.Subscribe(c.cfg.Subject, func(m *nats.Msg) { c.accept(ctx, m) })
	if err != nil {
		return err
	}
	c.subs = append(c.subs, sub)
	control, err := c.cfg.Conn.Subscribe(c.cfg.Subject+".control", c.control)
	if err == nil {
		c.subs = append(c.subs, control)
		flushCtx, cancel := context.WithTimeout(ctx, c.cfg.Limits.SetupTimeout)
		err = c.cfg.Conn.FlushWithContext(flushCtx)
		cancel()
	}
	if err != nil {
		for _, s := range c.subs {
			_ = s.Unsubscribe()
		}
		c.subs = nil
		return err
	}
	return nil
}
func (c *Channel) reject(inbox, id, code string) {
	if !wire.Inbox(inbox, c.cfg.InboxPrefix) {
		return
	}
	b, _ := json.Marshal(wire.Frame{Version: 1, ID: id, Kind: "error", Error: code})
	if len(b) <= c.frameLimit() && c.cfg.Conn.IsConnected() {
		_ = c.cfg.Conn.Publish(inbox, b)
	}
}
func (c *Channel) frameLimit() int { return min(c.cfg.Limits.MaxFrame, int(c.cfg.Conn.MaxPayload())) }
func (c *Channel) accept(parent context.Context, m *nats.Msg) {
	if !wire.Inbox(m.Reply, c.cfg.InboxPrefix) {
		return
	}
	var req wire.Request
	if len(m.Data) > int(c.cfg.Conn.MaxPayload()) || wire.Decode(m.Data, &req) != nil {
		c.reject(m.Reply, "", "invalid_request")
		return
	}
	if err := wire.Validate(req, c.cfg.Limits.MaxBody, c.cfg.AuthenticationHeaders); err != nil {
		c.reject(m.Reply, req.ID, err.Error())
		return
	}
	u, _ := wire.Path(req.Path)
	pool := c.ordinary
	if wire.Stream(u, req.Method) {
		pool = c.streams
	}
	c.mu.Lock()
	key := m.Reply + "/" + req.ID
	if c.stopped || c.active[key] != nil {
		c.mu.Unlock()
		c.reject(m.Reply, req.ID, "overloaded")
		return
	}
	select {
	case pool <- struct{}{}:
	default:
		c.mu.Unlock()
		c.reject(m.Reply, req.ID, "overloaded")
		return
	}
	ctx, cancel := context.WithCancel(parent)
	if req.TimeoutMS > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(parent, time.Duration(req.TimeoutMS)*time.Millisecond)
	}
	r := &request{ctx: ctx, cancel: cancel, inbox: m.Reply, req: req, status: c.cfg.Conn.StatusChanged(nats.DISCONNECTED, nats.RECONNECTING, nats.CLOSED), renewed: time.Now(), wake: make(chan struct{}, 1)}
	c.active[key] = r
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer c.cfg.Conn.RemoveStatusListener(r.status)
		defer func() { cancel(); <-pool; c.mu.Lock(); delete(c.active, key); c.mu.Unlock() }()
		w := newWriter(c, r)
		monitorDone := make(chan struct{})
		go func() { defer close(monitorDone); w.monitor() }()
		defer func() { cancel(); <-monitorDone }()
		defer func() {
			if recover() != nil {
				w.fail("transport_failure")
			}
		}()
		httpReq, err := http.NewRequestWithContext(ctx, req.Method, "http://bonnie-over-nats"+req.Path, bytes.NewReader(req.Body))
		if err != nil {
			w.fail("invalid_request")
			return
		}
		httpReq.Header = req.Headers.Clone()
		c.handler.ServeHTTP(w, httpReq)
		w.finish()
	}()
}
func (c *Channel) control(m *nats.Msg) {
	if len(m.Data) > 4096 {
		return
	}
	var ctrl wire.Control
	if wire.Decode(m.Data, &ctrl) != nil || ctrl.Version != 1 || !wire.Inbox(ctrl.Inbox, c.cfg.InboxPrefix) {
		return
	}
	c.mu.Lock()
	r := c.active[ctrl.Inbox+"/"+ctrl.ID]
	c.mu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(ctrl.Secret), []byte(r.req.Secret)) != 1 || ctrl.Sequence <= r.control {
		return
	}
	switch ctrl.Kind {
	case "cancel":
		r.control = ctrl.Sequence
		r.cancel()
	case "renew":
		r.control = ctrl.Sequence
		r.renewed = time.Now()
	case "ack":
		if !r.hasSent || ctrl.Ack > r.sent || r.hasAck && ctrl.Ack < r.ack {
			return
		}
		r.control = ctrl.Sequence
		r.ack = ctrl.Ack
		r.hasAck = true
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

// Shutdown stops admission and cancels HTTP requests within ctx.
func (c *Channel) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	c.stopped = true
	for _, s := range c.subs {
		_ = s.Unsubscribe()
	}
	c.subs = nil
	for _, r := range c.active {
		r.cancel()
	}
	c.mu.Unlock()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
