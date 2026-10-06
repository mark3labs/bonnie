// Package nats provides a typed JetStream client for channel/nats.
//
// Delivery is at least once. Results can repeat, and separate workers can run
// the same task again. Handlers must tolerate duplicates. This client does not
// execute agents or keep run state. The caller owns the NATS connection.
//
// The raw protocol is JSON: version 1 Task on TaskSubject, Result on
// ResultSubject, and Answer on AnswerSubject+"."+WorkerID. A broker publish
// acknowledgement confirms storage, not execution. Nats-Msg-Id suppresses
// repeats only within the stream's duplicate window. Use NATS permissions to
// restrict access; result content is not proof of worker identity.
package nats

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	gonats "github.com/nats-io/nats.go"

	protocol "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/runtime"
)

// Task is the channel wire task. Its identity field is TaskID, not ID.
type Task = protocol.Task

// Result is the channel wire result.
type Result = protocol.Result

// Answer is the channel wire answer.
type Answer = protocol.Answer

// Outcome is a result delivered to a handler, including rejected inputs.
type Outcome = Result

// Config selects literal subjects and the durable result consumer.
type Config struct {
	TaskSubject   string
	ResultSubject string
	// AnswerSubject is a base. Answers go only to base+"."+WorkerID.
	AnswerSubject string
	// ResultStream defaults to DefaultResultStreamName(ResultSubject).
	// Set it to use an operator-provisioned stream with a different name.
	ResultStream string
	// ResultConsumer defaults to DefaultResultConsumerName(ResultSubject).
	// Clients using the same stream and consumer share result processing.
	// Set separate consumers when applications must each receive every result.
	ResultConsumer string
	// CreateStream permits creation of the result stream only. The operator
	// must provision the input stream for tasks and AnswerSubject+".*".
	// Existing streams and consumers are never changed.
	CreateStream bool
}

// Receipt confirms that the broker stored an input, or detected a duplicate.
type Receipt struct {
	TaskID    string
	Stream    string
	Sequence  uint64
	Duplicate bool
}

// Client publishes inputs and reads durable results. It owns no connection.
type Client struct {
	js      gonats.JetStreamContext
	cfg     Config
	ackWait time.Duration
}

const maxMessageBytes = 1 << 20

