package natsapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	api "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/client"
	transport "github.com/mark3labs/bonnie/client/natsapi"
	"github.com/mark3labs/bonnie/internal/fakemodel"
	wire "github.com/mark3labs/bonnie/internal/natsapi"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func broker(t *testing.T, payload int32) (*nats.Conn, *nats.Conn) {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, MaxPayload: payload})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	if !s.ReadyForConnections(3 * time.Second) {
		t.Fatal("broker startup")
	}
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	agent, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.Close)
	caller, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(caller.Close)
	return agent, caller
}
func runner() *runtime.Runner {
	m := fakemodel.New(fakemodel.Say(strings.Repeat("answer", 2000)), fakemodel.Say("second"))
	return runtime.NewRunner(runtime.NewMemoryJournal(), runtime.KitAgent(m.Option(), func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.DisableCoreTools = true
		o.Quiet = true
	}))
}
func auth(r *http.Request) (*channel.Principal, error) {
	if r.Header.Get("Authorization") != "Bearer operator" {
		return nil, api.ErrUnauthenticated
	}
	return &channel.Principal{Authenticator: "test", Kind: "user", ID: "operator"}, nil
}
func setup(t *testing.T, l Limits) (*Channel, *transport.Transport, *client.Client, *http.Client) {
	t.Helper()
	a, b := broker(t, 8192)
	c, err := New(runner(), Config{Conn: a, Subject: "agent.api", Authenticator: auth, Limits: l})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if !a.IsConnected() {
			t.Error("agent connection closed")
		}
	})
	tr, err := transport.NewTransport(transport.Config{Conn: b, Subject: "agent.api", Limits: l})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tr.Close()
		if !b.IsConnected() {
			t.Error("client connection closed")
		}
	})
	hc := &http.Client{Transport: tr}
	cl := client.New("http://bonnie-over-nats", client.WithHTTPClient(hc), client.WithBearerToken("operator"))
	return c, tr, cl, hc
}
func TestExistingClientAndChunkedSnapshot(t *testing.T) {
	c, _, cl, hc := setup(t, Limits{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if h, err := cl.Health(ctx); err != nil || !h.OK {
		t.Fatalf("health: %+v %v", h, err)
	}
	if _, err := client.New("http://unused", client.WithHTTPClient(hc)).Info(ctx); !errors.Is(err, client.ErrUnauthenticated) {
		t.Fatalf("auth: %v", err)
	}
	id, _, err := cl.Ensure(ctx, "address/escaped")
	if err != nil {
		t.Fatal(err)
	}
	if found, _, err := cl.Lookup(ctx, "address/escaped"); err != nil || found != id {
		t.Fatalf("lookup: %q %v", found, err)
	}
	if _, err := c.api.From("address/escaped").RunID(ctx); err != nil {
		t.Fatal(err)
	}
	events, stop, err := cl.Stream(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	run, err := cl.Send(ctx, id, "hello")
	if err != nil || run.ID != id {
		t.Fatalf("send: %+v %v", run, err)
	}
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal("no stream event")
	}
	snap, err := cl.Snapshot(ctx, id)
	if err != nil || snap.RunID != id {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
	if err := cl.Clear(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := cl.Reset(ctx, id, "done"); err != nil {
		t.Fatal(err)
	}
}
func TestQuietStreamRenewsAndCloseReleases(t *testing.T) {
	l := Limits{Lease: 180 * time.Millisecond, Heartbeat: 40 * time.Millisecond, Startup: time.Second, AckTimeout: 200 * time.Millisecond}
	c, _, cl, hc := setup(t, l)
	id, _, err := cl.Ensure(t.Context(), "quiet")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://unused/bonnie/v1/runs/"+id+"/stream", nil)
	req.Header.Set("Authorization", "Bearer operator")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	c.mu.Lock()
	active := len(c.active)
	c.mu.Unlock()
	if active != 1 {
		t.Fatalf("quiet lease lost: %d", active)
	}
	done := make(chan error, 1)
	go func() { _, err := resp.Body.Read(make([]byte, 1)); done <- err }()
	_ = resp.Body.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("close yielded nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("Read blocked after Close")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.active)
		c.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("active request leaked")
}
func TestSlowConsumerFailsWithoutCleanEOF(t *testing.T) {
	c, _, cl, hc := setup(t, Limits{Window: 2, ChunkBytes: 128, AckTimeout: 60 * time.Millisecond, Heartbeat: 20 * time.Millisecond, Lease: 300 * time.Millisecond})
	id, _, err := cl.Ensure(t.Context(), "slow")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cl.Send(t.Context(), id, "hello"); err != nil {
		t.Fatal(err)
	}
	// Make a large response without consuming its body.
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://unused/bonnie/v1/runs/"+id+"/snapshot", nil)
	req.Header.Set("Authorization", "Bearer operator")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	time.Sleep(180 * time.Millisecond)
	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("slow response became clean EOF")
	}
	c.mu.Lock()
	n := len(c.active)
	c.mu.Unlock()
	if n != 0 {
		t.Fatal("slow observer leaked")
	}
}
func TestControlCapabilityAndReplay(t *testing.T) {
	c, _, _, _ := setup(t, Limits{})
	r := wire.Request{ID: strings.Repeat("a", 32), Secret: strings.Repeat("b", 64)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := newWriter(c, ctx, cancel, "_INBOX.bonnieapi."+strings.Repeat("c", 32), r)
	c.mu.Lock()
	c.active[key(w.inbox, r.ID)] = w
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.active, key(w.inbox, r.ID)); c.mu.Unlock() }()
	send := func(secret, kind string, seq uint64) {
		data, _ := json.Marshal(wire.Control{Version: 1, ID: r.ID, Inbox: w.inbox, Secret: secret, Kind: kind, Seq: seq})
		c.control(&nats.Msg{Data: data})
	}
	send(strings.Repeat("d", 64), "cancel", 1)
	if ctx.Err() != nil {
		t.Fatal("guessed capability cancelled")
	}
	send(r.Secret, "renew", 1)
	w.controlMu.Lock()
	lease := w.leaseUntil
	w.controlMu.Unlock()
	send(r.Secret, "renew", 1)
	w.controlMu.Lock()
	replayed := w.leaseUntil
	w.controlMu.Unlock()
	if !replayed.Equal(lease) {
		t.Fatal("replay extended lease")
	}
	send(r.Secret, "cancel", 2)
	if ctx.Err() == nil {
		t.Fatal("cancel ignored")
	}
}
