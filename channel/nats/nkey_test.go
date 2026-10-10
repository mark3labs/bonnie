package nats

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	gonats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// A seed must complete real broker authentication in both delivery modes.
// Shutdown must close the connection that the channel opened.
func TestNKeySeedAuthentication(t *testing.T) {
	t.Parallel()
	for _, jetstream := range []bool{false, true} {
		name := "core"
		if jetstream {
			name = "jetstream"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key, err := nkeys.CreateUser()
			if err != nil {
				t.Fatal(err)
			}
			defer key.Wipe()
			seed, err := key.Seed()
			if err != nil {
				t.Fatal(err)
			}
			pub, err := key.PublicKey()
			if err != nil {
				t.Fatal(err)
			}
			s, err := server.NewServer(&server.Options{
				Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
				Nkeys:     []*server.NkeyUser{{Nkey: pub}},
				JetStream: jetstream, StoreDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			s.Start()
			t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
			if !s.ReadyForConnections(5 * time.Second) {
				t.Fatal("server did not start")
			}
			nc, err := gonats.Connect(s.ClientURL(), gonats.Nkey(pub, key.Sign))
			if err != nil {
				t.Fatal(err)
			}
			defer nc.Close()
			cfg := Config{URL: s.ClientURL(), NKeySeed: string(seed), Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"}
			if jetstream {
				cfg.Stream, cfg.AgentID, cfg.CreateStream = "TASKS", "agent-1", true
				js, err := nc.JetStream()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := js.AddStream(&gonats.StreamConfig{Name: "RESULTS", Subjects: []string{"results"}}); err != nil {
					t.Fatal(err)
				}
			}
			c, err := New(testRunner(fakemodel.New(fakemodel.Say("done"))), cfg)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := nc.SubscribeSync("results")
			if err != nil {
				t.Fatal(err)
			}
			if err := nc.Flush(); err != nil {
				t.Fatal(err)
			}
			if err := c.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := c.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
				if !c.conn.IsClosed() {
					t.Error("channel-owned connection is still open")
				}
			})
			send(t, nc, "tasks", Task{Version: 1, TaskID: "task-1", Text: "hello"})
			if result := receive(t, sub); result.Error != "" || result.Response != "done" {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}

// Reject malformed and non-user seeds at construction without disclosing them.
func TestNKeySeedValidation(t *testing.T) {
	t.Parallel()
	account, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	defer account.Wipe()
	seed, err := account.Seed()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"not-a-secret-seed", string(seed)} {
		_, err := New(testRunner(fakemodel.New()), Config{
			URL: "nats://localhost:4222", NKeySeed: value,
			Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results",
		})
		if err == nil || strings.Contains(err.Error(), value) {
			t.Fatalf("expected a safe validation error, got %v", err)
		}
	}
	nc := testServer(t)
	if _, err := New(testRunner(fakemodel.New()), Config{
		Conn: nc, NKeySeed: "secret", Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results",
	}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("expected a safe Conn conflict error, got %v", err)
	}
}
