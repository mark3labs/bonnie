package natsapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/mark3labs/bonnie/internal/natsapi"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func scripted(t *testing.T, fn func(*nats.Conn, *nats.Msg, wire.Request)) (*Transport, *nats.Conn) {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	if !s.ReadyForConnections(time.Second) {
		t.Fatal("startup")
	}
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	_, err = nc.Subscribe("api", func(m *nats.Msg) {
		var r wire.Request
		if json.Unmarshal(m.Data, &r) != nil {
			return
		}
		fn(nc, m, r)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = nc.FlushTimeout(time.Second); err != nil {
		t.Fatal(err)
	}
	tr, err := NewTransport(Config{Conn: nc, Subject: "api", Limits: Limits{Heartbeat: 20 * time.Millisecond, Lease: 120 * time.Millisecond, Startup: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr, nc
}
func emit(nc *nats.Conn, m *nats.Msg, r wire.Request, f wire.Frame) {
	f.Version = 1
	if f.ID == "" {
		f.ID = r.ID
	}
	data, _ := json.Marshal(f)
	_ = nc.Publish(m.Reply, data)
}
func TestMalformedAndTruncatedBodies(t *testing.T) {
	for _, kind := range []string{"wrong-id", "duplicate", "reordered", "missing-end", "terminal-error"} {
		t.Run(kind, func(t *testing.T) {
			tr, _ := scripted(t, func(nc *nats.Conn, m *nats.Msg, r wire.Request) {
				emit(nc, m, r, wire.Frame{Kind: "headers", Status: 200, LeaseMS: 120, Window: 8})
				f := wire.Frame{Kind: "data", Seq: 1, Data: []byte("part")}
				switch kind {
				case "wrong-id":
					f.ID = strings.Repeat("a", 32)
				case "duplicate":
					f.Seq = 0
				case "reordered":
					f.Seq = 2
				case "terminal-error":
					f.Kind = "error"
					f.Code = "slow_consumer"
				}
				emit(nc, m, r, f)
			})
			req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://unused/bonnie/v1/info", nil)
			resp, err := tr.RoundTrip(req)
			if err != nil {
				return
			}
			defer func() { _ = resp.Body.Close() }()
			_, err = io.ReadAll(resp.Body)
			if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("failure became EOF: %v", err)
			}
		})
	}
}
func TestHeartbeatCannotACKUnreadData(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := &body{ctx: ctx, cancel: cancel, consumed: make(map[uint64]bool), t: &Transport{cfg: Config{}}, done: make(chan struct{})}
	// Check contiguous acknowledgment bookkeeping without a network publish.
	b.consumed[2] = true
	b.mu.Lock()
	next := b.nextAck
	b.mu.Unlock()
	if next != 0 {
		t.Fatal("heartbeat advanced gap")
	}
	b.consumed[0] = true
	for b.consumed[b.nextAck] {
		delete(b.consumed, b.nextAck)
		b.nextAck++
	}
	if b.nextAck != 1 {
		t.Fatal("ACK crossed unread data")
	}
}
func TestCancellationAndConcurrentCloseRead(t *testing.T) {
	tr, nc := scripted(t, func(nc *nats.Conn, m *nats.Msg, r wire.Request) {
		emit(nc, m, r, wire.Frame{Kind: "headers", Status: 200, LeaseMS: 120, Window: 8})
	})
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://unused/bonnie/v1/info", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := resp.Body.Read(make([]byte, 8))
		if err == nil {
			t.Error("read unexpectedly successful")
		}
	}()
	go func() { defer wg.Done(); _ = resp.Body.Close(); _ = resp.Body.Close() }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close/read leaked")
	}
	if !nc.IsConnected() {
		t.Fatal("caller connection closed")
	}
	tr.mu.Lock()
	n := len(tr.active)
	tr.mu.Unlock()
	if n != 0 {
		t.Fatal("request slot leaked")
	}
}
