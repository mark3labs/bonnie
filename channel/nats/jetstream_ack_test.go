package nats

import (
	"context"
	"errors"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

type cancelAfterPublish struct {
	gonats.JetStreamContext
	cancel context.CancelFunc
}

func (j cancelAfterPublish) PublishMsg(msg *gonats.Msg, opts ...gonats.PubOpt) (*gonats.PubAck, error) {
	ack, err := j.JetStreamContext.PublishMsg(msg, opts...)
	if err == nil {
		j.cancel()
	}
	return ack, err
}

// Shutdown after publication but before AckSync must return the no-ack outcome,
// without adding an operator failure for cancellation.
func TestJetStreamCancelledAckOutcome(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	resultStream(t, js)
	inputStream(t, js, time.Second)
	sub, err := js.PullSubscribe("tasks", "agents", gonats.Bind("INPUT", "agents"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	jsSend(t, js, "tasks", Task{Version: 1, TaskID: "a", Text: "work"})
	msgs, err := sub.Fetch(1, gonats.MaxWait(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(testRunner(fakemodel.New()), jsConfig(nc, "one"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.conn, c.js = nc, cancelAfterPublish{JetStreamContext: js, cancel: cancel}
	err = c.publishJetStreamResult(ctx, msgs[0], Result{Version: 1, TaskID: "a"}, "ack-test")
	if !errors.Is(err, errNoAck) {
		t.Fatalf("publication outcome = %v, want errNoAck", err)
	}
	if c.failure != nil {
		t.Fatalf("shutdown failure = %v", c.failure)
	}
}
