// Package natsapi serves the complete BONNIE run HTTP API on one Core NATS
// subject. The caller owns the connection and must enforce broker inbox ACLs.
package natsapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/channel"
	api "github.com/mark3labs/bonnie/channel/http"
	wire "github.com/mark3labs/bonnie/internal/natsapi"
	"github.com/mark3labs/bonnie/runtime"
)

// Limits bounds transport buffers, concurrency and waits. Zero uses defaults.
type Limits = wire.Limits

// Config requires a connected, caller-owned connection and a credential verifier.
type Config struct {
	Conn                  *nats.Conn
	Subject               string
	InboxPrefix           string
	Authenticator         api.Authenticator
	TurnPolicy            channel.TurnPolicy
	Info                  api.Info
	Outbound              channel.Outbound
	AuthenticationHeaders []string
	Limits                Limits
}

// Channel serves the run API without mounting TCP routes.
type Channel struct {
	api               *api.Channel
	cfg               Config
	handler           http.Handler
	mu                sync.Mutex
	started, stopped  bool
	subs              []*nats.Subscription
	active            map[string]*responseWriter
	ordinary, streams int
	wg                sync.WaitGroup
}

// New validates configuration without starting subscriptions.
func New(r *runtime.Runner, cfg Config) (*Channel, error) {
	if cfg.Conn == nil || cfg.Authenticator == nil || !wire.Subject(cfg.Subject) {
		return nil, errors.New("nats-api: connection, literal subject and authenticator required")
	}
	if cfg.InboxPrefix == "" {
		cfg.InboxPrefix = wire.DefaultInbox
	}
	if !wire.Subject(cfg.InboxPrefix) || cfg.Subject == cfg.InboxPrefix || len(cfg.Subject) > 512 || len(cfg.InboxPrefix) > 512 {
		return nil, errors.New("nats-api: invalid subject or inbox prefix")
	}
	l, err := cfg.Limits.Defaults()
	if err != nil {
		return nil, err
	}
	cfg.Limits = l
	if cfg.Conn.MaxPayload() < 4096 {
		return nil, errors.New("nats-api: broker payload too small")
	}
	if cfg.TurnPolicy != "" && cfg.TurnPolicy != channel.PolicySteer && cfg.TurnPolicy != channel.PolicyQueue {
		return nil, channel.ErrUnknownTurnPolicy
	}
	c := &Channel{cfg: cfg, active: make(map[string]*responseWriter)}
	auth := func(r *http.Request) (*channel.Principal, error) {
		p, err := cfg.Authenticator(r)
		if err == nil && p == nil {
			return nil, api.ErrUnauthenticated
		}
		return p, err
	}
	c.api = api.New(r, api.WithNamespace("nats-api"), api.WithAuthenticator(auth), api.WithTurnPolicy(defaultPolicy(cfg.TurnPolicy)), api.WithInfo(cfg.Info), api.WithBoundedStreams(256, 4<<20))
	c.handler = c.api.HandlerWithOutbound(cfg.Outbound)
	return c, nil
}
func defaultPolicy(p channel.TurnPolicy) channel.TurnPolicy {
	if p == "" {
		return channel.PolicySteer
	}
	return p
}

// Name identifies the separate run API transport.
func (c *Channel) Name() string { return "nats-api" }

// Routes returns no TCP routes.
func (c *Channel) Routes() []channel.Route { return nil }

// From resolves an address in the nats-api namespace.
func (c *Channel) From(address string) channel.SessionRef { return c.api.From(address) }

// Attach targets one exact run in the shared journal.
func (c *Channel) Attach(id string) channel.SessionRef { return c.api.Attach(id) }

// PrepareHost injects host metadata and outbound dispatch before startup.
func (c *Channel) PrepareHost(out channel.Outbound, info channel.HostInfo) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.stopped {
		return errors.New("nats-api: already started or stopped")
	}
	if info.Version == "" {
		info.Version = c.cfg.Info.Version
	}

	api.WithInfo(api.Info{Agent: info.Agent, Version: info.Version, Channels: info.Channels})(c.api)
	c.handler = c.api.HandlerWithOutbound(out)
	return nil
}

