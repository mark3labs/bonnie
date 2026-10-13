// Package natsapi adapts the existing BONNIE HTTP client to Core NATS.
// Requests are never retried. A lost response may hide an executed mutation.
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

	wire "github.com/mark3labs/bonnie/internal/natsapi"
	"github.com/nats-io/nats.go"
)

// Limits sets request, frame and timeout budgets. Zero uses defaults.
type Limits = wire.Limits

// Config selects one explicit journal-owning agent. Conn is caller-owned.
type Config struct {
	Conn                  *nats.Conn
	Subject               string
	InboxPrefix           string
	AuthenticationHeaders []string
	Limits                Limits
}

// Transport implements http.RoundTripper with no automatic retries.
type Transport struct {
	cfg    Config
	mu     sync.Mutex
	closed bool
	active map[*body]bool
	slots  chan struct{}
}

// NewTransport validates configuration without opening subscriptions.
func NewTransport(cfg Config) (*Transport, error) {
	if cfg.Conn == nil || !wire.Subject(cfg.Subject) {
		return nil, errors.New("nats-api: connection and literal subject required")
	}
	if cfg.InboxPrefix == "" {
		cfg.InboxPrefix = wire.DefaultInboxPrefix
	}
	if !wire.Subject(cfg.InboxPrefix) || cfg.Conn.MaxPayload() < 1024 {
		return nil, errors.New("nats-api: invalid inbox prefix or broker payload")
	}
	var err error
	cfg.Limits, err = cfg.Limits.Defaults()
	if err != nil {
		return nil, err
	}
	if err = wire.Headers(nil, cfg.AuthenticationHeaders); err != nil {
		return nil, err
	}
	return &Transport{cfg: cfg, active: make(map[*body]bool), slots: make(chan struct{}, cfg.Limits.MaxRequests)}, nil
}

// Close cancels owned requests, not the caller-owned NATS connection.
func (t *Transport) Close() error {
	t.mu.Lock()
	t.closed = true
	bodies := make([]*body, 0, len(t.active))
	for b := range t.active {
		bodies = append(bodies, b)
	}
	t.mu.Unlock()
	for _, b := range bodies {
		b.terminate(errors.New("nats-api: transport closed"), true)
	}
	return nil
}

