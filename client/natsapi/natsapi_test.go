package natsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	wire "github.com/mark3labs/bonnie/internal/natsapi"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func connect(t *testing.T) *nats.Conn {
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
	return nc
}
func fakeFrames(t *testing.T, frames func(string) []wire.Frame) (*Transport, *nats.Conn) {
	t.Helper()
	nc := connect(t)
	_, err := nc.Subscribe("agent", func(m *nats.Msg) {
		var r wire.Request
		_ = json.Unmarshal(m.Data, &r)
		for _, f := range frames(r.ID) {
			b, _ := json.Marshal(f)
			_ = nc.Publish(m.Reply, b)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	tr, err := NewTransport(Config{Conn: nc, Subject: "agent", Limits: Limits{Lease: 100 * time.Millisecond, Renew: 20 * time.Millisecond, SetupTimeout: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr, nc
}
func headers(id string) wire.Frame {
	return wire.Frame{Version: 1, ID: id, Kind: "headers", Status: 200, LeaseMS: 100, RenewMS: 20, Window: 8}
}
func TestDamagedFramesAreNotEOF(t *testing.T) {
	for _, kind := range []string{"gap", "duplicate", "wrong-id", "version", "missing-end", "error", "unknown", "headers"} {
		t.Run(kind, func(t *testing.T) {
			tr, _ := fakeFrames(t, func(id string) []wire.Frame {
				h := headers(id)
				f := wire.Frame{Version: 1, ID: id, Sequence: 1, Kind: "data", Data: []byte("body")}
				switch kind {
				case "gap":
					f.Sequence = 2
				case "duplicate":
					f.Sequence = 0
				case "wrong-id":
					f.ID = "other"
				case "version":
					f.Version = 2
				case "error":
					f.Kind = "error"
					f.Error = "transport_failure"
				case "unknown":
					f.Kind = "bogus"
				case "headers":
					f = h
					f.Sequence = 1
				}
				return []wire.Frame{h, f}
			})
			req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://logical/bonnie/v1/info", nil)
			resp, err := tr.RoundTrip(req)
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
			if err == nil || err == io.EOF {
				t.Fatal("damaged stream became EOF")
			}
		})
	}
}
func TestCloseConcurrentWithReadAndContextCancellation(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		tr, _ := fakeFrames(t, func(id string) []wire.Frame { return []wire.Frame{headers(id)} })
		ctx, cancel := context.WithCancel(t.Context())
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://logical/bonnie/v1/info", nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := resp.Body.Read(make([]byte, 1)); done <- err }()
		if cancelContext {
			cancel()
		} else {
			_ = resp.Body.Close()
		}
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("read did not fail")
			}
		case <-time.After(time.Second):
			t.Fatal("read stuck")
		}
		_ = resp.Body.Close()
		tr.mu.Lock()
		n := len(tr.active)
		tr.mu.Unlock()
		if n != 0 {
			t.Fatal("active request leaked")
		}
	}
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }
func TestSetupErrorClosesBodyAndNoRetry(t *testing.T) {
	nc := connect(t)
	tr, err := NewTransport(Config{Conn: nc, Subject: "absent", Limits: Limits{SetupTimeout: 30 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()
	body := &trackedBody{Reader: strings.NewReader("mutation")}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", "http://logical/bonnie/v1/runs", body)
	if _, err = tr.RoundTrip(req); err == nil {
		t.Fatal("unavailable agent succeeded")
	}
	if !body.closed {
		t.Fatal("body not closed")
	}
	if len(tr.active) != 0 || len(tr.slots) != 0 {
		t.Fatal("setup leaked resources")
	}
	if !nc.IsConnected() {
		t.Fatal("connection closed")
	}
}

func TestDisconnectFailsBodyWithoutReplacingHandlers(t *testing.T) {
	tr, nc := fakeFrames(t, func(id string) []wire.Frame { return []wire.Frame{headers(id)} })
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://logical/bonnie/v1/info", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	nc.Close()
	done := make(chan error, 1)
	go func() { _, err := resp.Body.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		if err == nil || err == io.EOF {
			t.Fatal("disconnect became EOF")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect not observed")
	}
	_ = resp.Body.Close()
	if _, err = tr.RoundTrip(req); err == nil {
		t.Fatal("closed broker accepted new request")
	}
}

// A synchronous turn writes headers only at its end. Heartbeats before the
// headers prove liveness, so SetupTimeout must not abort the request.
func TestHeartbeatsBeforeHeadersOutlastSetupTimeout(t *testing.T) {
	t.Parallel()
	nc := connect(t)
	_, err := nc.Subscribe("agent", func(m *nats.Msg) {
		var r wire.Request
		_ = json.Unmarshal(m.Data, &r)
		go func() {
			var seq uint64
			publish := func(f wire.Frame) {
				f.Version, f.ID, f.Sequence = 1, r.ID, seq
				seq++
				b, _ := json.Marshal(f)
				_ = nc.Publish(m.Reply, b)
			}
			for range 10 {
				publish(wire.Frame{Kind: "heartbeat"})
				time.Sleep(30 * time.Millisecond)
			}
			publish(headers(r.ID))
			publish(wire.Frame{Kind: "data", Data: []byte("ok")})
			publish(wire.Frame{Kind: "end"})
		}()
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	tr, err := NewTransport(Config{Conn: nc, Subject: "agent", Limits: Limits{Lease: 100 * time.Millisecond, Renew: 20 * time.Millisecond, SetupTimeout: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", "http://logical/bonnie/v1/runs", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "ok" {
		t.Fatalf("body %q, err %v", body, err)
	}
}

// The renew cadence follows the server's negotiated interval, not the
// client's default, so a short server lease is renewed in time.
func TestRenewFollowsNegotiatedInterval(t *testing.T) {
	t.Parallel()
	nc := connect(t)
	renews := make(chan struct{}, 1024)
	_, err := nc.Subscribe("agent.control", func(m *nats.Msg) {
		var c wire.Control
		if json.Unmarshal(m.Data, &c) == nil && c.Kind == "renew" {
			renews <- struct{}{}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = nc.Subscribe("agent", func(m *nats.Msg) {
		var r wire.Request
		_ = json.Unmarshal(m.Data, &r)
		h := wire.Frame{Version: 1, ID: r.ID, Kind: "headers", Status: 200, LeaseMS: 1000, RenewMS: 10, Window: 8}
		b, _ := json.Marshal(h)
		_ = nc.Publish(m.Reply, b)
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	tr, err := NewTransport(Config{Conn: nc, Subject: "agent", Limits: Limits{SetupTimeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://logical/bonnie/v1/info", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	time.Sleep(300 * time.Millisecond)
	if n := len(renews); n < 8 {
		t.Fatalf("sent %d renewals in 300ms with a 10ms renew interval", n)
	}
}
