package bonnie

import (
	"testing"

	gonats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/runtime"
)

// Environment fallback is resolved for each build, not when the option is made.
func TestWithNATS(t *testing.T) {
	clearNATSAuthEnv(t)
	t.Setenv("NATS_URL", "nats://127.0.0.1:4222")
	t.Setenv("NATS_NKEY_SEED", "")
	c := defaults()
	WithNATS(natschannel.Config{Subject: "agent.tasks", AnswerSubject: "agent.answers", ResultSubject: "agent.results"})(c)
	r := runtime.NewRunner(runtime.NewMemoryJournal(), nil)
	ch, err := c.channels[0](r)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Name() != "nats" || len(ch.Routes()) != 0 {
		t.Fatalf("unexpected channel: %s", ch.Name())
	}
	t.Setenv("NATS_URL", "")
	if _, err := c.channels[0](r); err == nil {
		t.Fatal("missing URL must fail")
	}
}

// Seed fallback is resolved for each build. Explicit settings win, and a
// caller-owned connection must not receive environment authentication settings.
func TestWithNATSSeedFallback(t *testing.T) {
	clearNATSAuthEnv(t)
	t.Setenv("NATS_URL", "nats://127.0.0.1:4222")
	t.Setenv("NATS_NKEY_SEED", "invalid-secret")
	key, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	defer key.Wipe()
	seed, err := key.Seed()
	if err != nil {
		t.Fatal(err)
	}
	r := runtime.NewRunner(runtime.NewMemoryJournal(), nil)
	cfg := natschannel.Config{Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"}
	c := defaults()
	WithNATS(cfg)(c)
	if _, err := c.channels[0](r); err == nil {
		t.Fatal("invalid environment seed must fail")
	}
	t.Setenv("NATS_NKEY_SEED", string(seed))
	if _, err := c.channels[0](r); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NATS_NKEY_SEED", "invalid-secret")
	cfg.NKeySeed = string(seed)
	c = defaults()
	WithNATS(cfg)(c)
	if _, err := c.channels[0](r); err != nil {
		t.Fatal(err)
	}
	cfg.NKeySeed = ""
	cfg.Conn = &gonats.Conn{}
	c = defaults()
	WithNATS(cfg)(c)
	if _, err := c.channels[0](r); err != nil {
		t.Fatal(err)
	}
}

func clearNATSAuthEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"NATS_NKEY_SEED", "NATS_TOKEN", "NATS_USERNAME", "NATS_PASSWORD"} {
		t.Setenv(name, "")
	}
}

// Authentication fallbacks must be applied at build time, without silently
// selecting one method when multiple methods are configured.
func TestWithNATSTokenAndUserFallback(t *testing.T) {
	clearNATSAuthEnv(t)
	t.Setenv("NATS_URL", "nats://127.0.0.1:4222")
	r := runtime.NewRunner(runtime.NewMemoryJournal(), nil)
	cfg := natschannel.Config{Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"}
	c := defaults()
	WithNATS(cfg)(c)
	t.Setenv("NATS_PASSWORD", "secret")
	if _, err := c.channels[0](r); err == nil {
		t.Fatal("password fallback must require username")
	}
	t.Setenv("NATS_USERNAME", "user")
	if _, err := c.channels[0](r); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NATS_TOKEN", "token")
	if _, err := c.channels[0](r); err == nil {
		t.Fatal("mixed environment methods must fail")
	}
	t.Setenv("NATS_USERNAME", "")
	t.Setenv("NATS_PASSWORD", "")
	if _, err := c.channels[0](r); err != nil {
		t.Fatal(err)
	}
	cfg.Conn = &gonats.Conn{}
	t.Setenv("NATS_PASSWORD", "secret")
	c = defaults()
	WithNATS(cfg)(c)
	if _, err := c.channels[0](r); err != nil {
		t.Fatal(err)
	}
}
