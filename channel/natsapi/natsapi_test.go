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
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/client"
	clientapi "github.com/mark3labs/bonnie/client/natsapi"
	wire "github.com/mark3labs/bonnie/internal/natsapi"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func broker(t *testing.T, maxPayload int32) (*nats.Conn, *nats.Conn) {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, MaxPayload: maxPayload})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	if !s.ReadyForConnections(3 * time.Second) {
		t.Fatal("broker startup")
	}
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	a, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	b, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	return a, b
}
func auth(r *http.Request) (*channel.Principal, error) {
	if r.Header.Get("Authorization") != "Bearer operator" {
		return nil, bonniehttp.ErrUnauthenticated
	}
	return &channel.Principal{Authenticator: "test", Kind: "user", ID: "operator"}, nil
}

type agent struct{ session *runtime.Session }

func (a *agent) PromptResult(ctx context.Context, text string) (*kit.TurnResult, error) {
	_, err := a.session.AppendMessage(kit.NewLLMUserMessage(text))
	if err != nil {
		return nil, err
	}
	_, err = a.session.AppendMessage(kit.LLMMessage{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "done"}}})
	return &kit.TurnResult{Response: "done"}, err
}
func (a *agent) InjectSteer(string) {}
func (a *agent) Close() error       { return nil }
func runner() *runtime.Runner {
	return runtime.NewRunner(runtime.NewMemoryJournal(), func(ctx context.Context, s *runtime.Session) (runtime.Agent, error) { return &agent{session: s}, nil })
}
func bridge(t *testing.T, nc *nats.Conn, subject string, limits Limits) *Channel {
	t.Helper()
	c, err := New(runner(), Config{Conn: nc, Subject: subject, Authenticator: auth, Limits: limits})
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
	})
	return c
}
func transport(t *testing.T, nc *nats.Conn, subject string) *clientapi.Transport {
	t.Helper()
	tr, err := clientapi.NewTransport(clientapi.Config{Conn: nc, Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}
func waitEmpty(t *testing.T, c *Channel) {
	t.Helper()
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
	t.Fatal("requests leaked")
}
func TestExistingClientRoutingSnapshotAndStream(t *testing.T) {
	a, b := broker(t, 0)
	c := bridge(t, a, "bonnie.api.agent.one", Limits{})
	other := bridge(t, a, "bonnie.api.agent.two", Limits{})
	if err := c.PrepareHost(nil, channel.HostInfo{}); err == nil {
		t.Fatal("prepared after start")
	}
	tr := transport(t, b, c.cfg.Subject)
	cli := client.New("http://bonnie-over-nats", client.WithHTTPClient(&http.Client{Transport: tr}), client.WithBearerToken("operator"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := cli.Info(ctx); err != nil {
		t.Fatal(err)
	}
	id, _, err := cli.Ensure(ctx, "address/escaped")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Send(ctx, id, "same run"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := cli.Snapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 2 || snapshot.RunID != id {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	otherClient := client.New("http://bonnie-over-nats", client.WithHTTPClient(&http.Client{Transport: transport(t, b, other.cfg.Subject)}), client.WithBearerToken("operator"))
	if _, err := otherClient.Snapshot(ctx, id); err == nil {
		t.Fatal("wrong agent could read run")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://bonnie-over-nats/bonnie/v1/runs/"+id+"/stream?cursor=0", nil)
	req.Header.Set("Authorization", "Bearer operator")
	started := time.Now()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatal("late/missing stream headers")
	}
	decoder := json.NewDecoder(resp.Body)
	var ev runtime.Event
	if err = decoder.Decode(&ev); err != nil {
		t.Fatal(err)
	}
	if ev.RunID != id {
		t.Fatal(ev)
	}
	_ = resp.Body.Close()
	waitEmpty(t, c)
	if err = cli.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = cli.Clear(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = cli.Reset(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	if !a.IsConnected() || !b.IsConnected() {
		t.Fatal("caller-owned connection closed")
	}
}
func TestAuthenticationAndRawHTTPParity(t *testing.T) {
	a, b := broker(t, 0)
	c := bridge(t, a, "api", Limits{})
	tr := transport(t, b, "api")
	for _, tc := range []struct {
		path, token string
		want        int
	}{{"health", "", 200}, {"info", "", 401}, {"info", "wrong", 401}, {"info", "operator", 200}, {"not-a-route", "operator", 404}} {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://logical/bonnie/v1/"+tc.path, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.want {
			t.Fatal(tc, resp.StatusCode)
		}
		if _, err = io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	nilAuth, err := New(runner(), Config{Conn: a, Subject: "nil-auth", Authenticator: func(*http.Request) (*channel.Principal, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = nilAuth.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nilAuth.Shutdown(context.Background()) }()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://logical/bonnie/v1/info", nil)
	resp, err := transport(t, b, "nil-auth").RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
	_ = resp.Body.Close()
	waitEmpty(t, c)
}
func TestLargeResponsesAndBrokerPayloadAccounting(t *testing.T) {
	a, b := broker(t, 4096)
	c, err := New(runner(), Config{Conn: a, Subject: "small", Authenticator: auth})
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("snapshot-data", 20000)
	c.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, content)
	})
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Shutdown(context.Background()) }()
	tr := transport(t, b, "small")
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://logical/bonnie/v1/info", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatal("chunked response changed")
	}
	_ = resp.Body.Close()
	req, _ = http.NewRequestWithContext(t.Context(), "POST", "http://logical/bonnie/v1/runs", strings.NewReader(strings.Repeat("x", 4000)))
	if _, err = tr.RoundTrip(req); err == nil {
		t.Fatal("base64 envelope exceeded payload but accepted")
	}
	waitEmpty(t, c)
}
func TestCapabilityAndLease(t *testing.T) {
	a, b := broker(t, 0)
	limits := Limits{Lease: 150 * time.Millisecond, Renew: 30 * time.Millisecond, AckTimeout: 50 * time.Millisecond}
	c, err := New(runner(), Config{Conn: a, Subject: "lease", Authenticator: auth, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	exited := make(chan struct{}, 1)
	c.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-r.Context().Done()
		exited <- struct{}{}
	})
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Shutdown(context.Background()) }()
	inbox := wire.DefaultInboxPrefix + "." + wire.Token(16)
	sub, err := b.SubscribeSync(inbox)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = b.Flush()
	req := wire.Request{Version: 1, ID: wire.Token(16), Secret: wire.Token(32), Method: "GET", Path: "/bonnie/v1/info"}
	data, _ := json.Marshal(req)
	_ = b.PublishRequest("lease", inbox, data)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no handler")
	}
	ctrl := wire.Control{Version: 1, ID: req.ID, Inbox: inbox, Secret: wire.Token(32), Sequence: 1, Kind: "cancel"}
	data, _ = json.Marshal(ctrl)
	_ = b.Publish("lease.control", data)
	_ = b.Flush()
	select {
	case <-exited:
		t.Fatal("guessed secret cancelled")
	case <-time.After(20 * time.Millisecond):
	}
	// A crashed client which never renews releases the HTTP handler.
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("lease did not expire")
	}
	waitEmpty(t, c)
}
func TestAdmissionSlowConsumerAndShutdown(t *testing.T) {
	a, b := broker(t, 0)
	c, err := New(runner(), Config{Conn: a, Subject: "bounded", Authenticator: auth, Limits: Limits{MaxRequests: 1, MaxStreams: 1, Window: 1, AckTimeout: 40 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	c.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Shutdown(context.Background()) }()
	tr := transport(t, b, "bounded")
	request := func(path string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://logical/bonnie/v1/"+path, nil)
		return tr.RoundTrip(req)
	}
	resp, err := request("info")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = request("info"); err == nil {
		t.Fatal("ordinary admission not bounded")
	}
	stream, err := request("runs/r/stream")
	if err != nil {
		t.Fatal("stream pool not independent", err)
	}
	_ = stream.Body.Close()
	_ = resp.Body.Close()
	waitEmpty(t, c)
	// A non-reading body cannot accumulate unbounded response chunks.
	c.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", 100000)) })
	resp, err = request("info")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	waitEmpty(t, c)
	if _, err = io.ReadAll(resp.Body); err == nil {
		t.Fatal("slow consumer appeared as clean EOF")
	}
	_ = resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.IsConnected() || !b.IsConnected() {
		t.Fatal("closed caller connection")
	}
}

func TestCancelBeforeHeadersPanicAndShutdown(t *testing.T) {
	a, b := broker(t, 0)
	c, err := New(runner(), Config{Conn: a, Subject: "lifecycle-api", Authenticator: auth})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	c.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("panic") == "1" {
			panic("private panic payload")
		}
		entered <- struct{}{}
		<-r.Context().Done()
	})
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Shutdown(context.Background()) }()
	tr := transport(t, b, "lifecycle-api")
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://logical/bonnie/v1/info", nil)
	done := make(chan error, 1)
	go func() { _, err := tr.RoundTrip(req); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release RoundTrip")
	}
	waitEmpty(t, c)
	req, _ = http.NewRequestWithContext(t.Context(), "GET", "http://logical/bonnie/v1/info?panic=1", nil)
	if _, err = tr.RoundTrip(req); err == nil || strings.Contains(err.Error(), "private panic") {
		t.Fatal("panic not safely recovered", err)
	}
	waitEmpty(t, c)
	req, _ = http.NewRequestWithContext(t.Context(), "GET", "http://logical/bonnie/v1/info", nil)
	go func() { _, err := tr.RoundTrip(req); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err = c.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown response not released")
	}
	waitEmpty(t, c)
}

func TestQuietStreamRenewsLease(t *testing.T) {
	a, b := broker(t, 0)
	limits := Limits{Lease: 120 * time.Millisecond, Renew: 20 * time.Millisecond, AckTimeout: 80 * time.Millisecond}
	c, err := New(runner(), Config{Conn: a, Subject: "quiet", Authenticator: auth, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	c.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Shutdown(context.Background()) }()
	tr := transport(t, b, "quiet")
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://logical/bonnie/v1/runs/r/stream", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resp.Body.Read(make([]byte, 1))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("quiet stream did not survive renewals", err)
	}
	_ = resp.Body.Close()
	waitEmpty(t, c)
}
