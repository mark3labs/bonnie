package presence

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStoreRegisterDiscoverAndUnregister(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	first := Record{Identity: Identity{Worker: "worker", Instance: "b"}, State: "ready", Endpoints: []Endpoint{{Address: "http://b", Input: true}}, Labels: map[string]string{"zone": "west"}}
	second := Record{Identity: Identity{Worker: "worker", Instance: "a"}, State: "draining"}
	if err := store.Register(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	second.Identity.Worker = "other"
	if err := store.Register(ctx, second); err != nil {
		t.Fatal(err)
	}
	first.Labels["zone"] = "changed"
	first.Endpoints[0].Address = "changed"
	found, err := store.Discover(ctx, Filter{Labels: map[string]string{"zone": "west"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Endpoints[0].Address != "http://b" {
		t.Fatalf("unexpected discovery: %#v", found)
	}
	found[0].Labels["zone"] = "caller mutation"
	again, err := store.Discover(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || again[0].Identity.Worker != "other" || again[1].Labels["zone"] != "west" {
		t.Fatalf("store changed or results unordered: %#v", again)
	}
	if err := store.Unregister(ctx, first.Identity); err != nil {
		t.Fatal(err)
	}
	if err := store.Unregister(ctx, first.Identity); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryStoreExpiry(t *testing.T) {
	s := NewMemoryStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	r := Record{Identity: Identity{"w", "i"}, ExpiresAt: now.Add(time.Second)}
	if err := s.Register(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if got, err := s.Discover(context.Background(), Filter{}); err != nil || len(got) != 0 {
		t.Fatalf("expired record discovered: %#v %v", got, err)
	}
	if err := s.Register(context.Background(), Record{Identity: Identity{"w", "next"}}); err != nil {
		t.Fatalf("expired owner blocks new instance: %v", err)
	}
}

func TestMemoryWatchDeletionAndCancellation(t *testing.T) {
	s := NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	id := Identity{"watched", "one"}
	if err := s.Register(ctx, Record{Identity: id}); err != nil {
		t.Fatal(err)
	}
	snapshot, events, err := s.Watch(ctx, Filter{Worker: "watched"})
	if err != nil || len(snapshot) != 1 {
		t.Fatalf("snapshot %#v, err %v", snapshot, err)
	}
	if err := s.Unregister(ctx, id); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if !event.Deleted || event.Record.Identity != id {
			t.Fatalf("unexpected event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for deletion")
	}
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("watch channel still open after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not exit after cancellation")
	}
}

func TestRecordValidationAndContext(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	if err := store.Register(context.Background(), Record{}); err == nil {
		t.Fatal("expected identity validation error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Register(ctx, Record{Identity: Identity{Worker: "w", Instance: "i"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if _, err := store.Discover(ctx, Filter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
