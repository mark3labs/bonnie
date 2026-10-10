package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	gonats "github.com/nats-io/nats.go"

	protocol "github.com/mark3labs/bonnie/channel/nats"
)

// Target selects an exact task attempt and its agent.
type Target = protocol.Target

// StatusEvent is a durable acceptance or run-state event, without agent activity.
type StatusEvent = protocol.StatusEvent

// Status is an attempt snapshot or a control reply.
type Status = protocol.Status

// DefaultEventConsumerName returns the stable shared status consumer name.
// Use separate names when each application needs every event.
func DefaultEventConsumerName(subject string) string {
	return subjectName("bonnie-status-reader-", subject)
}

func bindEvents(ctx context.Context, js gonats.JetStreamContext, cfg Config) (time.Duration, error) {
	if cfg.EventSubject == "" {
		return 0, nil
	}
	if err := protocol.EnsureStream(ctx, js, cfg.EventStream, []string{cfg.EventSubject}, cfg.CreateStream); err != nil {
		return 0, err
	}
	ci, err := js.ConsumerInfo(cfg.EventStream, cfg.EventConsumer, gonats.Context(ctx))
	if errors.Is(err, gonats.ErrConsumerNotFound) {
		ci, err = js.AddConsumer(cfg.EventStream, &gonats.ConsumerConfig{Durable: cfg.EventConsumer, FilterSubject: cfg.EventSubject, AckPolicy: gonats.AckExplicitPolicy, AckWait: 30 * time.Second, DeliverPolicy: gonats.DeliverAllPolicy, ReplayPolicy: gonats.ReplayInstantPolicy, MaxAckPending: 1, MaxWaiting: 1024}, gonats.Context(ctx))
		if err != nil {
			ci, err = js.ConsumerInfo(cfg.EventStream, cfg.EventConsumer, gonats.Context(ctx))
		}
	}
	if err != nil {
		return 0, fmt.Errorf("bonnie: client/nats: status consumer: %w", err)
	}
	cc := ci.Config
	if cc.Durable != cfg.EventConsumer || cc.DeliverSubject != "" || cc.DeliverGroup != "" || cc.AckPolicy != gonats.AckExplicitPolicy || cc.FilterSubject != cfg.EventSubject || len(cc.FilterSubjects) != 0 || cc.DeliverPolicy != gonats.DeliverAllPolicy || cc.ReplayPolicy != gonats.ReplayInstantPolicy || cc.AckWait < 3*time.Millisecond || cc.MaxDeliver > 0 || len(cc.BackOff) != 0 || cc.MaxAckPending < 1 || cc.HeadersOnly || cc.InactiveThreshold != 0 || (cc.MaxRequestExpires > 0 && cc.MaxRequestExpires < 250*time.Millisecond) || (cc.MaxRequestMaxBytes > 0 && cc.MaxRequestMaxBytes < maxMessageBytes) {
		return 0, errors.New("bonnie: client/nats: incompatible status consumer")
	}
	return cc.AckWait, nil
}

// ConsumeEvents reads durable status events and acknowledges after handler
// success. Events can repeat. No text deltas or tool activity are delivered.
func (c *Client) ConsumeEvents(ctx context.Context, handler func(context.Context, StatusEvent) error) error {
	if handler == nil || c.cfg.EventSubject == "" {
		return errors.New("bonnie: client/nats: status subject and handler are required")
	}
	return c.consume(ctx, c.cfg.EventSubject, c.cfg.EventStream, c.cfg.EventConsumer, c.eventAckWait, func(data []byte) error {
		var ev StatusEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("bonnie: client/nats: decode status: %w", err)
		}
		if ev.Version != 1 || ev.EventID == "" || !validTarget(ev.Target) || (ev.Type != "task_accepted" && ev.Type != "run_state") {
			return errors.New("bonnie: client/nats: invalid status event")
		}
		return handler(ctx, ev)
	})
}

// Status queries the owning agent. A timeout means the agent did not reply,
// not that the task failed. The target is obtained from an event or result.
func (c *Client) Status(ctx context.Context, target Target) (Status, error) {
	return c.requestStatus(ctx, c.cfg.QuerySubject, target)
}

// Cancel requests cancellation of an active turn. The reply confirms only the
// request; the cancelled state is reported separately. Idle runs are no-ops.
// Use CancelTurn to protect a later turn from a delayed request.
func (c *Client) Cancel(ctx context.Context, target Target) (Status, error) {
	return c.requestStatus(ctx, c.cfg.CommandSubject, target)
}

func validTarget(t Target) bool {
	return validID(t.TaskID) && validID(t.RunID) && validID(t.AttemptID) && safeToken(t.AgentID)
}

// CancelTurn requests cancellation of one observed turn, including pending input.
func (c *Client) CancelTurn(ctx context.Context, target Target, turnID string) (Status, error) {
	return c.requestTurnStatus(ctx, c.cfg.CommandSubject, target, turnID)
}

func (c *Client) requestStatus(ctx context.Context, base string, target Target) (Status, error) {
	return c.requestTurnStatus(ctx, base, target, "")
}

func (c *Client) requestTurnStatus(ctx context.Context, base string, target Target, turnID string) (Status, error) {
	if base == "" || !validTarget(target) {
		return Status{}, errors.New("bonnie: client/nats: control subject and exact target are required")
	}
	data, err := json.Marshal(protocol.StatusRequest{Version: 1, Target: target, TurnID: turnID})
	if err != nil {
		return Status{}, fmt.Errorf("bonnie: client/nats: encode request: %w", err)
	}
	msg, err := c.nc.RequestWithContext(ctx, base+"."+target.AgentID, data)
	if err != nil {
		return Status{}, fmt.Errorf("bonnie: client/nats: request: %w", err)
	}
	var out Status
	if len(msg.Data) > maxMessageBytes {
		return out, errors.New("bonnie: client/nats: status exceeds limit")
	}
	if err := json.Unmarshal(msg.Data, &out); err != nil {
		return out, fmt.Errorf("bonnie: client/nats: decode reply: %w", err)
	}
	if out.Version != 1 || (out.Error == "" && out.Target != target) {
		return out, errors.New("bonnie: client/nats: invalid status reply")
	}
	return out, nil
}
