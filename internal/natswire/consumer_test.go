package natswire

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
)

func TestCompatiblePullConsumer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*gonats.ConsumerConfig)
		want   bool
	}{
		{"compatible", func(*gonats.ConsumerConfig) {}, true},
		{"durable", func(c *gonats.ConsumerConfig) { c.Durable = "other" }, false},
		{"deliver subject", func(c *gonats.ConsumerConfig) { c.DeliverSubject = "push" }, false},
		{"deliver group", func(c *gonats.ConsumerConfig) { c.DeliverGroup = "group" }, false},
		{"ack policy", func(c *gonats.ConsumerConfig) { c.AckPolicy = gonats.AckNonePolicy }, false},
		{"filter subject", func(c *gonats.ConsumerConfig) { c.FilterSubject = "other" }, false},
		{"filter subjects", func(c *gonats.ConsumerConfig) { c.FilterSubjects = []string{"tasks"} }, false},
		{"deliver policy", func(c *gonats.ConsumerConfig) { c.DeliverPolicy = gonats.DeliverNewPolicy }, false},
		{"replay policy", func(c *gonats.ConsumerConfig) { c.ReplayPolicy = gonats.ReplayOriginalPolicy }, false},
		{"ack wait", func(c *gonats.ConsumerConfig) { c.AckWait = 3*time.Millisecond - 1 }, false},
		{"max deliver", func(c *gonats.ConsumerConfig) { c.MaxDeliver = 1 }, false},
		{"backoff", func(c *gonats.ConsumerConfig) { c.BackOff = []time.Duration{time.Second} }, false},
		{"max ack pending", func(c *gonats.ConsumerConfig) { c.MaxAckPending = 0 }, false},
		{"headers only", func(c *gonats.ConsumerConfig) { c.HeadersOnly = true }, false},
		{"inactive threshold", func(c *gonats.ConsumerConfig) { c.InactiveThreshold = time.Second }, false},
		{"request expires", func(c *gonats.ConsumerConfig) { c.MaxRequestExpires = 250*time.Millisecond - 1 }, false},
		{"request bytes", func(c *gonats.ConsumerConfig) { c.MaxRequestMaxBytes = MaxMessageBytes - 1 }, false},
		{"exact limits", func(c *gonats.ConsumerConfig) {
			c.AckWait = 3 * time.Millisecond
			c.MaxRequestExpires = 250 * time.Millisecond
			c.MaxRequestMaxBytes = MaxMessageBytes
		}, true},
		{"unlimited delivery", func(c *gonats.ConsumerConfig) { c.MaxDeliver = -1 }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cc := gonats.ConsumerConfig{
				Durable: "worker", FilterSubject: "tasks", AckPolicy: gonats.AckExplicitPolicy,
				AckWait: 30 * time.Second, DeliverPolicy: gonats.DeliverAllPolicy,
				ReplayPolicy: gonats.ReplayInstantPolicy, MaxAckPending: 1,
			}
			tt.change(&cc)
			if got := CompatiblePullConsumer(cc, "worker", "tasks"); got != tt.want {
				t.Fatalf("CompatiblePullConsumer() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Embed the full interface so unexpected calls fail; only these two methods
// belong to the find-or-create sequence.
type consumerJS struct {
	gonats.JetStreamContext
	info func(string, string, ...gonats.JSOpt) (*gonats.ConsumerInfo, error)
	add  func(string, *gonats.ConsumerConfig, ...gonats.JSOpt) (*gonats.ConsumerInfo, error)
}

func (j consumerJS) ConsumerInfo(stream, name string, opts ...gonats.JSOpt) (*gonats.ConsumerInfo, error) {
	return j.info(stream, name, opts...)
}

func (j consumerJS) AddConsumer(stream string, cfg *gonats.ConsumerConfig, opts ...gonats.JSOpt) (*gonats.ConsumerInfo, error) {
	return j.add(stream, cfg, opts...)
}

func TestEnsurePullConsumer(t *testing.T) {
	t.Parallel()
	failure := errors.New("lookup failed")
	for _, tt := range []struct {
		name                           string
		lookupErr, createErr, retryErr error
		calls                          []string
		wantErr                        error
	}{
		{"existing", nil, nil, nil, []string{"info"}, nil},
		{"create", gonats.ErrConsumerNotFound, nil, nil, []string{"info", "add"}, nil},
		{"concurrent creator", fmt.Errorf("lookup: %w", gonats.ErrConsumerNotFound), failure, nil, []string{"info", "add", "info"}, nil},
		{"lookup error", failure, nil, nil, []string{"info"}, failure},
		{"retry error", gonats.ErrConsumerNotFound, failure, context.DeadlineExceeded, []string{"info", "add", "info"}, context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			want := &gonats.ConsumerConfig{Durable: "worker", AckWait: 42 * time.Second, MaxAckPending: 7}
			result := &gonats.ConsumerInfo{Config: *want}
			var calls []string
			checkOpts := func(opts []gonats.JSOpt) {
				t.Helper()
				if len(opts) != 1 || opts[0] != gonats.Context(ctx) {
					t.Fatalf("context option = %v, want caller context", opts)
				}
			}
			js := consumerJS{
				info: func(stream, name string, opts ...gonats.JSOpt) (*gonats.ConsumerInfo, error) {
					checkOpts(opts)
					if stream != "stream" || name != want.Durable {
						t.Fatalf("lookup = %q, %q", stream, name)
					}
					calls = append(calls, "info")
					err := tt.lookupErr
					if len(calls) > 1 {
						err = tt.retryErr
					}
					if err != nil {
						return nil, err
					}
					return result, nil
				},
				add: func(stream string, cfg *gonats.ConsumerConfig, opts ...gonats.JSOpt) (*gonats.ConsumerInfo, error) {
					checkOpts(opts)
					if stream != "stream" || cfg != want {
						t.Fatal("create defaults were changed")
					}
					calls = append(calls, "add")
					if tt.createErr != nil {
						return nil, tt.createErr
					}
					return result, nil
				},
			}
			got, err := EnsurePullConsumer(ctx, js, "stream", want)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if err == nil && got != result {
				t.Fatal("consumer info was changed")
			}
			if !reflect.DeepEqual(calls, tt.calls) {
				t.Fatalf("calls = %v, want %v", calls, tt.calls)
			}
		})
	}
}
