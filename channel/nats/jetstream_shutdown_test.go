package nats

import (
	"context"
	"testing"

	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/runtime"
)

type cancellingResultLookup struct {
	gonats.JetStreamContext
	cancel context.CancelFunc
}

func (js cancellingResultLookup) StreamNameBySubject(string, ...gonats.JSOpt) (string, error) {
	js.cancel()
	return "", context.Canceled
}

// Shutdown can cancel a broker lookup after publication starts. This is not a
// channel failure, and the unacknowledged input must remain available for retry.
func TestJetStreamShutdownDuringResultSizeCheck(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	c, err := New(runtime.NewRunner(nil, nil), jsConfig(nc, "one"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.conn = nc
	c.js = cancellingResultLookup{JetStreamContext: js, cancel: cancel}
	c.publishJetStream(ctx, nil, Result{Version: 1, State: runtime.RunCompleted}, "result")
	if ctx.Err() == nil {
		t.Fatal("result-size lookup did not cancel the context")
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown reported cancellation as a failure: %v", err)
	}
}
