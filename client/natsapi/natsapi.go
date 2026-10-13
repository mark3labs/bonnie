// Package natsapi adapts the existing BONNIE HTTP client to Core NATS.
package natsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	wire "github.com/mark3labs/bonnie/internal/natsapi"
)

// Limits bounds transport buffers, concurrency and waits. Zero uses defaults.
type Limits = wire.Limits

// Config uses a caller-owned connection. Close never drains that connection.
type Config struct {
	Conn                  *nats.Conn
	Subject               string
	InboxPrefix           string
	AuthenticationHeaders []string
	Limits                Limits
}

// Transport implements HTTP requests over one exact agent subject.
type Transport struct {
	cfg    Config
	mu     sync.Mutex
	closed bool
	active map[*body]struct{}
}

// NewTransport validates configuration; it does not own the connection.
func NewTransport(cfg Config) (*Transport, error) {
	if cfg.Conn == nil || !wire.Subject(cfg.Subject) {
		return nil, errors.New("nats-api: connection and literal subject required")
	}
	if cfg.InboxPrefix == "" {
		cfg.InboxPrefix = wire.DefaultInbox
	}
	if !wire.Subject(cfg.InboxPrefix) || len(cfg.InboxPrefix) > 512 || len(cfg.Subject) > 512 {
		return nil, errors.New("nats-api: invalid inbox prefix or subject")
	}
	l, err := cfg.Limits.Defaults()
	if err != nil {
		return nil, err
	}
	cfg.Limits = l
	if cfg.Conn.MaxPayload() < 4096 {
		return nil, errors.New("nats-api: broker payload too small")
	}
	return &Transport{cfg: cfg, active: make(map[*body]struct{})}, nil
}

// Close cancels only requests owned by this transport.
func (t *Transport) Close() error {
	t.mu.Lock()
	t.closed = true
	active := make([]*body, 0, len(t.active))
	for b := range t.active {
		active = append(active, b)
	}
	t.mu.Unlock()
	for _, b := range active {
		b.stop(errors.New("nats-api: transport closed"), true)
	}
	return nil
}

// RoundTrip sends once, then returns headers and an incremental response body.
// A failed reply does not prove that a mutation did not execute.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		defer func() { _ = r.Body.Close() }()
	}
	if !t.cfg.Conn.IsConnected() {
		return nil, errors.New("nats-api: disconnected; execution not confirmed")
	}
	id, err := wire.Token(16)
	if err != nil {
		return nil, err
	}
	token, err := wire.Token(16)
	if err != nil {
		return nil, err
	}
	secret, err := wire.Token(32)
	if err != nil {
		return nil, err
	}
	req := wire.Request{Version: 1, ID: id, Method: r.Method, Path: r.URL.RequestURI(), Headers: r.Header.Clone(), Secret: secret}
	if err := req.Validate(t.cfg.Limits, t.cfg.AuthenticationHeaders); err != nil {
		return nil, err
	}
	if deadline, ok := r.Context().Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, r.Context().Err()
		}
		req.TimeoutMS = max(1, remaining.Milliseconds())
	}
	ctx, cancel := context.WithCancel(r.Context())
	b := &body{t: t, ctx: ctx, cancel: cancel, request: req, inbox: t.cfg.InboxPrefix + "." + token, frames: make(chan wire.Frame, t.cfg.Limits.Window+2), headers: make(chan wire.Frame, 1), done: make(chan struct{}), last: time.Now(), lease: t.cfg.Limits.Lease, consumed: make(map[uint64]bool)}
	t.mu.Lock()
	if t.closed || len(t.active) >= t.cfg.Limits.ClientRequests {
		t.mu.Unlock()
		cancel()
		return nil, errors.New("nats-api: closed or overloaded")
	}
	t.active[b] = struct{}{}
	t.mu.Unlock()
	success := false
	defer func() {
		if !success {
			b.stop(errors.New("nats-api: request setup failed; execution may be uncertain"), true)
		}
	}()
	// Reading is bounded by bytes and the request context; Close must unblock
	// readers, as required of outgoing HTTP request bodies.
	if r.Body != nil {
		type result struct {
			data []byte
			err  error
		}
		read := make(chan result, 1)
		go func() {
			data, err := io.ReadAll(io.LimitReader(r.Body, int64(t.cfg.Limits.BodyBytes)+1))
			read <- result{data, err}
		}()
		select {
		case rr := <-read:
			if rr.err != nil {
				return nil, rr.err
			}
			req.Body = rr.data
		case <-ctx.Done():
			_ = r.Body.Close()
			return nil, ctx.Err()
		}
	}
	if len(req.Body) > t.cfg.Limits.BodyBytes {
		return nil, errors.New("too_large")
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(encoded) > int(t.cfg.Conn.MaxPayload()) {
		return nil, errors.New("too_large: encoded request exceeds broker payload")
	}
	sub, err := t.cfg.Conn.Subscribe(b.inbox, b.receive)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		_ = sub.Unsubscribe()
		return nil, ctx.Err()
	}
	b.sub = sub
	b.mu.Unlock()
	if err = sub.SetPendingLimits(t.cfg.Limits.Window+2, min(t.cfg.Limits.FrameBytes, int(t.cfg.Conn.MaxPayload()))*(t.cfg.Limits.Window+2)); err != nil {
		return nil, err
	}
	setup, stop := context.WithTimeout(ctx, t.cfg.Limits.Startup)
	defer stop()
	if err = t.cfg.Conn.FlushWithContext(setup); err != nil {
		return nil, err
	}
	if !t.cfg.Conn.IsConnected() {
		return nil, errors.New("nats-api: disconnected")
	}
	if err = t.cfg.Conn.PublishRequest(t.cfg.Subject, b.inbox, encoded); err != nil {
		return nil, err
	}
	go b.monitor()
	timer := time.NewTimer(t.cfg.Limits.Startup)
	defer timer.Stop()
	select {
	case f := <-b.headers:
		success = true
		return &http.Response{StatusCode: f.Status, Status: fmt.Sprintf("%d %s", f.Status, http.StatusText(f.Status)), Header: f.Headers, Body: b, Request: r, ContentLength: -1, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}, nil
	case <-b.done:
		return nil, b.failure()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("nats-api: header timeout; execution may be uncertain")
	}
}

