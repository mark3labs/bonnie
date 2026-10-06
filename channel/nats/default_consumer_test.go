package nats

import (
	"testing"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// Default names depend only on the exact subject, never worker identity.
// Explicit names preserve existing durable consumers and their acknowledgement state.
func TestDefaultConsumer(t *testing.T) {
	t.Parallel()
	cfg := Config{URL: "nats://localhost:4222", Subject: "agents.review.tasks", AnswerSubject: "answers", ResultSubject: "results", Stream: "INPUT", WorkerID: "one"}
	first, err := New(testRunner(fakemodel.New()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	name := first.cfg.Consumer
	if !safeToken(name) || name != DefaultConsumerName(cfg.Subject) {
		t.Fatalf("invalid default: %q", name)
	}
	cfg.WorkerID = "two"
	second, err := New(testRunner(fakemodel.New()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second.cfg.Consumer != name || cfg.Consumer != "" {
		t.Fatal("default varies by worker or changes caller config")
	}
	cfg.Subject = "agents.other.tasks"
	other, err := New(testRunner(fakemodel.New()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if other.cfg.Consumer == name {
		t.Fatal("different subjects share default")
	}
	cfg.Consumer = "custom-workers"
	override, err := New(testRunner(fakemodel.New()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if override.cfg.Consumer != cfg.Consumer {
		t.Fatal("explicit consumer was replaced")
	}
	cfg.Stream, cfg.Consumer, cfg.WorkerID = "", "", ""
	core, err := New(testRunner(fakemodel.New()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if core.cfg.Consumer != "" {
		t.Fatal("Core mode received JetStream default")
	}
}
