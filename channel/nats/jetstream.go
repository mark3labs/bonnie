package nats

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/mark3labs/bonnie/internal/natswire"
	"github.com/mark3labs/bonnie/runtime"
)

const defaultAckWait = 30 * time.Second

const (
	extTaskAdmitted   = "nats_task_attempt_admitted"
	extAnswerAdmitted = "nats_answer_admitted"
)

func validateJetStream(cfg Config) error {
	if cfg.Stream == "" {
		if cfg.Consumer != "" || cfg.AgentID != "" || cfg.CreateStream || cfg.TargetedTasks {
			return errors.New("bonnie: channel/nats: Stream is required for JetStream options")
		}
		return nil
	}
	if cfg.TargetedTasks {
		if err := ValidateTargetedSubjects(Subjects{Tasks: cfg.Subject, Answers: cfg.AnswerSubject, Results: cfg.ResultSubject, Events: cfg.EventSubject, Commands: cfg.CommandSubject, Queries: cfg.QuerySubject}); err != nil {
			return err
		}
	}
	if cfg.AgentID == "" {
		return errors.New("bonnie: channel/nats: AgentID is required for JetStream")
	}
	if !natswire.ValidToken(cfg.Stream) || !natswire.ValidToken(cfg.Consumer) || !natswire.ValidToken(cfg.AgentID) {
		return errors.New("bonnie: channel/nats: stream, consumer, and agent must be safe tokens (letters, digits, hyphen)")
	}
	if strings.HasPrefix(cfg.Subject, cfg.AnswerSubject+".") || strings.HasPrefix(cfg.ResultSubject, cfg.AnswerSubject+".") {
		return errors.New("bonnie: channel/nats: answer routes overlap task or result subject")
	}
	return nil
}

