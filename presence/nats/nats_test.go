package nats

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/presence"
	"github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
)

func testStore(t *testing.T, ttl time.Duration) (*Store, *natsgo.Conn) {
	t.Helper()
	opts := &server.Options{JetStream: true, StoreDir: t.TempDir(), Port: -1}
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("NATS server did not start")
	}
	t.Cleanup(srv.Shutdown)
	conn, err := natsgo.Connect(srv.ClientURL(), natsgo.Timeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	store, err := New(context.Background(), conn, Config{Bucket: fmt.Sprintf("P%d", time.Now().UnixNano()), TTL: ttl, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	return store, conn
}

func record(worker, instance string) presence.Record {
	return presence.Record{Identity: presence.Identity{Worker: worker, Instance: instance}, State: "ready"}
}

func TestOwnershipConflictAndRefresh(t *testing.T) {
	s, _ := testStore(t, 2*time.Second)
	ctx := context.Background()
	a := record("worker", "one")
	if err := s.Register(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ctx, record("worker", "two")); !errors.Is(err, presence.ErrConflict) {
		t.Fatalf("Register conflict = %v", err)
	}
	before, err := s.Discover(ctx, presence.Filter{})
	if err != nil || len(before) != 1 {
		t.Fatalf("discover = %v, %v", before, err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.Register(ctx, a); err != nil {
		t.Fatal(err)
	}
	after, err := s.Discover(ctx, presence.Filter{})
	if err != nil || !after[0].UpdatedAt.After(before[0].UpdatedAt) {
		t.Fatalf("refresh did not update timestamp: %v %v", after, err)
	}
}

func TestOldInstanceCannotDeleteReplacement(t *testing.T) {
	s, _ := testStore(t, 2*time.Second)
	ctx := context.Background()
	old := record("worker", "old")
	if err := s.Register(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Unregister(ctx, old.Identity); err != nil {
		t.Fatal(err)
	}
	newer := record("worker", "new")
	if err := s.Register(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if err := s.Unregister(ctx, old.Identity); !errors.Is(err, presence.ErrNotFound) {
		t.Fatalf("old unregister = %v", err)
	}
	got, err := s.Discover(ctx, presence.Filter{})
	if err != nil || len(got) != 1 || got[0].Identity != newer.Identity {
		t.Fatalf("replacement lost: %v, %v", got, err)
	}
}

func TestTTLExpiryAllowsReplacement(t *testing.T) {
	s, _ := testStore(t, 150*time.Millisecond)
	ctx := context.Background()
	if err := s.Register(ctx, record("worker", "one")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.Discover(ctx, presence.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, err := s.Discover(ctx, presence.Filter{})
	if err != nil || len(got) != 0 {
		t.Fatalf("expired record remains: %v %v", got, err)
	}
	if err := s.Register(ctx, record("worker", "two")); err != nil {
		t.Fatalf("replacement register: %v", err)
	}
}

func TestWatchSnapshotDeleteIdentityAndCancel(t *testing.T) {
	s, _ := testStore(t, 3*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initial := record("worker", "one")
	if err := s.Register(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	snapshot, events, err := s.Watch(ctx, presence.Filter{})
	if err != nil || len(snapshot) != 1 || snapshot[0].Identity != initial.Identity {
		t.Fatalf("snapshot = %v, %v", snapshot, err)
	}
	if err := s.Unregister(context.Background(), initial.Identity); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		if !ev.Deleted || ev.Record.Identity != initial.Identity {
			t.Fatalf("delete event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not report deletion")
	}
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			for range events {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("watch channel did not close on cancel")
	}
}

func TestConfigTTLMatch(t *testing.T) {
	_, conn := testStore(t, time.Second)
	// Create a known bucket with a TTL different from the requested configuration.
	js, e := conn.JetStream()
	if e != nil {
		t.Fatal(e)
	}
	_, e = js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "TTL_MISMATCH", TTL: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	_, err := New(context.Background(), conn, Config{Bucket: "TTL_MISMATCH", TTL: 2 * time.Second})
	if err == nil {
		t.Fatal("expected TTL mismatch")
	}
}
