package bonnie

import (
	"context"
	"errors"
	"github.com/mark3labs/bonnie/runtime"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/presence"
)

type presenceTestChannel struct{ agent string }

func (c presenceTestChannel) From(string) channel.SessionRef   { return nil }
func (c presenceTestChannel) Attach(string) channel.SessionRef { return nil }

func (c presenceTestChannel) Name() string            { return "test" }
func (c presenceTestChannel) Routes() []channel.Route { return nil }
func (c presenceTestChannel) AgentIdentity() string   { return c.agent }
func (c presenceTestChannel) PresenceEndpoints() []presence.Endpoint {
	return []presence.Endpoint{{Channel: "test", Address: "tasks", Input: true, Ready: true}}
}

type failingPresenceRegistry struct{}

func (failingPresenceRegistry) Register(context.Context, presence.Record) error {
	return errors.New("offline")
}
func (failingPresenceRegistry) Unregister(context.Context, presence.Identity) error { return nil }

func TestPresenceIdentityAndReadiness(t *testing.T) {
	t.Parallel()
	cfg := PresenceConfig{Registry: presence.NewMemoryStore(), AgentID: "agent"}
	p, err := newPresenceLifecycle(cfg, []Channel{presenceTestChannel{"agent"}})
	if err != nil {
		t.Fatal(err)
	}
	q, err := newPresenceLifecycle(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.id.Instance == "" || p.id.Instance == q.id.Instance {
		t.Fatal("instances must be unique")
	}
	if !p.record(presence.Ready).Endpoints[0].Ready {
		t.Fatal("ready endpoint missing")
	}
	if p.record(presence.Draining).Endpoints[0].Ready {
		t.Fatal("draining endpoint must not be ready")
	}
	if _, err := newPresenceLifecycle(cfg, []Channel{presenceTestChannel{"other"}}); err == nil {
		t.Fatal("accepted mismatched agent identity")
	}
}

func TestPresenceRefreshFailureReturns(t *testing.T) {
	t.Parallel()
	p, err := newPresenceLifecycle(PresenceConfig{Registry: failingPresenceRegistry{}, AgentID: "agent", RefreshInterval: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.run(ctx); err == nil {
		t.Fatal("refresh error was hidden")
	}
}

func TestPresenceInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for _, cfg := range []PresenceConfig{{}, {Registry: presence.NewMemoryStore()}, {Registry: presence.NewMemoryStore(), AgentID: "agent", RefreshInterval: -1}} {
		if _, err := newPresenceLifecycle(cfg, nil); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}

type recordingPresenceRegistry struct {
	mu          sync.Mutex
	events      []string
	cancel      context.CancelFunc
	failRefresh bool
}

func (r *recordingPresenceRegistry) Register(_ context.Context, rec presence.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failRefresh && len(r.events) > 0 && rec.State == presence.Ready {
		return errors.New("refresh failed")
	}
	r.events = append(r.events, string(rec.State))
	if r.cancel != nil && rec.State == presence.Ready {
		r.cancel()
	}
	return nil
}
func (r *recordingPresenceRegistry) Unregister(context.Context, presence.Identity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "removed")
	return nil
}
func TestPresenceHostShutdownOrder(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := &recordingPresenceRegistry{cancel: cancel}
	ch := &lifecycleChannel{name: "test", start: func(context.Context) error { return nil }, shutdown: func(context.Context) error {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		registry.events = append(registry.events, "channel stopped")
		return nil
	}}
	a := lifecycleAgent(t, WithAddr("127.0.0.1:0"), WithPresence(PresenceConfig{Registry: registry, AgentID: "agent"}), WithChannel(func(*runtime.Runner) (Channel, error) { return ch, nil }))
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	want := []string{"ready", "draining", "channel stopped", "removed"}
	if len(registry.events) != len(want) {
		t.Fatalf("events %v", registry.events)
	}
	for i := range want {
		if registry.events[i] != want[i] {
			t.Fatalf("events %v", registry.events)
		}
	}
}
func TestPresenceHostRefreshErrorDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	registry := &recordingPresenceRegistry{failRefresh: true}
	a := lifecycleAgent(t, WithAddr("127.0.0.1:0"), WithPresence(PresenceConfig{Registry: registry, AgentID: "agent", RefreshInterval: time.Millisecond}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("refresh failure hidden")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("presence error deadlocked cleanup")
	}
}
func TestPresenceHostStartupFailureDoesNotAdvertise(t *testing.T) {
	t.Parallel()
	registry := &recordingPresenceRegistry{}
	ch := &lifecycleChannel{name: "test", start: func(context.Context) error { return errors.New("startup failed") }, shutdown: func(context.Context) error { return nil }}
	a := lifecycleAgent(t, WithAddr("127.0.0.1:0"), WithPresence(PresenceConfig{Registry: registry, AgentID: "agent"}), WithChannel(func(*runtime.Runner) (Channel, error) { return ch, nil }))
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("startup failure hidden")
	}
	if len(registry.events) != 0 {
		t.Fatalf("advertised before startup: %v", registry.events)
	}
}
