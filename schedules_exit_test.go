package bonnie

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/schedule"
)

// A bad occurrence makes Reconcile fail while Run serves. Consuming the
// schedule exit must not leave shutdown waiting for the same one-value channel.
func TestScheduleRunReconcileErrorDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const shutdown = 5 * time.Second
	a := lifecycleAgent(t, WithListener(ln), WithShutdownTimeout(shutdown),
		WithScheduleClock(false), WithSchedule(schedule.Definition{
			Name: "host", Cron: "0 9 * * *", TimeZone: "UTC",
			Run: func(context.Context, schedule.Fire) error { return nil },
		}), WithChannel(func(r *runtime.Runner) (Channel, error) {
			return &lifecycleChannel{name: "bad-schedule",
				start: func(ctx context.Context) error {
					_, err := r.Journal().Append(ctx, runtime.Record{
						RunID: runtime.ReservedRunPrefix + "schedule", Kind: runtime.RecordExtensionData,
						ExtType: "schedule.occurrence", Payload: []byte(`{"fire":42}`),
					})
					return err
				},
				shutdown: func(context.Context) error { return nil },
			}, nil
		}))
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "bonnie: schedule: decode occurrence:") {
			t.Fatalf("Run error = %v, want occurrence decode error", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown waited for the consumed schedule exit: %v", err)
		}
	case <-time.After(shutdown):
		cancel()
		t.Fatalf("Run did not return within the shutdown timeout; after cancel it returned %v", <-done)
	}
}
