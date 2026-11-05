package bonnie

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/workflow"
)

// Unconfigured agents must not open another service, scan records, or start
// a workflow goroutine. Even a nil journal is safe on this path.
func TestWorkflowsAbsentDoesNotStartService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	got, stop, err := resolve().startWorkflows(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != ctx {
		t.Fatal("replaced unconfigured context")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowSetupRequiresDefinitions(t *testing.T) {
	t.Parallel()
	_, _, err := resolve(WithWorkflowSetup(func(context.Context, *workflow.Service) error { t.Error("setup called"); return nil })).startWorkflows(context.Background(), runtime.NewMemoryJournal())
	if err == nil {
		t.Fatal("accepted setup without definitions")
	}
}

func TestWorkflowRootLifecycle(t *testing.T) {
	t.Parallel()
	d := workflow.Define("root", "1", func(ctx context.Context, n int) (int, error) {
		return workflow.Step(ctx, "double", func(context.Context, workflow.Attempt) (int, error) { return n * 2, nil })
	})
	var handle *workflow.Handle[int]
	cfg := resolve(WithWorkflows(d), WithWorkflowSetup(func(ctx context.Context, s *workflow.Service) error {
		var err error
		handle, err = workflow.Start(ctx, s, d, "root-request", 21)
		return err
	}))
	ctx, stop, err := cfg.startWorkflows(context.Background(), runtime.NewMemoryJournal())
	if err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, resultErr := handle.Result(wait)
	stopErr := stop()
	if resultErr != nil {
		t.Fatal(resultErr)
	}
	if stopErr != nil {
		t.Fatal(stopErr)
	}
	if got != 42 {
		t.Fatal(got)
	}
	if ctx.Err() == nil {
		t.Fatal("lifecycle context not cancelled")
	}
}

func TestWorkflowAgentRunIntegration(t *testing.T) {
	t.Parallel()
	d := workflow.Define("served", "1", func(ctx context.Context, n int) (int, error) {
		return workflow.Step(ctx, "work", func(context.Context, workflow.Attempt) (int, error) { return n + 1, nil })
	})
	setup := make(chan *workflow.Handle[int], 1)
	a := lifecycleAgent(t, WithHTTPListenerDisabled(), WithWorkflows(d), WithWorkflowSetup(func(ctx context.Context, s *workflow.Service) error {
		handle, err := workflow.Start(ctx, s, d, "served", 4)
		if err == nil {
			setup <- handle
		}
		return err
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var handle *workflow.Handle[int]
	select {
	case handle = <-setup:
	case err := <-done:
		t.Fatalf("startup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("setup did not run")
	}
	wait, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	value, err := handle.Result(wait)
	cancel()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not stop")
	}
	if err != nil {
		t.Fatal(err)
	}
	if value != 5 {
		t.Fatal(value)
	}
}

func TestWorkflowSetupErrorStopsStartup(t *testing.T) {
	t.Parallel()
	want := errors.New("setup failed")
	d := workflow.Define("bad", "1", func(context.Context, int) (int, error) { t.Error("executed after setup failure"); return 0, nil })
	cfg := resolve(WithWorkflows(d), WithWorkflowSetup(func(context.Context, *workflow.Service) error { return want }))
	if _, _, err := cfg.startWorkflows(context.Background(), runtime.NewMemoryJournal()); !errors.Is(err, want) {
		t.Fatal(err)
	}
}

// Startup failures must not admit or execute workflows. The setup callback
// would start an effect immediately if it ran before channel startup finished.
func TestWorkflowStartupFailureDoesNotRunSetup(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"build", "listen", "start"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			failure := errors.New("channel startup failed")
			var setupCalls, effects atomic.Int32
			d := workflow.Define("startup", "1", func(ctx context.Context, _ int) (int, error) {
				return workflow.Step(ctx, "effect", func(context.Context, workflow.Attempt) (int, error) {
					effects.Add(1)
					return 1, nil
				})
			})
			opts := []Option{WithAddr("127.0.0.1:0"), WithWorkflows(d), WithWorkflowSetup(func(ctx context.Context, s *workflow.Service) error {
				setupCalls.Add(1)
				handle, err := workflow.Start(ctx, s, d, "startup", 0)
				if err != nil {
					return err
				}
				if err := s.Tick(ctx); err != nil {
					return err
				}
				_, err = handle.Result(ctx)
				return err
			})}
			switch stage {
			case "build":
				opts = append(opts, WithChannel(func(*runtime.Runner) (Channel, error) { return nil, failure }))
			case "listen":
				opts = append(opts, WithAddr("invalid address"))
			case "start":
				opts = append(opts, WithChannel(func(*runtime.Runner) (Channel, error) {
					return &lifecycleChannel{name: "failure", start: func(context.Context) error { return failure }, shutdown: func(context.Context) error { return nil }}, nil
				}))
			}
			if err := lifecycleAgent(t, opts...).Run(context.Background()); err == nil {
				t.Fatal("startup failure was lost")
			}
			if setupCalls.Load() != 0 || effects.Load() != 0 {
				t.Fatalf("setup calls=%d effects=%d before successful startup", setupCalls.Load(), effects.Load())
			}
		})
	}
}

// A later nil callback must be rejected before an earlier callback can retain
// the service or start work that would need cleanup.
func TestWorkflowNilSetupRejectedBeforeAnyCallback(t *testing.T) {
	t.Parallel()
	d := workflow.Define("nil-setup", "1", func(context.Context, int) (int, error) { return 0, nil })
	var calls int
	cfg := resolve(WithWorkflows(d), WithWorkflowSetup(func(context.Context, *workflow.Service) error {
		calls++
		return nil
	}), WithWorkflowSetup(nil))
	if _, _, err := cfg.startWorkflows(context.Background(), runtime.NewMemoryJournal()); err == nil {
		t.Fatal("accepted nil setup callback")
	}
	if calls != 0 {
		t.Fatal("ran setup before validating all callbacks")
	}
}

// Setup can use Tick before the normal loop starts. A later setup failure
// must cancel and join those callbacks without closing the host's journal.
func TestWorkflowSetupFailureDrainsActiveCallback(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	stopped := make(chan struct{})
	journal := runtime.NewMemoryJournal()
	d := workflow.Define("setup-drain", "1", func(ctx context.Context, _ int) (int, error) {
		return workflow.Step(ctx, "work", func(ctx context.Context, _ workflow.Attempt) (int, error) {
			close(started)
			<-ctx.Done()
			defer close(stopped)
			_, err := journal.Runs(context.Background(), "")
			return 0, err
		})
	})
	var service *workflow.Service
	failure := errors.New("later setup failed")
	cfg := resolve(WithWorkflows(d), WithWorkflowSetup(func(ctx context.Context, s *workflow.Service) error {
		service = s
		if _, err := workflow.Start(ctx, s, d, "setup-drain", 0); err != nil {
			return err
		}
		if err := s.Tick(ctx); err != nil {
			return err
		}
		select {
		case <-started:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}), WithWorkflowSetup(func(context.Context, *workflow.Service) error { return failure }))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := cfg.startWorkflows(ctx, journal); !errors.Is(err, failure) {
		t.Fatalf("setup error = %v, want %v", err, failure)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("setup failure returned before callback stopped")
	}
	if err := service.Tick(context.Background()); err == nil {
		t.Fatal("service remains open after setup failure")
	}
	if _, err := journal.Runs(context.Background(), ""); err != nil {
		t.Fatalf("host journal was closed: %v", err)
	}
}
