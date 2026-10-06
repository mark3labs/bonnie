package nats

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

// Token and user/password authentication must deliver tasks and results in
// both modes. Shutdown must close the connection that the channel opened.
func TestTokenAndUserPasswordAuthentication(t *testing.T) {
	t.Parallel()
	for _, auth := range []string{"token", "user_password", "empty_password"} {
		for _, jetstream := range []bool{false, true} {
			mode := "core"
			if jetstream {
				mode = "jetstream"
			}
			t.Run(auth+"/"+mode, func(t *testing.T) {
				t.Parallel()
				opts := &server.Options{
					Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
					JetStream: jetstream, StoreDir: t.TempDir(),
				}
				cfg := Config{Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"}
				var clientAuth gonats.Option
				if auth == "token" {
					cfg.Token = "test-token-secret"
					opts.Authorization = cfg.Token
					clientAuth = gonats.Token(cfg.Token)
				} else {
					cfg.Username = "test-user"
					if auth == "user_password" {
						cfg.Password = "test-password-secret"
					}
					opts.Users = []*server.User{{Username: cfg.Username, Password: cfg.Password}}
					clientAuth = gonats.UserInfo(cfg.Username, cfg.Password)
				}
				s, err := server.NewServer(opts)
				if err != nil {
					t.Fatal(err)
				}
				s.Start()
				t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
				if !s.ReadyForConnections(5 * time.Second) {
					t.Fatal("server did not start")
				}
				nc, err := gonats.Connect(s.ClientURL(), clientAuth)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(nc.Close)
				cfg.URL = s.ClientURL()
				if jetstream {
					cfg.Stream, cfg.WorkerID, cfg.CreateStream = "TASKS", "worker-1", true
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
				if result := receive(t, sub); result.Error != "" || result.Response != "done" || result.TaskID != "task-1" || result.State != runtime.RunCompleted {
					t.Fatalf("unexpected result: %+v", result)
				}
			})
		}
	}
}

// Reject conflicting credentials before connecting. Errors must identify the
// validation rule without disclosing credentials, including URL credentials.
func TestAuthenticationValidation(t *testing.T) {
	t.Parallel()
	const (
		token       = "private-token-value"
		user        = "private-user-value"
		password    = "private-password-value"
		seed        = "private-seed-value"
		urlUser     = "private-url-user"
		urlPassword = "private-url-password"
	)
	for _, tc := range []struct {
		name   string
		config Config
		want   string
	}{
		{"token_nkey", Config{Token: token, NKeySeed: seed}, "use only one authentication method"},
		{"token_user", Config{Token: token, Username: user}, "use only one authentication method"},
		{"token_password", Config{Token: token, Password: password}, "use only one authentication method"},
		{"nkey_user_password", Config{NKeySeed: seed, Username: user, Password: password}, "use only one authentication method"},
		{"all_methods", Config{Token: token, NKeySeed: seed, Username: user, Password: password}, "use only one authentication method"},
		{"password_only", Config{Password: password}, "password requires username"},
		{"url_token", Config{URL: "nats://" + urlUser + ":" + urlPassword + "@localhost:4222", Token: token}, "use URL credentials or authentication fields, not both"},
		{"url_user_password", Config{URL: "nats://" + urlUser + ":" + urlPassword + "@localhost:4222", Username: user, Password: password}, "use URL credentials or authentication fields, not both"},
		{"url_token_user", Config{URL: "nats://" + urlUser + "@localhost:4222", Username: user}, "use URL credentials or authentication fields, not both"},
		{"conn_token", Config{Token: token}, "use URL and authentication fields or Conn, not both"},
		{"conn_user", Config{Username: user}, "use URL and authentication fields or Conn, not both"},
		{"conn_password", Config{Password: password}, "use URL and authentication fields or Conn, not both"},
		{"conn_user_password", Config{Username: user, Password: password}, "use URL and authentication fields or Conn, not both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.config
			if strings.HasPrefix(tc.name, "conn_") {
				cfg.Conn = testServer(t)
			} else if cfg.URL == "" {
				cfg.URL = "nats://localhost:4222"
			}
			cfg.Subject, cfg.AnswerSubject, cfg.ResultSubject = "tasks", "answers", "results"
			c, err := New(testRunner(fakemodel.New()), cfg)
			if err == nil || c != nil {
				t.Fatal("expected construction to reject credentials")
			}
			for _, secret := range []string{token, user, password, seed, urlUser, urlPassword} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("validation error disclosed credentials")
				}
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected validation rule %q, got %v", tc.want, err)
			}
			if cfg.Conn != nil && cfg.Conn.IsClosed() {
				t.Error("validation closed the caller-owned connection")
			}
		})
	}
}