type body struct {
	t             *Transport
	ctx           context.Context
	cancel        context.CancelFunc
	request       wire.Request
	inbox         string
	sub           *nats.Subscription
	mu            sync.Mutex
	stopped       bool
	err           error
	expected      uint64
	gotHeaders    bool
	ended         bool
	last          time.Time
	lease         time.Duration
	controlSeq    uint64
	frames        chan wire.Frame
	headers       chan wire.Frame
	done          chan struct{}
	controlSendMu sync.Mutex
	consumed      map[uint64]bool
	nextAck       uint64
	readMu        sync.Mutex
	current       []byte
	currentSeq    uint64
}

func (b *body) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	return errors.New("nats-api: closed before headers")
}
func (b *body) acknowledge(seq uint64) {
	b.mu.Lock()
	b.consumed[seq] = true
	for b.consumed[b.nextAck] {
		delete(b.consumed, b.nextAck)
		b.nextAck++
	}
	next := b.nextAck
	b.mu.Unlock()
	if next > 0 {
		b.control("ack", next-1)
	}
}
func (b *body) control(kind string, ack uint64) {
	b.controlSendMu.Lock()
	defer b.controlSendMu.Unlock()
	b.mu.Lock()
	b.controlSeq++
	seq := b.controlSeq
	b.mu.Unlock()
	ctl := wire.Control{Version: 1, ID: b.request.ID, Inbox: b.inbox, Secret: b.request.Secret, Seq: seq, Kind: kind, Ack: ack}
	data, _ := json.Marshal(ctl)
	if b.t.cfg.Conn.IsConnected() && len(data) <= int(b.t.cfg.Conn.MaxPayload()) {
		_ = b.t.cfg.Conn.Publish(b.t.cfg.Subject+".control", data)
	}
}
func (b *body) stop(err error, cancelRemote bool) {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.stopped = true
	b.err = err
	sub := b.sub
	close(b.done)
	b.mu.Unlock()
	if cancelRemote {
		b.control("cancel", 0)
	}
	b.cancel()
	if sub != nil {
		_ = sub.Unsubscribe()
	}
	b.t.mu.Lock()
	delete(b.t.active, b)
	b.t.mu.Unlock()
}
func (b *body) receive(msg *nats.Msg) {
	if msg.Header.Get("Status") == "503" {
		b.stop(errors.New("nats-api: no responders; execution not confirmed"), false)
		return
	}
	var f wire.Frame
	err := wire.Decode(msg.Data, min(b.t.cfg.Limits.FrameBytes, int(b.t.cfg.Conn.MaxPayload())), &f)
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	if err != nil || f.Version != 1 || f.ID != b.request.ID || f.Seq != b.expected || b.ended {
		b.mu.Unlock()
		b.stop(errors.New("nats-api: invalid or out-of-order frame"), true)
		return
	}
	if b.expected-b.nextAck > uint64(b.t.cfg.Limits.Window) || f.Kind == "data" && len(f.Data) > b.t.cfg.Limits.ChunkBytes {
		b.mu.Unlock()
		b.stop(errors.New("nats-api: peer exceeded window or chunk limit"), true)
		return
	}
	b.expected++
	b.last = time.Now()
	if f.Kind == "error" {
		b.mu.Unlock()
		b.stop(fmt.Errorf("nats-api: %s; execution may be uncertain", safeCode(f.Code)), true)
		return
	}
	if !b.gotHeaders {
		if f.Kind == "heartbeat" {
			b.mu.Unlock()
			b.acknowledge(f.Seq)
			return
		}
		if f.Kind != "headers" || f.Status < 200 || f.Status > 599 || wire.Headers(f.Headers, nil, true, b.t.cfg.Limits) != nil || f.LeaseMS <= 0 || f.LeaseMS > int64((24*time.Hour)/time.Millisecond) || f.Window < 1 || f.Window > b.t.cfg.Limits.Window {
			b.mu.Unlock()
			b.stop(errors.New("nats-api: invalid headers frame"), true)
			return
		}
		b.gotHeaders = true
		b.lease = time.Duration(f.LeaseMS) * time.Millisecond
		b.mu.Unlock()
		b.acknowledge(f.Seq)
		b.headers <- f
		return
	}
	if f.Kind == "end" {
		b.ended = true
	}
	switch f.Kind {
	case "heartbeat":
		b.mu.Unlock()
		b.acknowledge(f.Seq)
		return
	case "data", "end":
		select {
		case b.frames <- f:
			b.mu.Unlock()
			if f.Kind == "end" {
				b.stop(nil, false)
			}
			return
		default:
			b.mu.Unlock()
			b.stop(errors.New("nats-api: local frame overflow"), true)
			return
		}
	default:
		b.mu.Unlock()
		b.stop(errors.New("nats-api: unexpected frame"), true)
	}
}
func safeCode(s string) string {
	switch s {
	case "invalid_request", "unsupported_version", "overloaded", "too_large", "lease_expired", "slow_consumer", "transport_failure":
		return s
	}
	return "transport_failure"
}
func (b *body) monitor() {
	// nats.go v1.47.0 RemoveStatusListener removes ALL connection listeners.
	// Do not call it on a caller-owned connection. Poll status until upstream
	// supports isolated listener cleanup; fast disconnect/reconnect is not proven.

	ticker := time.NewTicker(min(b.t.cfg.Limits.Heartbeat, 100*time.Millisecond))
	defer ticker.Stop()
	renew := time.Now().Add(b.t.cfg.Limits.Heartbeat)
	for {
		select {
		case <-b.done:
			return
		case <-b.ctx.Done():
			b.stop(b.ctx.Err(), true)
			return
		case now := <-ticker.C:
			b.mu.Lock()
			last, lease, sub := b.last, b.lease, b.sub
			b.mu.Unlock()
			if !b.t.cfg.Conn.IsConnected() {
				b.stop(errors.New("nats-api: disconnected; execution may be uncertain"), true)
				return
			}
			if sub != nil {
				dropped, err := sub.Dropped()
				if err != nil || dropped > 0 {
					b.stop(errors.New("nats-api: subscription overflow"), true)
					return
				}
			}
			if now.Sub(last) > lease {
				b.stop(errors.New("nats-api: liveness expired; missing end"), true)
				return
			}
			if !now.Before(renew) {
				b.control("renew", 0)
				renew = now.Add(min(b.t.cfg.Limits.Heartbeat, lease/3))
			}
		}
	}
}
func (b *body) Read(p []byte) (int, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for {
		b.mu.Lock()
		err, ended, stopped := b.err, b.ended, b.stopped
		b.mu.Unlock()
		if stopped && (!ended || err != nil) {
			if err == nil {
				err = io.ErrClosedPipe
			}
			return 0, err
		}
		if len(b.current) > 0 {
			n := copy(p, b.current)
			b.current = b.current[n:]
			if len(b.current) == 0 {
				b.acknowledge(b.currentSeq)
			}
			return n, nil
		}
		var f wire.Frame
		select {
		case f = <-b.frames:
		default:
			if ended {
				return 0, io.EOF
			}
			select {
			case f = <-b.frames:
			case <-b.done:
				continue
			}
		}
		if f.Kind == "end" {
			return 0, io.EOF
		}
		b.current = f.Data
		b.currentSeq = f.Seq
		if len(f.Data) == 0 {
			b.acknowledge(f.Seq)
		}
	}
}
func (b *body) Close() error { b.stop(io.ErrClosedPipe, true); return nil }

var _ http.RoundTripper = (*Transport)(nil)