// Start subscribes once and confirms readiness with a bounded flush.
func (c *Channel) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.stopped {
		return errors.New("nats-api: already started or stopped")
	}
	c.started = true
	if !c.cfg.Conn.IsConnected() {
		return errors.New("nats-api: not connected")
	}
	sub, err := c.cfg.Conn.Subscribe(c.cfg.Subject, c.accept)
	if err != nil {
		return err
	}
	c.subs = append(c.subs, sub)
	control, err := c.cfg.Conn.Subscribe(c.cfg.Subject+".control", c.control)
	if err == nil {
		c.subs = append(c.subs, control)
		err = sub.SetPendingLimits(c.cfg.Limits.OrdinaryRequests+c.cfg.Limits.StreamRequests, int(c.cfg.Conn.MaxPayload())*(c.cfg.Limits.OrdinaryRequests+c.cfg.Limits.StreamRequests))
	}
	if err == nil {
		err = control.SetPendingLimits(1024, 1<<20)
	}
	if err == nil {
		startup, cancel := context.WithTimeout(ctx, c.cfg.Limits.Startup)
		defer cancel()
		err = c.cfg.Conn.FlushWithContext(startup)
	}
	if err != nil {
		for _, s := range c.subs {
			_ = s.Unsubscribe()
		}
		c.stopped = true
	}
	return err
}

// Shutdown stops admission and cancels accepted observations within ctx.
// It never drains or closes the caller connection.
func (c *Channel) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	c.stopped = true
	for _, s := range c.subs {
		_ = s.Unsubscribe()
	}
	for _, w := range c.active {
		w.cancel()
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
func (c *Channel) reject(inbox, id, code string) {
	f := wire.Frame{Version: 1, ID: id, Kind: "error", Code: code}
	b, err := json.Marshal(f)
	if err == nil && len(b) <= c.frameMax() && c.cfg.Conn.IsConnected() {
		_ = c.cfg.Conn.Publish(inbox, b)
	}
}
func (c *Channel) frameMax() int  { return min(c.cfg.Limits.FrameBytes, int(c.cfg.Conn.MaxPayload())) }
func key(inbox, id string) string { return inbox + "/" + id }
func (c *Channel) accept(msg *nats.Msg) {
	if !wire.Inbox(msg.Reply, c.cfg.InboxPrefix) {
		return
	}
	var r wire.Request
	if err := wire.Decode(msg.Data, int(c.cfg.Conn.MaxPayload()), &r); err != nil {
		c.reject(msg.Reply, "", "invalid_request")
		return
	}
	if err := r.Validate(c.cfg.Limits, c.cfg.AuthenticationHeaders); err != nil {
		c.reject(msg.Reply, r.ID, err.Error())
		return
	}
	stream := wire.Stream(r)
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		c.reject(msg.Reply, r.ID, "transport_failure")
		return
	}
	if _, ok := c.active[key(msg.Reply, r.ID)]; ok {
		c.mu.Unlock()
		c.reject(msg.Reply, r.ID, "invalid_request")
		return
	}
	if stream && c.streams >= c.cfg.Limits.StreamRequests || !stream && c.ordinary >= c.cfg.Limits.OrdinaryRequests {
		c.mu.Unlock()
		c.reject(msg.Reply, r.ID, "overloaded")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	if r.TimeoutMS > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(r.TimeoutMS)*time.Millisecond)
	}
	w := newWriter(c, ctx, cancel, msg.Reply, r)
	c.active[key(msg.Reply, r.ID)] = w
	if stream {
		c.streams++
	} else {
		c.ordinary++
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer func() {
			cancel()
			c.mu.Lock()
			delete(c.active, key(msg.Reply, r.ID))
			if stream {
				c.streams--
			} else {
				c.ordinary--
			}
			c.mu.Unlock()
		}()
		monitorDone := make(chan struct{})
		go func() { defer close(monitorDone); w.monitor() }()
		defer func() { cancel(); <-monitorDone }()
		defer func() {
			if recover() != nil {
				w.Fail(errors.New("transport_failure"))
			}
			w.finish()
		}()
		req, err := http.NewRequestWithContext(ctx, r.Method, "http://bonnie-over-nats"+r.Path, bytes.NewReader(r.Body))
		if err != nil {
			w.Fail(err)
			return
		}
		req.Header = r.Headers
		req.RequestURI = r.Path
		c.handler.ServeHTTP(w, req)
	}()
}
func (c *Channel) control(msg *nats.Msg) {
	var ctl wire.Control
	if wire.Decode(msg.Data, min(4096, int(c.cfg.Conn.MaxPayload())), &ctl) != nil || ctl.Version != 1 {
		return
	}
	c.mu.Lock()
	w := c.active[key(ctl.Inbox, ctl.ID)]
	c.mu.Unlock()
	if w == nil || subtle.ConstantTimeCompare([]byte(ctl.Secret), []byte(w.request.Secret)) != 1 {
		return
	}
	w.controlMu.Lock()
	defer w.controlMu.Unlock()
	if ctl.Seq <= w.controlSeq {
		return
	}
	w.controlSeq = ctl.Seq
	switch ctl.Kind {
	case "cancel":
		w.cancel()
	case "renew":
		w.leaseUntil = time.Now().Add(c.cfg.Limits.Lease)
	case "ack":
		if ctl.Ack >= w.sent || w.acked && ctl.Ack < w.ack {
			return
		}
		w.ack = ctl.Ack
		w.acked = true
		select {
		case w.credit <- struct{}{}:
		default:
		}
	}
}

