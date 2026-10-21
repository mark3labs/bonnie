package natswire

import (
	"context"
	"errors"
	"time"

	gonats "github.com/nats-io/nats.go"
)

// MaxMessageBytes bounds one BONNIE NATS message. The server and the client
// must agree on it: a pull consumer that caps requests below it is refused.
const MaxMessageBytes = 1 << 20

// CompatiblePullConsumer reports whether an existing durable pull consumer
// matches what BONNIE needs: explicit ack, one literal filter subject,
// deliver-all, instant replay, no redelivery cap or backoff, and request
// limits that admit one full message.
func CompatiblePullConsumer(cc gonats.ConsumerConfig, durable, subject string) bool {
	return cc.Durable == durable && cc.DeliverSubject == "" && cc.DeliverGroup == "" &&
		cc.AckPolicy == gonats.AckExplicitPolicy && cc.FilterSubject == subject && len(cc.FilterSubjects) == 0 &&
		cc.DeliverPolicy == gonats.DeliverAllPolicy && cc.ReplayPolicy == gonats.ReplayInstantPolicy &&
		cc.AckWait >= 3*time.Millisecond && cc.MaxDeliver <= 0 && len(cc.BackOff) == 0 &&
		cc.MaxAckPending >= 1 && !cc.HeadersOnly && cc.InactiveThreshold == 0 &&
		(cc.MaxRequestExpires <= 0 || cc.MaxRequestExpires >= 250*time.Millisecond) &&
		(cc.MaxRequestMaxBytes <= 0 || cc.MaxRequestMaxBytes >= MaxMessageBytes)
}

// EnsurePullConsumer returns the consumer named want.Durable on stream. When
// it does not exist, it is created from want. If creation fails, the lookup
// is repeated in case a concurrent creator won the race. The caller checks
// CompatiblePullConsumer and wraps errors with its own context.
func EnsurePullConsumer(ctx context.Context, js gonats.JetStreamContext, stream string, want *gonats.ConsumerConfig) (*gonats.ConsumerInfo, error) {
	ci, err := js.ConsumerInfo(stream, want.Durable, gonats.Context(ctx))
	if errors.Is(err, gonats.ErrConsumerNotFound) {
		ci, err = js.AddConsumer(stream, want, gonats.Context(ctx))
		if err != nil {
			ci, err = js.ConsumerInfo(stream, want.Durable, gonats.Context(ctx))
		}
	}
	return ci, err
}