// RoundTrip returns headers promptly and exposes ordered frames through Body.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		stopClose := context.AfterFunc(req.Context(), func() { _ = req.Body.Close() })
		defer stopClose()
		defer func() { _ = req.Body.Close() }()
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if !t.cfg.Conn.IsConnected() {
		return nil, nats.ErrDisconnected
	}
	raw := []byte(nil)
	if req.Body != nil {
		var err error
		raw, err = io.ReadAll(io.LimitReader(req.Body, int64(t.cfg.Limits.MaxBody)+1))
		if err != nil {
			return nil, err
		}
	}
	r := wire.Request{Version: 1, ID: wire.Token(16), Secret: wire.Token(32), Method: req.Method, Path: req.URL.RequestURI(), Headers: req.Header.Clone(), Body: raw}
	if d, ok := req.Context().Deadline(); ok {
		r.TimeoutMS = max(1, time.Until(d).Milliseconds())
	}
	if err := wire.Validate(r, t.cfg.Limits.MaxBody, t.cfg.AuthenticationHeaders); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(encoded) > int(t.cfg.Conn.MaxPayload()) {
		return nil, errors.New("too_large: encoded request exceeds broker payload")
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errors.New("nats-api: transport closed")
	}
	select {
	case t.slots <- struct{}{}:
	default:
		t.mu.Unlock()
		return nil, errors.New("overloaded")
	}
	ctx, cancel := context.WithCancel(req.Context())
	b := &body{t: t, ctx: ctx, cancel: cancel, request: r, inbox: t.cfg.InboxPrefix + "." + wire.Token(16), queue: make(chan wire.Frame, t.cfg.Limits.Window+1), done: make(chan struct{}), status: t.cfg.Conn.StatusChanged(nats.DISCONNECTED, nats.RECONNECTING, nats.CLOSED), last: time.Now(), lease: t.cfg.Limits.Lease, renew: t.cfg.Limits.Renew}
	t.active[b] = true
	t.mu.Unlock()
	// Serialize setup against Close so an inbox cannot escape cleanup.
	b.setup.Lock()
	select {
	case <-b.done:
		b.setup.Unlock()
		return nil, b.failure()
	default:
	}
	sub, err := t.cfg.Conn.Subscribe(b.inbox, b.receive)
	if err == nil {
		b.sub = sub
		err = sub.SetPendingLimits(t.cfg.Limits.Window+2, (t.cfg.Limits.Window+2)*t.cfg.Limits.MaxFrame)
	}
	if err == nil {
		flushCtx, stop := context.WithTimeout(ctx, t.cfg.Limits.SetupTimeout)
		err = t.cfg.Conn.FlushWithContext(flushCtx)
		stop()
	}
	if err == nil && !t.cfg.Conn.IsConnected() {
		err = nats.ErrDisconnected
	}
	if err == nil {
		err = t.cfg.Conn.PublishRequest(t.cfg.Subject, b.inbox, encoded)
	}
	b.setup.Unlock()
	if err != nil {
		b.terminate(err, true)
		return nil, err
	}
	go b.monitor()
	timer := time.NewTimer(t.cfg.Limits.SetupTimeout)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			err = errors.New("nats-api: header timeout; execution may be uncertain")
		case <-b.done:
			err = b.failure()
		case f := <-b.queue:
			switch f.Kind {
			case "heartbeat":
				b.ack(f.Sequence)
				continue
			case "headers":
				if f.Status < 200 || f.Status > 999 || f.LeaseMS <= 0 || f.RenewMS <= 0 || f.RenewMS*2 >= f.LeaseMS || f.Window < 1 || f.Window > t.cfg.Limits.Window {
					err = errors.New("nats-api: invalid headers")
					break
				}
				b.mu.Lock()
				b.lease = time.Duration(f.LeaseMS) * time.Millisecond
				b.renew = time.Duration(f.RenewMS) * time.Millisecond
				b.mu.Unlock()
				b.ack(f.Sequence)
				return &http.Response{StatusCode: f.Status, Status: fmt.Sprintf("%d %s", f.Status, http.StatusText(f.Status)), Header: f.Headers, Body: b, Request: req, ContentLength: -1, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}, nil
			case "error":
				err = errors.New("nats-api: " + f.Error)
			default:
				err = errors.New("nats-api: expected headers")
			}
		}
		b.terminate(err, true)
		return nil, err
	}
}

type body struct {
	t            *Transport
	ctx          context.Context
	cancel       context.CancelFunc
	request      wire.Request
	inbox        string
	setup        sync.Mutex
	sub          *nats.Subscription
	status       chan nats.Status
	queue        chan wire.Frame
	done         chan struct{}
	once         sync.Once
	readMu       sync.Mutex
	remainder    []byte
	remainderSeq uint64
	mu           sync.Mutex
	next         uint64
	control      uint64
	err          error
	last         time.Time
	lease, renew time.Duration
}

