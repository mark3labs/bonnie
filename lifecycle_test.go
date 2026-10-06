package bonnie

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// lifecycleChannel has no HTTP routes. Its callbacks model a transport that
// owns a subscription and must drain it while the journal is still open.
type lifecycleChannel struct {
	name     string
	routes   []channel.Route
	start    func(context.Context) error
	shutdown func(context.Context) error
}

func (c *lifecycleChannel) Name() string                       { return c.name }
func (c *lifecycleChannel) Routes() []channel.Route            { return c.routes }
func (*lifecycleChannel) From(string) channel.SessionRef       { return nil }
func (*lifecycleChannel) Attach(string) channel.SessionRef     { return nil }
func (c *lifecycleChannel) Start(ctx context.Context) error    { return c.start(ctx) }
func (c *lifecycleChannel) Shutdown(ctx context.Context) error { return c.shutdown(ctx) }

func lifecycleAgent(t *testing.T, opts ...Option) *Agent {
	t.Helper()
	return New(append([]Option{
		WithAgentFactory(stubFactory), WithJournal(t.TempDir()),
		WithInstructions(""), WithWorkspace(""), Quiet(),
		WithShutdownTimeout(time.Second),
	}, opts...)...)
}

// Channels start in construction order after bind. A cancelled host context
// must not cancel the drain context, and drain must precede journal close.
func TestChannelLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	var journal runtime.Journal
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var opts []Option
	opts = append(opts, WithListener(ln))
	for _, name := range []string{"first", "second"} {
		opts = append(opts, WithChannel(func(r *runtime.Runner) (Channel, error) {
			journal = r.Journal()
			return &lifecycleChannel{name: name,
				start: func(context.Context) error {
					events = append(events, "start "+name)
					// A second bind cannot succeed while BONNIE owns the listener.
					other, bindErr := net.Listen("tcp", ln.Addr().String())
					if bindErr == nil {
						_ = other.Close()
						t.Error("Start ran without a bound listener")
					}
					if name == "second" {
						cancel()
					}
					return nil
				},
				shutdown: func(drain context.Context) error {
					events = append(events, "shutdown "+name)
					if drain.Err() != nil {
						t.Errorf("drain context is cancelled: %v", drain.Err())
					}
					if _, ok := drain.Deadline(); !ok {
						t.Error("drain has no deadline")
					}
					_, err := journal.Runs(drain, "")
					return err
				},
			}, nil
		}))
	}
	if err := lifecycleAgent(t, opts...).Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"start first", "start second", "shutdown second", "shutdown first"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if _, err := journal.Runs(context.Background(), ""); err == nil {
		t.Error("journal remains open after Run")
	}
	if err := ln.Close(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("listener remains open: %v", err)
	}
}

// Every failure after construction must release channel resources. A failed
// Start and a channel that never started both need Shutdown.
func TestChannelLifecycleFailureCleanup(t *testing.T) {
	t.Parallel()
	failure := errors.New("test failure")
	drainFailure := errors.New("test drain failure")
	for _, stage := range []string{"build", "partial build", "reserved", "listen", "start", "serve"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			var events []string
			var journal runtime.Journal
			first := &lifecycleChannel{name: "first",
				start: func(context.Context) error { events = append(events, "start first"); return nil },
				shutdown: func(ctx context.Context) error {
					events = append(events, "shutdown first")
					if _, err := journal.Runs(ctx, ""); err != nil {
						t.Errorf("journal closed before drain: %v", err)
					}
					return drainFailure
				},
			}
			second := &lifecycleChannel{name: "second",
				start:    func(context.Context) error { events = append(events, "start second"); return failure },
				shutdown: func(context.Context) error { events = append(events, "shutdown second"); return nil },
			}
			opts := []Option{WithAddr("127.0.0.1:0"), WithChannel(func(r *runtime.Runner) (Channel, error) {
				journal = r.Journal()
				return first, nil
			})}
			want := []string{"shutdown first"}
			wantErr := failure
			switch stage {
			case "build":
				opts = append(opts, WithChannel(func(*runtime.Runner) (Channel, error) { return nil, failure }))
			case "partial build":
				opts = append(opts, WithChannel(func(*runtime.Runner) (Channel, error) { return second, failure }))
				want = []string{"shutdown second", "shutdown first"}
			case "reserved":
				first.routes = []channel.Route{{Method: http.MethodGet, Path: "/bonnie/no"}}
				wantErr = channel.ErrReservedPath
			case "listen":
				opts = append(opts, WithAddr("invalid address"))
				wantErr = nil
			case "start":
				opts = append(opts, WithChannel(func(*runtime.Runner) (Channel, error) { return second, nil }))
				want = []string{"start first", "start second", "shutdown second", "shutdown first"}
			case "serve":
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				opts = append(opts, WithListener(&lifecycleFailListener{Listener: ln, err: failure}))
				want = []string{"start first", "shutdown first"}
			}
			err := lifecycleAgent(t, opts...).Run(context.Background())
			if wantErr != nil && !errors.Is(err, wantErr) {
				t.Errorf("Run = %v, want %v", err, wantErr)
			}
			if !errors.Is(err, drainFailure) {
				t.Errorf("Run dropped drain failure: %v", err)
			}
			if !reflect.DeepEqual(events, want) {
				t.Errorf("events = %v, want %v", events, want)
			}
			if _, err := journal.Runs(context.Background(), ""); err == nil {
				t.Error("journal remains open")
			}
		})
	}
}

type lifecycleFailListener struct {
	net.Listener
	err error
}

func (ln *lifecycleFailListener) Accept() (net.Conn, error) { return nil, ln.err }

// A transport that cannot drain must observe a bounded deadline. All channels
// share that deadline, and a timeout must not skip the remaining cleanup.
func TestChannelLifecycleShutdownDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var deadlines []time.Time
	var stops int
	opts := []Option{WithAddr("127.0.0.1:0"), WithShutdownTimeout(20 * time.Millisecond)}
	for _, name := range []string{"first", "second"} {
		opts = append(opts, WithChannel(func(*runtime.Runner) (Channel, error) {
			return &lifecycleChannel{name: name,
				start: func(context.Context) error { cancel(); return nil },
				shutdown: func(ctx context.Context) error {
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Fatal("shutdown has no deadline")
					}
					deadlines = append(deadlines, deadline)
					stops++
					<-ctx.Done()
					return ctx.Err()
				},
			}, nil
		}))
	}
	err := lifecycleAgent(t, opts...).Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want deadline exceeded", err)
	}
	if stops != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("shutdowns = %d, deadlines = %v", stops, deadlines)
	}
}