type responseWriter struct {
	c                     *Channel
	ctx                   context.Context
	cancel                context.CancelFunc
	inbox                 string
	request               wire.Request
	header                http.Header
	mu                    sync.Mutex
	committed             bool
	status                int
	seq                   uint64
	failed                error
	controlMu             sync.Mutex
	controlSeq, sent, ack uint64
	acked                 bool
	leaseUntil            time.Time
	credit                chan struct{}
}

func newWriter(c *Channel, ctx context.Context, cancel context.CancelFunc, inbox string, r wire.Request) *responseWriter {
	return &responseWriter{c: c, ctx: ctx, cancel: cancel, inbox: inbox, request: r, header: make(http.Header), leaseUntil: time.Now().Add(c.cfg.Limits.Lease), credit: make(chan struct{}, 1)}
}
func (w *responseWriter) Header() http.Header { return w.header }
func (w *responseWriter) Fail(err error)      { w.mu.Lock(); defer w.mu.Unlock(); w.failLocked(err) }
func (w *responseWriter) failLocked(err error) {
	if w.failed == nil {
		w.failed = err
		w.cancel()
	}
}
func (w *responseWriter) publish(f wire.Frame, credit bool) error {
	if w.failed != nil {
		return w.failed
	}
	if credit {
		timer := time.NewTimer(w.c.cfg.Limits.AckTimeout)
		defer timer.Stop()
		for {
			w.controlMu.Lock()
			outstanding := w.sent
			if w.acked {
				outstanding -= w.ack + 1
			}
			w.controlMu.Unlock()
			if outstanding < uint64(w.c.cfg.Limits.Window) {
				break
			}
			select {
			case <-w.credit:
			case <-timer.C:
				w.failLocked(errors.New("slow_consumer"))
				return w.failed
			case <-w.ctx.Done():
				w.failLocked(w.ctx.Err())
				return w.failed
			}
		}
	}
	if err := w.ctx.Err(); err != nil {
		w.failLocked(err)
		return err
	}
	f.Version = 1
	f.ID = w.request.ID
	f.Seq = w.seq
	data, err := json.Marshal(f)
	if err == nil && len(data) > w.c.frameMax() {
		err = errors.New("too_large")
	}
	if err == nil && !w.c.cfg.Conn.IsConnected() {
		err = errors.New("transport_failure")
	}
	if err == nil {
		// Record before publishing: an ACK can arrive immediately.
		w.controlMu.Lock()
		w.sent = w.seq + 1
		w.controlMu.Unlock()
		err = w.c.cfg.Conn.Publish(w.inbox, data)
	}
	if err != nil {
		w.failLocked(err)
		return err
	}
	w.seq++
	return nil
}
func (w *responseWriter) commit(status int) error {
	if w.committed {
		return w.failed
	}
	w.committed = true
	w.status = status
	h := wire.ResponseHeaders(w.header)
	if err := wire.Headers(h, nil, true, w.c.cfg.Limits); err != nil {
		w.failLocked(err)
		return err
	}
	return w.publish(wire.Frame{Kind: "headers", Status: status, Headers: h, LeaseMS: w.c.cfg.Limits.Lease.Milliseconds(), Window: w.c.cfg.Limits.Window}, true)
}
func (w *responseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if status < 200 {
		return
	}
	if status > 599 {
		w.failLocked(errors.New("transport_failure"))
		return
	}
	_ = w.commit(status)
}
func (w *responseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.commit(200); err != nil {
		return 0, err
	}
	if w.request.Method == "HEAD" {
		return len(p), nil
	}
	if w.status == 204 || w.status == 304 {
		return 0, http.ErrBodyNotAllowed
	}
	n := 0
	for len(p) > 0 {
		size := min(len(p), w.c.cfg.Limits.ChunkBytes)
		for size > 0 {
			b, _ := json.Marshal(wire.Frame{Version: 1, ID: w.request.ID, Seq: w.seq, Kind: "data", Data: p[:size]})
			if len(b) <= w.c.frameMax() {
				break
			}
			size /= 2
		}
		if size == 0 {
			w.failLocked(errors.New("too_large"))
			return n, w.failed
		}
		if err := w.publish(wire.Frame{Kind: "data", Data: p[:size]}, true); err != nil {
			return n, err
		}
		n += size
		p = p[size:]
	}
	return n, nil
}
func (w *responseWriter) Flush()            { _ = w.FlushError() }
func (w *responseWriter) FlushError() error { w.mu.Lock(); defer w.mu.Unlock(); return w.commit(200) }
func (w *responseWriter) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed == nil {
		_ = w.commit(200)
		if w.failed == nil {
			_ = w.publish(wire.Frame{Kind: "end"}, false)
		}
	}
	if w.failed != nil && w.c.cfg.Conn.IsConnected() {
		code := "transport_failure"
		for _, s := range []string{"slow_consumer", "lease_expired", "too_large"} {
			if w.failed.Error() == s {
				code = s
			}
		}
		data, _ := json.Marshal(wire.Frame{Version: 1, ID: w.request.ID, Seq: w.seq, Kind: "error", Code: code})
		if len(data) <= w.c.frameMax() {
			_ = w.c.cfg.Conn.Publish(w.inbox, data)
		}
	}
}
func (w *responseWriter) monitor() {
	// nats.go v1.47.0 RemoveStatusListener removes ALL connection listeners.
	// Do not call it on a caller-owned connection. Poll status until upstream
	// supports isolated listener cleanup; fast disconnect/reconnect is not proven.

	ticker := time.NewTicker(min(w.c.cfg.Limits.Heartbeat, 100*time.Millisecond))
	defer ticker.Stop()
	next := time.Now().Add(w.c.cfg.Limits.Heartbeat)
	for {
		select {
		case <-w.ctx.Done():
			return
		case now := <-ticker.C:
			w.controlMu.Lock()
			expired := now.After(w.leaseUntil)
			w.controlMu.Unlock()
			if expired {
				w.cancel()
				w.Fail(errors.New("lease_expired"))
				return
			}
			if !w.c.cfg.Conn.IsConnected() {
				w.cancel()
				w.Fail(errors.New("transport_failure"))
				return
			}
			if !now.Before(next) {
				// TryLock avoids blocking lease and connection checks behind ACK waits.
				if w.mu.TryLock() {
					_ = w.publish(wire.Frame{Kind: "heartbeat"}, true)
					w.mu.Unlock()
				}
				next = now.Add(w.c.cfg.Limits.Heartbeat)
			}
		}
	}
}

var _ http.Flusher = (*responseWriter)(nil)
var _ channel.HostPreparer = (*Channel)(nil)