func (c *Channel) answerRoute() string { return c.cfg.AnswerSubject + "." + c.cfg.AgentID }

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
	inputSubjects := []string{c.cfg.Subject, c.cfg.AnswerSubject + ".*"}
	if c.cfg.TargetedTasks {
		inputSubjects = append(inputSubjects, c.cfg.Subject+".agent.*")
	}
	info, err := js.StreamInfo(c.cfg.Stream, gonats.Context(ready))
	if errors.Is(err, gonats.ErrStreamNotFound) && c.cfg.CreateStream {
		info, err = js.AddStream(&gonats.StreamConfig{Name: c.cfg.Stream, Subjects: inputSubjects, Storage: gonats.FileStorage}, gonats.Context(ready))
		// Another agent can create the stream at the same time.
		if err != nil {
			info, err = js.StreamInfo(c.cfg.Stream, gonats.Context(ready))
		}
	}
	if err != nil {
		return fmt.Errorf("bonnie: channel/nats: input stream: %w", err)
	}
	if !slices.Contains(info.Config.Subjects, c.cfg.Subject) || !slices.Contains(info.Config.Subjects, c.cfg.AnswerSubject+".*") || info.Config.Retention != gonats.LimitsPolicy {
		return errors.New("bonnie: channel/nats: input stream must retain tasks and agent answer routes with limits retention")
	}
	if c.cfg.TargetedTasks && !slices.Contains(info.Config.Subjects, c.cfg.Subject+".agent.*") {
		return errors.New("bonnie: channel/nats: input stream must retain targeted task routes")
	}
	if _, err := c.resultLimit(ready, js, nc); err != nil {
		return err
	}
	var ackWait time.Duration
	routes := []string{c.cfg.Subject, c.answerRoute()}
	if c.cfg.TargetedTasks {
		routes = append(routes, c.cfg.Subject+".agent."+c.cfg.AgentID)
	}
	for i, subject := range routes {
		name := c.cfg.Consumer
		if i == 1 {
			name += "_" + c.cfg.AgentID
		}
		if i == 2 {
			name = streamName("bonnie-target-", c.cfg.Consumer+"."+subject)
		}
		ci, err := natswire.EnsurePullConsumer(ready, js, c.cfg.Stream, &gonats.ConsumerConfig{Durable: name, AckPolicy: gonats.AckExplicitPolicy, AckWait: defaultAckWait, DeliverPolicy: gonats.DeliverAllPolicy, ReplayPolicy: gonats.ReplayInstantPolicy, FilterSubject: subject, MaxAckPending: c.cfg.Concurrency, MaxWaiting: 1024})
		if err != nil {
			return fmt.Errorf("bonnie: channel/nats: consumer: %w", err)
		}
		cc := ci.Config
		if !natswire.CompatiblePullConsumer(cc, name, subject) {
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
	// Each agent requests one message only when it has a free execution slot.
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
	for agent := range c.cfg.Concurrency {
		wg.Go(func() {
			index := agent % len(routes)
			for workCtx.Err() == nil {
				fetchCtx, cancel := context.WithTimeout(workCtx, 250*time.Millisecond)
				msgs, err := c.subs[index].Fetch(1, gonats.Context(fetchCtx))
				cancel()
				index = (index + 1) % len(routes)
				if err != nil {
					if workCtx.Err() != nil {
						return
					}
					if errors.Is(err, gonats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, gonats.ErrConnectionClosed) || errors.Is(err, gonats.ErrDisconnected) {
						continue
					}
					// A reconnect can invalidate the outstanding fetch inbox. Retry
					// without poisoning shutdown with a transient transport error.
					time.Sleep(100 * time.Millisecond)
					continue
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

// cacheKey keeps existing journal keys when they fit. Hash the full key when
// it exceeds the journal's 200-byte limit, including all answer identities.
// The reserved prefix keeps transport records out of the public run list.
func (c *Channel) cacheKey(kind string, ids ...string) string {
	var b strings.Builder
	b.WriteString(runtime.ReservedRunPrefix)
	b.WriteString("nats.")
	b.WriteString(c.cfg.Stream)
	b.WriteByte('.')
	b.WriteString(c.cfg.Consumer)
	b.WriteByte('.')
	b.WriteString(c.cfg.AgentID)
	b.WriteByte('.')
	b.WriteString(kind)
	for _, id := range ids {
		b.WriteByte('.')
		b.WriteString(hex.EncodeToString([]byte(id)))
	}
	key := b.String()
	if len(key) <= 200 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return runtime.ReservedRunPrefix + "nats.sha256." + hex.EncodeToString(sum[:])
}

// admittedTask binds a delivered stream sequence to its stable local attempt before execution.
type admittedTask struct {
	Result Result `json:"result"`
	RunID  string `json:"run_id"`
}

func (c *Channel) loadTaskAdmission(ctx context.Context, key string) (*admittedTask, error) {
	return loadAdmitted[admittedTask](ctx, c.core.Runner().Journal(), key, extTaskAdmitted)
}

// loadAdmitted returns the first admission record of extType on key, or nil
// when the key has no run or no such record.
func loadAdmitted[T any](ctx context.Context, j runtime.Journal, key, extType string) (*T, error) {
	records, err := j.Replay(ctx, key)
	if errors.Is(err, runtime.ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.ExtType == extType {
			var admission T
			if err := json.Unmarshal(record.Payload, &admission); err != nil {
				return nil, err
			}
			return &admission, nil
		}
	}
	return nil, nil
}

// admittedAnswer records the accepted input before Resume can change the run.
// Keep the full answer and suspension: a redelivery must not use changed input.
type admittedAnswer struct {
	Answer  Answer                  `json:"answer"`
	Suspend *runtime.SuspendRequest `json:"suspend"`
	Result  Result                  `json:"result"`
}

func (c *Channel) loadAdmission(ctx context.Context, key string) (*admittedAnswer, error) {
	return loadAdmitted[admittedAnswer](ctx, c.core.Runner().Journal(), key, extAnswerAdmitted)
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
	// message ID is at most 200 bytes. Reserve 512 bytes for that header and
	// NATS framing. The client JSON limit remains maxMessageBytes,
	// independently of broker headers.
	limit = min(int64(maxMessageBytes), limit-512)
	// Reserve room for maximum-length identities, including JSON escaping.
	metadata := Result{Version: 1, TaskID: strings.Repeat("<", 256), RunID: strings.Repeat("<", 256), AttemptID: strings.Repeat("a", 32), AgentID: c.cfg.AgentID, AnswerSubject: c.answerRoute(), State: runtime.RunFailed, Error: "result too large"}
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
		if !natswire.ValidID(result.TaskID) {
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

// errNoAck leaves a delivery pending for redelivery after an infrastructure
// failure. Protocol errors become Result.Error and are acknowledged instead.
var errNoAck = errors.New("bonnie: channel/nats: leave unacknowledged")

// errHandled means a cached result was already published (and acknowledged).
var errHandled = errors.New("bonnie: channel/nats: cached result handled")

type jsInput struct {
	key       string
	isAnswer  bool
	task      Task
	answer    Answer
	decodeErr error // A protocol error, not errNoAck.
}

func (c *Channel) handleJetStream(ctx context.Context, msg *gonats.Msg) {
	stop := c.heartbeat(ctx, msg)
	defer stop()
	in, err := c.decodeJetStream(msg)
	if errors.Is(err, errNoAck) {
		return
	}
	unlock := c.lockIdentity(in)
	defer unlock()
	result, answerKey, run, err := c.processJetStream(ctx, msg, in)
	if errors.Is(err, errNoAck) || errors.Is(err, errHandled) || ctx.Err() != nil {
		return
	}
	// An interrupted turn is deliberately neither cached nor acknowledged: the
	// broker redelivery must re-enter Runner with the admitted attempt identity.
	if run != nil && run.State == runtime.RunInterrupted {
		return
	}
	if err := c.finishJetStream(ctx, msg, in.key, answerKey, result, run, err); errors.Is(err, errNoAck) {
		return
	}
}

func (c *Channel) heartbeat(ctx context.Context, msg *gonats.Msg) func() {
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
	return func() { close(finished); <-watched }
}

func (c *Channel) decodeJetStream(msg *gonats.Msg) (jsInput, error) {
	meta, err := msg.Metadata()
	if err != nil {
		c.report("JetStream metadata failed")
		return jsInput{}, errNoAck
	}
	key := c.cacheKey("input", fmt.Sprint(meta.Sequence.Stream))
	var answer Answer
	var task Task
	isAnswer := msg.Subject == c.answerRoute()
	if isAnswer {
		err = decode(msg.Data, &answer)
		task.TaskID = answer.TaskID
	} else {
		err = decode(msg.Data, &task)
	}
	return jsInput{key: key, isAnswer: isAnswer, task: task, answer: answer, decodeErr: err}, nil
}

// Serialize same-run answers and redelivered input within this process.
func (c *Channel) lockIdentity(in jsInput) func() {
	identity := in.key
	if in.isAnswer {
		identity = in.answer.RunID
	}
	var hash uint32
	for _, b := range []byte(identity) {
		hash = hash*31 + uint32(b)
	}
	lock := &c.locks[hash%uint32(len(c.locks))]
	lock.Lock()
	return lock.Unlock
}

// processJetStream returns errHandled for a cached publication, errNoAck for
// infrastructure failures, and an ordinary error for a protocol or turn result.
func (c *Channel) processJetStream(ctx context.Context, msg *gonats.Msg, in jsInput) (Result, string, *runtime.Run, error) {
	key, isAnswer, task, answer, err := in.key, in.isAnswer, in.task, in.answer, in.decodeErr
	cached, loadErr := c.loadResult(ctx, key)
	if loadErr != nil {
		c.report("result cache read failed")
		return Result{}, "", nil, errNoAck
	}
	if cached != nil {
		if err := c.publishJetStreamResult(ctx, msg, *cached, key); err != nil {
			return Result{}, "", nil, err
		}
		return Result{}, "", nil, errHandled
	}
	answerKey := ""
	if err == nil && isAnswer && answer.Version == 1 && answer.AgentID == c.cfg.AgentID && natswire.ValidID(answer.RunID) && natswire.ValidID(answer.MessageID) {
		answerKey = c.cacheKey("answer", answer.RunID, answer.MessageID)
		cached, loadErr = c.loadResult(ctx, answerKey)
		if loadErr != nil {
			c.report("answer cache read failed")
			return Result{}, "", nil, errNoAck
		}
		if cached != nil {
			if err := c.publishJetStreamResult(ctx, msg, *cached, key); err != nil {
				return Result{}, "", nil, err
			}
			return Result{}, "", nil, errHandled
		}
	}
	result := Result{Version: 1, TaskID: task.TaskID, AgentID: c.cfg.AgentID, AnswerSubject: c.answerRoute()}
	var taskAdmission *admittedTask
	if err == nil && !isAnswer {
		taskAdmission, loadErr = c.loadTaskAdmission(ctx, key)
		if loadErr != nil {
			c.report("task admission read failed")
			return Result{}, "", nil, errNoAck
		}
		if taskAdmission != nil {
			result = taskAdmission.Result
		}
	}
	if taskAdmission == nil {
		attempt := make([]byte, 16)
		if _, randomErr := rand.Read(attempt); randomErr != nil {
			c.report("attempt identity failed")
			return Result{}, "", nil, errNoAck
		}
		result.AttemptID = hex.EncodeToString(attempt)
	}
	if err == nil && !natswire.ValidID(task.TaskID) {
		err = errors.New("invalid task_id")
	}
	if err == nil && ((!isAnswer && task.Version != 1) || (isAnswer && answer.Version != 1)) {
		err = errors.New("JetStream requires version 1")
	}
	if err != nil {
		return result, answerKey, nil, err
	}
	var run *runtime.Run
	if isAnswer {
		run, result, err = c.admitJetStreamAnswer(ctx, answer, answerKey, result)
	} else {
		run, err = c.admitJetStreamTask(ctx, task, key, result, taskAdmission)
	}
	return result, answerKey, run, err
}

func (c *Channel) admitJetStreamAnswer(ctx context.Context, answer Answer, answerKey string, result Result) (*runtime.Run, Result, error) {
	if !natswire.ValidID(answer.MessageID) || answer.AgentID != c.cfg.AgentID || !natswire.ValidID(answer.RunID) || answer.ToolCallID == "" || len(answer.Responses) == 0 {
		return nil, result, errors.New("invalid answer route or identity")
	}
	admission, err := c.loadAdmission(ctx, answerKey)
	if err != nil {
		c.report("answer admission read failed")
		return nil, result, errNoAck
	}
	if admission != nil {
		answer = admission.Answer
		result = admission.Result
	} else {
		prior, err := c.loadResult(ctx, c.cacheKey("run", answer.RunID))
		if err != nil {
			return nil, result, err
		}
		if prior == nil || prior.TaskID != answer.TaskID {
			return nil, result, errors.New("unknown task attempt")
		}
		result.AttemptID, result.RunID = prior.AttemptID, answer.RunID
	}
	run, err := c.core.Runner().Snapshot(ctx, answer.RunID)
	if err != nil {
		return run, result, err
	}
	if admission == nil {
		if run.State != runtime.RunWaiting || run.Suspend == nil || run.Suspend.ToolCallID != answer.ToolCallID {
			return run, result, errors.New("stale answer")
		}
		admission = &admittedAnswer{Answer: answer, Suspend: run.Suspend, Result: result}
		data, encodeErr := json.Marshal(admission)
		if encodeErr != nil {
			c.report("answer admission encode failed")
			return nil, result, errNoAck
		}
		if _, saveErr := c.core.Runner().Journal().Append(ctx, runtime.Record{RunID: answerKey, Kind: runtime.RecordExtensionData, ExtType: extAnswerAdmitted, Payload: data}); saveErr != nil {
			c.report("answer admission save failed")
			return nil, result, errNoAck
		}
	}
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
	return run, result, err
}

func (c *Channel) admitJetStreamTask(ctx context.Context, task Task, key string, result Result, taskAdmission *admittedTask) (*runtime.Run, error) {
	if strings.TrimSpace(task.Text) == "" {
		return nil, errors.New("text is required")
	}
	// Reuse the durable mapping on redelivery so Runner continues the same attempt.
	ref := c.core.From("js/" + result.AttemptID)
	var runID string
	if taskAdmission != nil {
		runID = taskAdmission.RunID
	} else {
		var err error
		runID, err = ref.RunID(ctx)
		if err != nil {
			return nil, err
		}
		taskAdmission = &admittedTask{Result: result, RunID: runID}
		data, encodeErr := json.Marshal(taskAdmission)
		if encodeErr != nil {
			c.report("task admission encode failed")
			return nil, errNoAck
		}
		if _, saveErr := c.core.Runner().Journal().Append(ctx, runtime.Record{RunID: key, Kind: runtime.RecordExtensionData, ExtType: extTaskAdmitted, Payload: data}); saveErr != nil {
			c.report("task admission save failed")
			return nil, errNoAck
		}
		if err := c.admitTask(ctx, result, runID); err != nil {
			c.report("task admission index save failed")
			return nil, errNoAck
		}
	}
	return c.turn(ctx, runID, func() (*runtime.Run, error) {
		return ref.SendRetry(ctx, task.Text, channel.SendOptions{Context: task.Context, Kind: "task"})
	})
}

func (c *Channel) finishJetStream(ctx context.Context, msg *gonats.Msg, key, answerKey string, result Result, run *runtime.Run, err error) error {
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
		if ctx.Err() == nil {
			c.report("result size check failed")
		}
		return errNoAck
	}
	if result.RunID != "" {
		if err := c.saveResult(ctx, c.cacheKey("run", result.RunID), result); err != nil {
			c.report("run route save failed")
			return errNoAck
		}
	}
	if answerKey != "" {
		if err := c.saveResult(ctx, answerKey, result); err != nil {
			c.report("answer result save failed")
			return errNoAck
		}
	}
	if err := c.saveResult(ctx, key, result); err != nil {
		c.report("result cache save failed")
		return errNoAck
	}
	return c.publishJetStreamResult(ctx, msg, result, key)
}

// publishJetStream keeps the existing publication entry point for callers that
// do not need the ack outcome. Failures are reported by the result helper.
func (c *Channel) publishJetStream(ctx context.Context, input *gonats.Msg, result Result, key string) {
	_ = c.publishJetStreamResult(ctx, input, result, key)
}

func (c *Channel) publishJetStreamResult(ctx context.Context, input *gonats.Msg, result Result, key string) error {
	if ctx.Err() != nil {
		return errNoAck
	}
	result, err := c.boundResult(ctx, result)
	if err != nil {
		// Shutdown can cancel the broker lookup after the earlier context check.
		// Leave input unacknowledged for redelivery without recording a failure.
		if ctx.Err() == nil {
			c.report("result size check failed")
		}
		return errNoAck
	}
	data, err := json.Marshal(result)
	if err != nil {
		c.report("result encode failed")
		return errNoAck
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
		return errNoAck
	}
	if err = input.AckSync(gonats.Context(publishCtx)); err != nil {
		if ctx.Err() == nil {
			c.report("JetStream input ack failed")
		}
		return errNoAck
	}
	return nil
}
