package nats

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

const defaultAckWait = 30 * time.Second

func safeToken(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, ch := range s {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' {
			return false
		}
	}
	return true
}

func validateJetStream(cfg Config) error {
	if cfg.Stream == "" {
		if cfg.Consumer != "" || cfg.WorkerID != "" || cfg.CreateStream {
			return errors.New("bonnie: channel/nats: Stream is required for JetStream options")
		}
		return nil
	}
	if !safeToken(cfg.Stream) || !safeToken(cfg.Consumer) || !safeToken(cfg.WorkerID) {
		return errors.New("bonnie: channel/nats: stream, consumer, and worker must be safe tokens (letters, digits, hyphen)")
	}
	if strings.HasPrefix(cfg.Subject, cfg.AnswerSubject+".") || strings.HasPrefix(cfg.ResultSubject, cfg.AnswerSubject+".") {
		return errors.New("bonnie: channel/nats: answer routes overlap task or result subject")
	}
	return nil
}

func (c *Channel) answerRoute() string { return c.cfg.AnswerSubject + "." + c.cfg.WorkerID }

// startJetStream is called with the lifecycle lock held. Explicit Bind prevents
// the NATS library from changing or deleting an operator-owned consumer.
func (c *Channel) startJetStream(ctx context.Context, nc *gonats.Conn) error {
	ready, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("bonnie: channel/nats: JetStream: %w", err)
	}
	if c.cfg.RootSubject != "" {
		if err := EnsureStream(ready, js, c.cfg.ResultStream, []string{c.cfg.ResultSubject}, c.cfg.CreateStream); err != nil {
			return err
		}
	}
	if c.cfg.EventSubject != "" {
		if err := EnsureStream(ready, js, c.cfg.EventStream, []string{c.cfg.EventSubject}, c.cfg.CreateStream); err != nil {
			return err
		}
	}
	info, err := js.StreamInfo(c.cfg.Stream, gonats.Context(ready))
	if errors.Is(err, gonats.ErrStreamNotFound) && c.cfg.CreateStream {
		info, err = js.AddStream(&gonats.StreamConfig{Name: c.cfg.Stream, Subjects: []string{c.cfg.Subject, c.cfg.AnswerSubject + ".*"}, Storage: gonats.FileStorage}, gonats.Context(ready))
		// Another worker can create the stream at the same time.
		if err != nil {
			info, err = js.StreamInfo(c.cfg.Stream, gonats.Context(ready))
		}
	}
	if err != nil {
		return fmt.Errorf("bonnie: channel/nats: input stream: %w", err)
	}
	if !slices.Contains(info.Config.Subjects, c.cfg.Subject) || !slices.Contains(info.Config.Subjects, c.cfg.AnswerSubject+".*") || info.Config.Retention != gonats.LimitsPolicy {
		return errors.New("bonnie: channel/nats: input stream must retain tasks and worker answer routes with limits retention")
	}
	if _, err := c.resultLimit(ready, js, nc); err != nil {
		return err
	}
	var ackWait time.Duration
	for i, subject := range []string{c.cfg.Subject, c.answerRoute()} {
		name := c.cfg.Consumer
		if i == 1 {
			name += "_" + c.cfg.WorkerID
		}
		ci, err := js.ConsumerInfo(c.cfg.Stream, name, gonats.Context(ready))
		if errors.Is(err, gonats.ErrConsumerNotFound) {
			ci, err = js.AddConsumer(c.cfg.Stream, &gonats.ConsumerConfig{Durable: name, AckPolicy: gonats.AckExplicitPolicy, AckWait: defaultAckWait, DeliverPolicy: gonats.DeliverAllPolicy, ReplayPolicy: gonats.ReplayInstantPolicy, FilterSubject: subject, MaxAckPending: c.cfg.Concurrency, MaxWaiting: 1024}, gonats.Context(ready))
			if err != nil {
				ci, err = js.ConsumerInfo(c.cfg.Stream, name, gonats.Context(ready))
			}
		}
		if err != nil {
			return fmt.Errorf("bonnie: channel/nats: consumer: %w", err)
		}
		cc := ci.Config
		if cc.Durable != name || cc.DeliverSubject != "" || cc.AckPolicy != gonats.AckExplicitPolicy || cc.FilterSubject != subject || len(cc.FilterSubjects) != 0 || cc.DeliverPolicy != gonats.DeliverAllPolicy || cc.ReplayPolicy != gonats.ReplayInstantPolicy || cc.AckWait < 3*time.Millisecond || cc.MaxDeliver > 0 || len(cc.BackOff) != 0 || cc.MaxAckPending < 1 || cc.HeadersOnly || cc.InactiveThreshold != 0 || cc.DeliverGroup != "" || (cc.MaxRequestExpires > 0 && cc.MaxRequestExpires < 250*time.Millisecond) || (cc.MaxRequestMaxBytes > 0 && cc.MaxRequestMaxBytes < maxMessageBytes) {
			return errors.New("bonnie: channel/nats: incompatible existing pull consumer")
		}
		if ackWait == 0 || cc.AckWait < ackWait {
			ackWait = cc.AckWait
		}
		sub, err := js.PullSubscribe(subject, name, gonats.Bind(c.cfg.Stream, name))
		if err != nil {
			return fmt.Errorf("bonnie: channel/nats: bind consumer: %w", err)
		}
		c.subs = append(c.subs, sub)
	}
	workCtx, stop := context.WithCancel(ctx)
	c.js, c.conn = js, nc
	if err := c.startStatusControls(workCtx, nc); err != nil {
		stop()
		return err
	}
	c.started, c.cancel, c.done, c.ackWait = true, stop, make(chan struct{}), ackWait
	// Each worker requests one message only when it has a free execution slot.
	// Alternate consumers so waiting answers cannot be starved by a task backlog.
	var wg sync.WaitGroup
	if c.cfg.EventSubject != "" {
		wg.Go(func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				if err := c.flushStatuses(workCtx); err != nil && workCtx.Err() == nil {
					c.report("status publication failed")
				}
				select {
				case <-workCtx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	}
	for worker := range c.cfg.Concurrency {
		wg.Go(func() {
			index := worker % 2
			for workCtx.Err() == nil {
				fetchCtx, cancel := context.WithTimeout(workCtx, 250*time.Millisecond)
				msgs, err := c.subs[index].Fetch(1, gonats.Context(fetchCtx))
				cancel()
				index = 1 - index
				if err != nil {
					if workCtx.Err() != nil {
						return
					}
					if errors.Is(err, gonats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
						continue
					}
					c.report("JetStream fetch failed")
					stop()
					return
				}
				for _, msg := range msgs {
					c.handleJetStream(workCtx, msg)
				}
			}
		})
	}
	go func() {
		<-workCtx.Done()
		wg.Wait()
		for _, sub := range c.subs {
			if err := sub.Unsubscribe(); err != nil {
				c.report("unsubscribe failed")
			}
		}
		if c.cfg.Conn == nil {
			nc.Close()
		}
		close(c.done)
	}()
	return nil
}

func (c *Channel) cacheKey(kind, id string) string {
	return runtime.ReservedRunPrefix + "nats." + c.cfg.Stream + "." + c.cfg.Consumer + "." + c.cfg.WorkerID + "." + kind + "." + hex.EncodeToString([]byte(id))
}

// admittedAnswer records the accepted input before Resume can change the run.
// Keep the full answer and suspension: a redelivery must not use changed input.
type admittedAnswer struct {
	Answer  Answer                  `json:"answer"`
	Suspend *runtime.SuspendRequest `json:"suspend"`
	Result  Result                  `json:"result"`
}

func (c *Channel) loadAdmission(ctx context.Context, key string) (*admittedAnswer, error) {
	records, err := c.core.Runner().Journal().Replay(ctx, key)
	if errors.Is(err, runtime.ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.ExtType == "nats_answer_admitted" {
			var admission admittedAnswer
			if err := json.Unmarshal(record.Payload, &admission); err != nil {
				return nil, err
			}
			return &admission, nil
		}
	}
	return nil, nil
}

// resultLimit bounds encoded JSON plus the publication header, not response
// text alone. A missing output stream can be added later; publication still
// fails until then.
func (c *Channel) resultLimit(ctx context.Context, js gonats.JetStreamContext, nc *gonats.Conn) (int, error) {
	limit := nc.MaxPayload()
	name, err := js.StreamNameBySubject(c.cfg.ResultSubject, gonats.Context(ctx))
	if err == nil {
		info, infoErr := js.StreamInfo(name, gonats.Context(ctx))
		if infoErr != nil {
			return 0, infoErr
		}
		if info.Config.MaxMsgSize > 0 {
			limit = min(limit, int64(info.Config.MaxMsgSize))
		}
	} else if !errors.Is(err, gonats.ErrStreamNotFound) && !errors.Is(err, gonats.ErrNoMatchingStream) {
		return 0, err
	}
	// NATS counts headers in broker and stream message limits. The cache-key
	// message ID contains three names of at most 128 bytes plus a sequence
	// number. Reserve 512 bytes for that header and NATS framing. The client
	// JSON limit remains maxMessageBytes, independently of broker headers.
	limit = min(int64(maxMessageBytes), limit-512)
	// Reserve room for maximum-length identities, including JSON escaping.
	metadata := Result{Version: 1, TaskID: strings.Repeat("<", 256), RunID: strings.Repeat("<", 256), AttemptID: strings.Repeat("a", 32), WorkerID: c.cfg.WorkerID, AnswerSubject: c.answerRoute(), State: runtime.RunFailed, Error: "result too large"}
	data, err := json.Marshal(metadata)
	if err != nil {
		return 0, err
	}
	if limit < int64(len(data)) {
		return 0, errors.New("bonnie: channel/nats: result payload limit too small for metadata")
	}
	return int(limit), nil
}

func (c *Channel) boundResult(ctx context.Context, result Result) (Result, error) {
	limit, err := c.resultLimit(ctx, c.js, c.conn)
	if err != nil {
		return result, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if len(data) > limit {
		// This is a transport failure, not a change to the journalled run state.
		// Never truncate a suspension: its questions are needed to resume safely.
		result.Response, result.Suspend = "", nil
		result.State, result.Error = runtime.RunFailed, "result too large"
		if !validID(result.TaskID) {
			result.TaskID = ""
		}
		data, err = json.Marshal(result)
		if err != nil {
			return result, err
		}
		if len(data) > limit {
			return result, errors.New("bonnie: channel/nats: result metadata exceeds payload limit")
		}
	}
	return result, nil
}

func (c *Channel) loadResult(ctx context.Context, key string) (*Result, error) {
	records, err := c.core.Runner().Journal().Replay(ctx, key)
	if errors.Is(err, runtime.ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, record := range slices.Backward(records) {
		if record.ExtType == "nats_result" {
			var result Result
			if err := json.Unmarshal(record.Payload, &result); err != nil {
				return nil, err
			}
			return &result, nil
		}
	}
	return nil, nil
}

func (c *Channel) saveResult(ctx context.Context, key string, result Result) error {
	result, err := c.boundResult(ctx, result)
	if err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = c.core.Runner().Journal().Append(ctx, runtime.Record{RunID: key, Kind: runtime.RecordExtensionData, ExtType: "nats_result", Payload: data})
	return err
}

func (c *Channel) handleJetStream(ctx context.Context, msg *gonats.Msg) {
	// Heartbeats also cover journal access, lock waits, and result publication.
	finished := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		ticker := time.NewTicker(c.ackWait / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-finished:
				return
			case <-ticker.C:
				if err := msg.InProgress(); err != nil && ctx.Err() == nil {
					c.report("JetStream heartbeat failed")
				}
			}
		}
	}()
	defer func() { close(finished); <-watched }()
	meta, err := msg.Metadata()
	if err != nil {
		c.report("JetStream metadata failed")
		return
	}
	key := c.cacheKey("input", fmt.Sprint(meta.Sequence.Stream))
	// Serialize same-run answers and redelivered input within this process.
	var answer Answer
	var task Task
	isAnswer := msg.Subject == c.answerRoute()
	if isAnswer {
		err = decode(msg.Data, &answer)
		task.TaskID = answer.TaskID
	} else {
		err = decode(msg.Data, &task)
	}
	identity := key
	if isAnswer {
		identity = answer.RunID
	}
	var hash uint32
	for _, b := range []byte(identity) {
		hash = hash*31 + uint32(b)
	}
	lock := &c.locks[hash%uint32(len(c.locks))]
	lock.Lock()
	defer lock.Unlock()
	cached, loadErr := c.loadResult(ctx, key)
	if loadErr != nil {
		c.report("result cache read failed")
		return
	}
	if cached != nil {
		c.publishJetStream(ctx, msg, *cached, key)
		return
	}
	answerKey := ""
	if err == nil && isAnswer && answer.Version == 1 && answer.WorkerID == c.cfg.WorkerID && validID(answer.RunID) && validID(answer.MessageID) {
		answerKey = c.cacheKey("answer", answer.RunID) + "." + hex.EncodeToString([]byte(answer.MessageID))
		cached, loadErr = c.loadResult(ctx, answerKey)
		if loadErr != nil {
			c.report("answer cache read failed")
			return
		}
		if cached != nil {
			c.publishJetStream(ctx, msg, *cached, key)
			return
		}
	}
	result := Result{Version: 1, TaskID: task.TaskID, WorkerID: c.cfg.WorkerID, AnswerSubject: c.answerRoute()}
	attempt := make([]byte, 16)
	if _, randomErr := rand.Read(attempt); randomErr != nil {
		c.report("attempt identity failed")
		return
	}
	result.AttemptID = hex.EncodeToString(attempt)
	if err == nil && !validID(task.TaskID) {
		err = errors.New("invalid task_id")
	}
	if err == nil && ((!isAnswer && task.Version != 1) || (isAnswer && answer.Version != 1)) {
		err = errors.New("JetStream requires version 1")
	}
	var run *runtime.Run
	if err == nil && isAnswer {
		if !validID(answer.MessageID) || answer.WorkerID != c.cfg.WorkerID || !validID(answer.RunID) || answer.ToolCallID == "" || len(answer.Responses) == 0 {
			err = errors.New("invalid answer route or identity")
		} else {
			admission, admissionErr := c.loadAdmission(ctx, answerKey)
			if admissionErr != nil {
				c.report("answer admission read failed")
				return
			}
			if admission != nil {
				answer = admission.Answer
				result = admission.Result
			} else {
				var prior *Result
				prior, err = c.loadResult(ctx, c.cacheKey("run", answer.RunID))
				if err == nil && (prior == nil || prior.TaskID != answer.TaskID) {
					err = errors.New("unknown task attempt")
				}
				if err == nil {
					result.AttemptID, result.RunID = prior.AttemptID, answer.RunID
				}
			}
			if err == nil {
				run, err = c.core.Runner().Snapshot(ctx, answer.RunID)
				if err == nil && admission == nil {
					if run.State != runtime.RunWaiting || run.Suspend == nil || run.Suspend.ToolCallID != answer.ToolCallID {
						err = errors.New("stale answer")
					} else {
						admission = &admittedAnswer{Answer: answer, Suspend: run.Suspend, Result: result}
						data, encodeErr := json.Marshal(admission)
						if encodeErr != nil {
							c.report("answer admission encode failed")
							return
						}
						if _, saveErr := c.core.Runner().Journal().Append(ctx, runtime.Record{RunID: answerKey, Kind: runtime.RecordExtensionData, ExtType: "nats_answer_admitted", Payload: data}); saveErr != nil {
							c.report("answer admission save failed")
							return
						}
					}
				}
				if err == nil {
					switch {
					case run.State == runtime.RunWaiting && reflect.DeepEqual(run.Suspend, admission.Suspend):
						run, err = c.turn(ctx, answer.RunID, func() (*runtime.Run, error) { return c.core.Attach(answer.RunID).Respond(ctx, answer.Responses) })
					case run.State == runtime.RunCompleted || (run.State == runtime.RunWaiting && run.Suspend != nil):
						// Resume finished before the outcome was saved. Recover its snapshot.
					default:
						// A cancelled, failed, or interrupted turn can have executed tools.
						// Repeating the answer or continuing automatically is not safe.
						err = errors.New("admitted answer resume interrupted; cannot auto continue safely")
					}
				}
			}
		}
	} else if err == nil {
		if strings.TrimSpace(task.Text) == "" {
			err = errors.New("text is required")
		} else {
			// Never send again to an uncertain original run: create a fresh address.
			ref := c.core.From("js/" + result.AttemptID)
			var runID string
			runID, err = ref.RunID(ctx)
			if err == nil {
				if err := c.admitTask(ctx, result, runID); err != nil {
					c.report("task admission save failed")
					return
				}
			}
			if err == nil {
				run, err = c.turn(ctx, runID, func() (*runtime.Run, error) {
					return ref.Send(ctx, task.Text, channel.SendOptions{Context: task.Context, Kind: "task", TurnPolicy: channel.PolicyQueue})
				})
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	if run != nil {
		result.RunID, result.State, result.Response, result.Suspend = run.ID, run.State, run.Response, run.Suspend
		if err == nil {
			err = run.Err
		}
	}
	if err != nil {
		result.Error = err.Error()
	}
	result, err = c.boundResult(ctx, result)
	if err != nil {
		c.report("result size check failed")
		return
	}
	if result.RunID != "" {
		if err := c.saveResult(ctx, c.cacheKey("run", result.RunID), result); err != nil {
			c.report("run route save failed")
			return
		}
	}
	if answerKey != "" {
		if err := c.saveResult(ctx, answerKey, result); err != nil {
			c.report("answer result save failed")
			return
		}
	}
	if err := c.saveResult(ctx, key, result); err != nil {
		c.report("result cache save failed")
		return
	}
	c.publishJetStream(ctx, msg, result, key)
}

func (c *Channel) publishJetStream(ctx context.Context, input *gonats.Msg, result Result, key string) {
	if ctx.Err() != nil {
		return
	}
	result, err := c.boundResult(ctx, result)
	if err != nil {
		c.report("result size check failed")
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		c.report("result encode failed")
		return
	}
	publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := gonats.NewMsg(c.cfg.ResultSubject)
	out.Data = data
	out.Header.Set(gonats.MsgIdHdr, key)
	if _, err = c.js.PublishMsg(out, gonats.Context(publishCtx)); err != nil {
		if ctx.Err() == nil {
			c.report("JetStream result publish failed")
		}
		return
	}
	if err = input.AckSync(gonats.Context(publishCtx)); err != nil && ctx.Err() == nil {
		c.report("JetStream input ack failed")
	}
}
