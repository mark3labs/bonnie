package bonnie

import (
	"testing"

	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/runtime"
)

// Environment fallback is resolved for each build, not when the option is made.
func TestWithNATS(t *testing.T) {
	t.Setenv("NATS_URL", "nats://127.0.0.1:4222")
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
