package nats

import (
	"context"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
)

// Defaults bind the same durable resources across clients, without granting
// implicit creation permission. Explicit resource names are never replaced.
func TestResultDefaults(t *testing.T) {
	t.Parallel()
	nc, js := broker(t)
	cfg := Config{TaskSubject: "tasks", AnswerSubject: "answers", ResultSubject: "results"}
	if _, err := New(nc, cfg); err == nil {
		t.Fatal("missing stream must fail without CreateStream")
	}
	cfg.CreateStream = true
	first, err := New(nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stream, consumer := DefaultResultStreamName(cfg.ResultSubject), DefaultResultConsumerName(cfg.ResultSubject)
	if first.cfg.ResultStream != stream || first.cfg.ResultConsumer != consumer || !safeToken(stream) || !safeToken(consumer) {
		t.Fatalf("unexpected defaults: %+v", first.cfg)
	}
	if cfg.ResultStream != "" || cfg.ResultConsumer != "" {
		t.Fatal("caller config changed")
	}
	second, err := New(nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second.cfg.ResultStream != stream || second.cfg.ResultConsumer != consumer {
		t.Fatal("defaults are not stable")
	}
	if _, err := js.Publish("results", []byte(`{"version":1,"task_id":"one"}`)); err != nil {
		t.Fatal(err)
	}
	sub, err := js.PullSubscribe("results", consumer, gonats.Bind(stream, consumer))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	msgs, err := sub.Fetch(1, gonats.Context(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if err := msgs[0].AckSync(gonats.Context(ctx)); err != nil {
		t.Fatal(err)
	}
	ci, err := second.js.ConsumerInfo(stream, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if ci.AckFloor.Stream != 1 {
		t.Fatalf("acknowledgement state lost: %+v", ci.AckFloor)
	}
	cfg.ResultConsumer = "audit-reader"
	audit, err := New(nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if audit.cfg.ResultStream != stream || audit.cfg.ResultConsumer != "audit-reader" {
		t.Fatal("consumer override replaced")
	}
	cfg.ResultSubject, cfg.ResultStream = "other.results", "OPERATOR"
	other, err := New(nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if other.cfg.ResultStream != "OPERATOR" {
		t.Fatal("stream override replaced")
	}
	if DefaultResultStreamName("other.results") == stream || DefaultResultConsumerName("other.results") == consumer {
		t.Fatal("subjects share default names")
	}
}
