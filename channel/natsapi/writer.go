package natsapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	wire "github.com/mark3labs/bonnie/internal/natsapi"
	"github.com/nats-io/nats.go"
)

type writer struct {
	c         *Channel
	r         *request
	header    http.Header
	mu        sync.Mutex
	status    int
	next      uint64
	err       error
	finished  bool
	lastFrame time.Time
}

func newWriter(c *Channel, r *request) *writer {
	return &writer{c: c, r: r, header: make(http.Header), lastFrame: time.Now()}
}
func (w *writer) Header() http.Header { return w.header }
func (w *writer) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if status < 100 || status > 999 {
		panic("invalid HTTP status")
	}
	if status < 200 {
		return
	} // Interim responses are not part of version one.
	if w.status != 0 || w.err != nil || w.finished {
		return
	}
	w.commit(status)
}
func (w *writer) commit(status int) {
	w.status = status
	w.err = w.send(wire.Frame{Kind: "headers", Status: status, Headers: wire.ResponseHeaders(w.header), LeaseMS: w.c.cfg.Limits.Lease.Milliseconds(), RenewMS: w.c.cfg.Limits.Renew.Milliseconds(), Window: w.c.cfg.Limits.Window})
	if w.err != nil {
		w.r.cancel()
	}
}
func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if w.finished {
		return 0, errors.New("nats-api: response finished")
	}
	if w.status == 0 {
		if w.header.Get("Content-Type") == "" {
			w.header.Set("Content-Type", http.DetectContentType(p))
		}
		w.commit(200)
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.r.req.Method == "HEAD" {
		return len(p), nil
	}
	if w.status == 204 || w.status == 304 {
		return 0, http.ErrBodyNotAllowed
	}
	n := 0
	for len(p) > 0 {
		size := min(len(p), w.c.cfg.Limits.ChunkSize)
		for size > 0 {
			f := wire.Frame{Version: 1, ID: w.r.req.ID, Sequence: w.next, Kind: "data", Data: p[:size]}
			encoded, _ := json.Marshal(f)
			if len(encoded) <= w.c.frameLimit() {
				break
			}
			size /= 2
		}
		if size == 0 {
			w.err = errors.New("too_large")
			w.r.cancel()
			return n, w.err
		}
		if err := w.send(wire.Frame{Kind: "data", Data: p[:size]}); err != nil {
			w.err = err
			w.r.cancel()
			return n, err
		}
		n += size
		p = p[size:]
	}
	return n, nil
}
func (w *writer) Flush() { _ = w.FlushError() }
func (w *writer) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 && w.err == nil && !w.finished {
		w.commit(200)
	}
	return w.err
}

// FailStream lets the shared HTTP stream surface observer overflow as failure.
func (w *writer) FailStream(err error) {
	if err != nil {
		w.fail("slow_consumer")
	}
}
func (w *writer) fail(code string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failLocked(code)
}
func (w *writer) failLocked(code string) {
	if w.finished {
		return
	}
	f := wire.Frame{Version: 1, ID: w.r.req.ID, Sequence: w.next, Kind: "error", Error: code}
	b, _ := json.Marshal(f)
	if len(b) <= w.c.frameLimit() && w.c.cfg.Conn.IsConnected() {
		_ = w.c.cfg.Conn.Publish(w.r.inbox, b)
	}
	w.err = errors.New(code)
	w.finished = true
	w.r.cancel()
}
func (w *writer) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return
	}
	if w.err != nil {
		code := "transport_failure"
		if w.err.Error() == "too_large" || w.err.Error() == "slow_consumer" {
			code = w.err.Error()
		}
		w.failLocked(code)
		return
	}
	if w.status == 0 {
		w.commit(200)
	}
	if w.err == nil {
		w.err = w.send(wire.Frame{Kind: "end"})
	}
	if w.err != nil {
		w.failLocked("transport_failure")
		return
	}
	w.finished = true
}

// send runs under the writer mutex; controls only acquire request.mu.
func (w *writer) send(f wire.Frame) error {
	f.Version = 1
	f.ID = w.r.req.ID
	f.Sequence = w.next
	b, err := json.Marshal(f)
	if err != nil || len(b) > w.c.frameLimit() {
		return errors.New("too_large")
	}
	timer := time.NewTimer(w.c.cfg.Limits.AckTimeout)
	defer timer.Stop()
	for {
		w.r.mu.Lock()
		credit := w.next
		if w.r.hasAck {
			credit = w.next - w.r.ack - 1
		}
		w.r.mu.Unlock()
		if credit < uint64(w.c.cfg.Limits.Window) {
			break
		}
		select {
		case <-w.r.wake:
		case <-w.r.ctx.Done():
			return w.r.ctx.Err()
		case <-timer.C:
			return errors.New("slow_consumer")
		}
	}
	if err = w.r.ctx.Err(); err != nil {
		return err
	}
	if !w.c.cfg.Conn.IsConnected() {
		return nats.ErrDisconnected
	}
	// Record before publish so an immediate ACK cannot race registration.
	w.r.mu.Lock()
	w.r.sent = w.next
	w.r.hasSent = true
	w.r.mu.Unlock()
	if err = w.c.cfg.Conn.Publish(w.r.inbox, b); err != nil {
		return err
	}
	w.next++
	w.lastFrame = time.Now()
	return nil
}
func (w *writer) monitor() {
	interval := min(w.c.cfg.Limits.Renew, time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case _, ok := <-w.r.status:
			if ok {
				w.r.cancel()
				w.fail("transport_failure")
			}
			return
		case <-w.r.ctx.Done():
			return
		case <-ticker.C:
			w.r.mu.Lock()
			expired := time.Since(w.r.renewed) > w.c.cfg.Limits.Lease
			w.r.mu.Unlock()
			// Cancel first to unblock a writer waiting for ACK credit.
			if expired || !w.c.cfg.Conn.IsConnected() {
				w.r.cancel()
				code := "transport_failure"
				if expired {
					code = "lease_expired"
				}
				w.fail(code)
				return
			}
			if w.mu.TryLock() {
				if !w.finished && w.err == nil && time.Since(w.lastFrame) >= w.c.cfg.Limits.Renew {
					// Heartbeats before headers keep synchronous turns alive but do not
					// masquerade as an HTTP response.
					if err := w.send(wire.Frame{Kind: "heartbeat"}); err != nil {
						w.err = err
						w.r.cancel()
					}
				}
				w.mu.Unlock()
			}
		}
	}
}