// New validates configuration and binds to JetStream. It creates a durable
// pull consumer if absent, but creates a result stream only with CreateStream.
// Existing resources must retain all results and use explicit acknowledgements.
func New(nc *gonats.Conn, cfg Config) (*Client, error) {
	if nc == nil {
		return nil, errors.New("bonnie: client/nats: connection is required")
	}
	for _, s := range []string{cfg.TaskSubject, cfg.ResultSubject, cfg.AnswerSubject} {
		if !literalSubject(s) {
			return nil, errors.New("bonnie: client/nats: subjects must be literal")
		}
	}
	if cfg.TaskSubject == cfg.ResultSubject || cfg.TaskSubject == cfg.AnswerSubject || cfg.ResultSubject == cfg.AnswerSubject || strings.HasPrefix(cfg.TaskSubject, cfg.AnswerSubject+".") || strings.HasPrefix(cfg.ResultSubject, cfg.AnswerSubject+".") {
		return nil, errors.New("bonnie: client/nats: subjects overlap")
	}
	if cfg.ResultStream == "" {
		cfg.ResultStream = DefaultResultStreamName(cfg.ResultSubject)
	}
	if cfg.ResultConsumer == "" {
		cfg.ResultConsumer = DefaultResultConsumerName(cfg.ResultSubject)
	}
	if !safeToken(cfg.ResultStream) || !safeToken(cfg.ResultConsumer) {
		return nil, errors.New("bonnie: client/nats: invalid stream or consumer name")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("bonnie: client/nats: JetStream: %w", err)
	}
	si, err := js.StreamInfo(cfg.ResultStream, gonats.Context(ctx))
	if errors.Is(err, gonats.ErrStreamNotFound) && cfg.CreateStream {
		si, err = js.AddStream(&gonats.StreamConfig{Name: cfg.ResultStream, Subjects: []string{cfg.ResultSubject}, Storage: gonats.FileStorage}, gonats.Context(ctx))
		if err != nil {
			si, err = js.StreamInfo(cfg.ResultStream, gonats.Context(ctx))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("bonnie: client/nats: result stream: %w", err)
	}
	if !slices.Contains(si.Config.Subjects, cfg.ResultSubject) || si.Config.Retention != gonats.LimitsPolicy {
		return nil, errors.New("bonnie: client/nats: result stream must retain the literal result subject with limits retention")
	}
	ci, err := js.ConsumerInfo(cfg.ResultStream, cfg.ResultConsumer, gonats.Context(ctx))
	if errors.Is(err, gonats.ErrConsumerNotFound) {
		ci, err = js.AddConsumer(cfg.ResultStream, &gonats.ConsumerConfig{Durable: cfg.ResultConsumer, FilterSubject: cfg.ResultSubject, AckPolicy: gonats.AckExplicitPolicy, AckWait: 30 * time.Second, DeliverPolicy: gonats.DeliverAllPolicy, ReplayPolicy: gonats.ReplayInstantPolicy, MaxAckPending: 1, MaxWaiting: 1024}, gonats.Context(ctx))
		if err != nil {
			ci, err = js.ConsumerInfo(cfg.ResultStream, cfg.ResultConsumer, gonats.Context(ctx))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("bonnie: client/nats: result consumer: %w", err)
	}
	cc := ci.Config
	if cc.Durable != cfg.ResultConsumer || cc.DeliverSubject != "" || cc.DeliverGroup != "" || cc.AckPolicy != gonats.AckExplicitPolicy || cc.FilterSubject != cfg.ResultSubject || len(cc.FilterSubjects) != 0 || cc.DeliverPolicy != gonats.DeliverAllPolicy || cc.ReplayPolicy != gonats.ReplayInstantPolicy || cc.AckWait < 3*time.Millisecond || cc.MaxDeliver > 0 || len(cc.BackOff) != 0 || cc.MaxAckPending < 1 || cc.HeadersOnly || cc.InactiveThreshold != 0 || (cc.MaxRequestExpires > 0 && cc.MaxRequestExpires < 250*time.Millisecond) || (cc.MaxRequestMaxBytes > 0 && cc.MaxRequestMaxBytes < maxMessageBytes) {
		return nil, errors.New("bonnie: client/nats: incompatible result pull consumer")
	}
	return &Client{js: js, cfg: cfg, ackWait: cc.AckWait}, nil
}

// DefaultResultStreamName returns a stable stream name for the exact result
// subject: "bonnie-results-" followed by its full SHA-256 digest. It does not
// validate the subject or grant permission to create a stream.
func DefaultResultStreamName(subject string) string {
	return subjectName("bonnie-results-", subject)
}

// DefaultResultConsumerName returns a stable durable consumer name for the
// exact result subject: "bonnie-reader-" followed by its full SHA-256 digest.
// Clients with this name on the same stream share work and acknowledgement
// state. Use an explicit ResultConsumer for independent result readers.
func DefaultResultConsumerName(subject string) string {
	return subjectName("bonnie-reader-", subject)
}

func subjectName(prefix, subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return prefix + hex.EncodeToString(sum[:])
}

func literalSubject(s string) bool {
	return s != "" && !strings.ContainsAny(s, "*>\\") && strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0 && !slices.Contains(strings.Split(s, "."), "")
}
func safeToken(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
func validID(s string) bool {
	return s != "" && len(s) <= 256 && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0 && !strings.HasPrefix(s, "nats/")
}
func messageID(kind string, parts ...string) string {
	// JSON preserves boundaries, so different identity tuples cannot collide
	// through concatenation. Hashing keeps arbitrary IDs out of headers.
	data, _ := json.Marshal(parts) // A slice of strings always encodes.
	sum := sha256.Sum256(data)
	return "bonnie-client-" + kind + "-" + hex.EncodeToString(sum[:])
}

// Submit stores a version 1 task. Version 0 defaults to 1; other versions are
// rejected. Reuse TaskID on the same TaskSubject only for retries of the same
// task. A receipt does not mean that an agent has started or completed work.
func (c *Client) Submit(ctx context.Context, task Task) (Receipt, error) {
	if task.Version == 0 {
		task.Version = 1
	}
	if task.Version != 1 || !validID(task.TaskID) || strings.TrimSpace(task.Text) == "" {
		return Receipt{}, errors.New("bonnie: client/nats: invalid version 1 task")
	}
	return c.publish(ctx, c.cfg.TaskSubject, task.TaskID, messageID("task", c.cfg.TaskSubject, task.TaskID), task)
}

// Answer stores responses for the exact waiting run and tool call. Retries of
// that suspension on the same answer route use the same message identity,
// even if responses change.
// The outcome route must equal the configured base plus a safe worker token.
func (c *Client) Answer(ctx context.Context, outcome Outcome, responses []runtime.InputResponse) (Receipt, error) {
	if outcome.Version != 1 || !validID(outcome.TaskID) || !validID(outcome.RunID) || !safeToken(outcome.WorkerID) || outcome.State != runtime.RunWaiting || outcome.Suspend == nil || outcome.Suspend.ToolCallID == "" || len(responses) == 0 || outcome.Error != "" {
		return Receipt{}, errors.New("bonnie: client/nats: invalid waiting outcome")
	}
	route := c.cfg.AnswerSubject + "." + outcome.WorkerID
	if outcome.AnswerSubject != route {
		return Receipt{}, errors.New("bonnie: client/nats: invalid answer route")
	}
	id := messageID("answer", route, outcome.RunID, outcome.Suspend.ToolCallID)
	answer := Answer{Version: 1, MessageID: id, TaskID: outcome.TaskID, RunID: outcome.RunID, WorkerID: outcome.WorkerID, ToolCallID: outcome.Suspend.ToolCallID, Responses: responses}
	return c.publish(ctx, route, outcome.TaskID, id, answer)
}

func (c *Client) publish(ctx context.Context, subject, taskID, id string, value any) (Receipt, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Receipt{}, fmt.Errorf("bonnie: client/nats: encode input: %w", err)
	}
	if len(data) > maxMessageBytes {
		return Receipt{}, errors.New("bonnie: client/nats: input exceeds message limit")
	}
	msg := gonats.NewMsg(subject)
	msg.Data = data
	msg.Header.Set(gonats.MsgIdHdr, id)
	ack, err := c.js.PublishMsg(msg, gonats.Context(ctx))
	if err != nil {
		return Receipt{}, fmt.Errorf("bonnie: client/nats: store input: %w", err)
	}
	return Receipt{TaskID: taskID, Stream: ack.Stream, Sequence: ack.Sequence, Duplicate: ack.Duplicate}, nil
}

// Consume pulls results until cancellation or failure. It acknowledges only
// after handler success. While the handler runs, InProgress extends AckWait.
// A handler or decode failure sends a delayed negative acknowledgement and
// returns the error. Call Consume again to retry; results are not discarded.
// Cancellation returns the context error. Handler side effects must be safe
// to repeat, including when the broker acknowledgement fails.
func (c *Client) Consume(ctx context.Context, handler func(context.Context, Outcome) error) (err error) {
	if handler == nil {
		return errors.New("bonnie: client/nats: handler is required")
	}
	sub, err := c.js.PullSubscribe(c.cfg.ResultSubject, c.cfg.ResultConsumer, gonats.Bind(c.cfg.ResultStream, c.cfg.ResultConsumer))
	if err != nil {
		return fmt.Errorf("bonnie: client/nats: bind results: %w", err)
	}
	defer func() {
		if closeErr := sub.Unsubscribe(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("bonnie: client/nats: unsubscribe: %w", closeErr))
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		msgs, err := sub.Fetch(1, gonats.Context(fetchCtx))
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, gonats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return fmt.Errorf("bonnie: client/nats: fetch results: %w", err)
		}
		for _, msg := range msgs {
			if err := c.handle(ctx, msg, handler); err != nil {
				return err
			}
		}
	}
}

func (c *Client) handle(ctx context.Context, msg *gonats.Msg, handler func(context.Context, Outcome) error) error {
	done := make(chan struct{})
	heartbeat := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(c.ackWait / 3)
		defer ticker.Stop()
		var err error
		defer func() { heartbeat <- err }()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e := msg.InProgress(); e != nil {
					err = fmt.Errorf("bonnie: client/nats: heartbeat: %w", e)
					return
				}
			}
		}
	}()
	var outcome Outcome
	var err error
	if len(msg.Data) > maxMessageBytes {
		err = errors.New("bonnie: client/nats: result exceeds message limit")
	} else if e := json.Unmarshal(msg.Data, &outcome); e != nil {
		err = fmt.Errorf("bonnie: client/nats: decode result: %w", e)
	} else if outcome.Version != 1 {
		err = errors.New("bonnie: client/nats: invalid result version")
	} else {
		err = handler(ctx, outcome)
	}
	close(done)
	err = errors.Join(err, <-heartbeat, ctx.Err())
	if err != nil {
		if e := msg.NakWithDelay(time.Second); e != nil {
			err = errors.Join(err, fmt.Errorf("bonnie: client/nats: retry result: %w", e))
		}
		return err
	}
	if err := msg.AckSync(gonats.Context(ctx)); err != nil {
		return fmt.Errorf("bonnie: client/nats: acknowledge result: %w", err)
	}
	return nil
}