func (b *body) receive(m *nats.Msg) {
	if len(m.Data) > min(b.t.cfg.Limits.MaxFrame, int(b.t.cfg.Conn.MaxPayload())) {
		b.terminate(errors.New("nats-api: oversized frame"), true)
		return
	}
	var f wire.Frame
	if wire.Decode(m.Data, &f) != nil || f.Version != 1 || f.ID != b.request.ID {
		b.terminate(errors.New("nats-api: invalid frame"), true)
		return
	}
	b.mu.Lock()
	valid := f.Sequence == b.next
	if valid {
		b.next++
		b.last = time.Now()
	}
	b.mu.Unlock()
	if !valid {
		b.terminate(errors.New("nats-api: frame sequence gap or duplicate"), true)
		return
	}
	switch f.Kind {
	case "headers", "data", "heartbeat", "end", "error":
	default:
		b.terminate(errors.New("nats-api: unknown frame"), true)
		return
	}
	select {
	case <-b.done:
		return
	default:
	}
	select {
	case b.queue <- f:
	default:
		b.terminate(errors.New("nats-api: slow_consumer"), true)
	}
}
func (b *body) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		return io.ErrClosedPipe
	}
	return b.err
}
func (b *body) controlFrame(kind string, ack uint64) {
	b.mu.Lock()
	b.control++
	sequence := b.control
	// Ordering publication under this lock prevents concurrent ACK and renew
	// from getting reversed and causing a valid ACK to be rejected as replay.
	ctrl := wire.Control{Version: 1, ID: b.request.ID, Inbox: b.inbox, Secret: b.request.Secret, Sequence: sequence, Kind: kind, Ack: ack}
	data, _ := json.Marshal(ctrl)
	if b.t.cfg.Conn.IsConnected() && len(data) <= int(b.t.cfg.Conn.MaxPayload()) {
		_ = b.t.cfg.Conn.Publish(b.t.cfg.Subject+".control", data)
	}
	b.mu.Unlock()
}
func (b *body) ack(seq uint64) { b.controlFrame("ack", seq) }
func (b *body) terminate(err error, remote bool) {
	b.once.Do(func() {
		b.mu.Lock()
		b.err = err
		b.mu.Unlock()
		close(b.done)
		b.t.cfg.Conn.RemoveStatusListener(b.status)
		b.cancel()
		if remote {
			b.controlFrame("cancel", 0)
		}
		b.setup.Lock()
		if b.sub != nil {
			_ = b.sub.Unsubscribe()
		}
		b.setup.Unlock()
		b.t.mu.Lock()
		delete(b.t.active, b)
		b.t.mu.Unlock()
		<-b.t.slots
	})
}
func (b *body) Close() error { b.terminate(io.ErrClosedPipe, true); return nil }
func (b *body) Read(p []byte) (int, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for {
		select {
		case <-b.done:
			return 0, b.failure()
		default:
		}
		if len(b.remainder) > 0 {
			n := copy(p, b.remainder)
			b.remainder = b.remainder[n:]
			if len(b.remainder) == 0 {
				b.ack(b.remainderSeq)
			}
			return n, nil
		}
		select {
		case <-b.done:
			return 0, b.failure()
		case f := <-b.queue:
			switch f.Kind {
			case "data":
				b.remainder = f.Data
				b.remainderSeq = f.Sequence
				if len(f.Data) == 0 {
					b.ack(f.Sequence)
				}
			case "heartbeat":
				b.ack(f.Sequence)
			case "end":
				b.terminate(io.EOF, false)
				return 0, io.EOF
			case "error":
				err := errors.New("nats-api: " + f.Error)
				b.terminate(err, false)
				return 0, err
			default:
				err := errors.New("nats-api: repeated headers")
				b.terminate(err, true)
				return 0, err
			}
		}
	}
}
func (b *body) monitor() {
	ticker := time.NewTicker(min(b.t.cfg.Limits.Renew/2, 100*time.Millisecond))
	defer ticker.Stop()
	renewed := time.Now()
	for {
		select {
		case <-b.done:
			return
		case _, ok := <-b.status:
			if ok {
				b.terminate(nats.ErrDisconnected, true)
			}
			return
		case <-b.ctx.Done():
			b.terminate(b.ctx.Err(), true)
			return
		case <-ticker.C:
			b.mu.Lock()
			last, lease, renew := b.last, b.lease, b.renew
			b.mu.Unlock()
			if !b.t.cfg.Conn.IsConnected() {
				b.terminate(nats.ErrDisconnected, true)
				return
			}
			if time.Since(last) > lease {
				b.terminate(errors.New("nats-api: missing end or agent liveness expired"), true)
				return
			}
			if time.Since(renewed) >= renew {
				b.controlFrame("renew", 0)
				renewed = time.Now()
			}
			b.setup.Lock()
			sub := b.sub
			b.setup.Unlock()
			if sub != nil {
				dropped, err := sub.Dropped()
				if err == nil && dropped > 0 {
					b.terminate(errors.New("nats-api: subscription overflow"), true)
					return
				}
			}
		}
	}
}
