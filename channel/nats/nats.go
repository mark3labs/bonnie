// Package nats carries asynchronous tasks over Core NATS or JetStream.
//
// Core delivery and deduplication are process-local and can lose messages.
// JetStream delivery is at least once. A completed outcome is saved in the
// local journal before publication; input is acknowledged only after the
// result broker acknowledges publication. An interrupted task starts a new
// independent attempt. Redelivery to another worker with a separate journal
// can execute again. This is not an exactly-once execution guarantee.
//
// Only explicit answers with the current run and tool-call IDs can resume a
// waiting run. Use NATS permissions to restrict publishers and subscribers.
// All results use the configured subject, never Msg.Reply or publisher input.
package nats

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	gonats "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/presence"
	"github.com/mark3labs/bonnie/runtime"
)

const maxMessageBytes = 1 << 20
const dedupeLimit = 4096

// Config sets the connection, subjects, and resource limits.
type Config struct {
	// RootSubject derives empty protocol subjects and enables JetStream.
	RootSubject string
	// EventSubject receives durable status-only events. Empty disables events.
	EventSubject string
	// ResultStream and EventStream override root-derived output stream names.
	// They select provisioned streams; existing resources are never changed.
	ResultStream string
	EventStream  string
	// CommandSubject and QuerySubject are worker-routed control bases.
	CommandSubject string
	QuerySubject   string
	// URL is one NATS server URL. It is required unless Conn is supplied.
	URL string
	// Conn is optional. The caller owns it; Shutdown never closes it. Do not
	// supply URL or authentication fields with Conn. This channel does not
	// change its handlers.
	Conn *gonats.Conn
	// NKeySeed is a user NKey seed, not a file path. It authenticates the
	// connection opened by this channel. Keep it secret. For JWT credentials
	// or other authentication options, supply an authenticated Conn instead.
	NKeySeed string
	// Token is a bearer token for NATS token authentication. Use only one of
	// Token, NKeySeed, or Username/Password. Keep credentials secret.
	Token string
	// Username and Password select user/password authentication. Username is
	// required when Password is set; an empty password is permitted.
	Username string
	// Password is the password for Username. Keep it secret.
	Password string
	// Subject receives Task JSON. AnswerSubject receives Answer JSON in Core
	// mode. In JetStream it is a base; answers use base+"."+WorkerID.
	// ResultSubject receives Result JSON. These must be distinct literal subjects.
	Subject       string
	AnswerSubject string
	ResultSubject string
	// Concurrency is the worker limit (default 4, maximum 1024).
	Concurrency int
	// Buffer is the pending message limit per subscription and work queue
	// (default 64, maximum 65536). Core NATS drops excess messages.
	Buffer int
	// Stream enables JetStream when nonempty. There is no separate mode flag.
	Stream string
	// TargetedTasks enables tasks on Subject+".worker."+WorkerID in addition
	// to shared tasks. JetStream is required. The input stream must also
	// retain Subject+".worker.*"; existing streams are never changed.
	TargetedTasks bool
	// Consumer is the shared durable task pull consumer. When empty in
	// JetStream mode, DefaultConsumerName(Subject) supplies a stable name.
	// Set it explicitly for separate processing groups or existing consumers.
	Consumer string
	// WorkerID is required in JetStream mode. It identifies this worker and
	// its local journal. Use a stable, unique token (letters, digits, hyphen)
	// and keep the same journal when this worker restarts.
	WorkerID string
	// CreateStream permits creation of the input stream. With RootSubject it
	// also provisions result and status streams. Existing resources are never
	// changed. Without a root, the result stream remains operator-owned.
	CreateStream bool
}

// Task starts one independent run. Context is journalled separately from Text.
type Task struct {
	// Version is 1 in JetStream. Core also accepts legacy version 0.
	Version int      `json:"version,omitempty"`
	TaskID  string   `json:"task_id"`
	Text    string   `json:"text"`
	Context []string `json:"context,omitempty"`
}

// Answer resumes a waiting task at the specified suspension.
type Answer struct {
	// Version is 1 in JetStream. MessageID is a stable answer identity.
	Version   int    `json:"version,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	// RunID and WorkerID select the waiting attempt in JetStream.
	RunID      string                  `json:"run_id,omitempty"`
	WorkerID   string                  `json:"worker_id,omitempty"`
	TaskID     string                  `json:"task_id"`
	ToolCallID string                  `json:"tool_call_id"`
	Responses  []runtime.InputResponse `json:"responses"`
}

// Result is one task outcome or a rejected message. Error is set on failure.
// Malformed JSON can produce a result with no task_id or run_id.
type Result struct {
	// Version is 1 in JetStream. AttemptID identifies the independent task
	// attempt and stays the same when an answer resumes that attempt.
	Version   int    `json:"version,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
	WorkerID  string `json:"worker_id,omitempty"`
	// AnswerSubject is the worker route. Clients must check it against their
	// configured answer base before publishing an answer.
	AnswerSubject string                  `json:"answer_subject,omitempty"`
	TaskID        string                  `json:"task_id"`
	RunID         string                  `json:"run_id,omitempty"`
	State         runtime.RunState        `json:"state,omitempty"`
	Response      string                  `json:"response,omitempty"`
	Suspend       *runtime.SuspendRequest `json:"suspend,omitempty"`
	Error         string                  `json:"error,omitempty"`
}

// Channel implements the channel and lifecycle contracts. It can start once.
// Shutdown cancels work and waits. A deadline does not stop the background
// wait; a later Shutdown can wait again. Publish failures are reported on
// stderr without connection details and returned by Shutdown.
type Channel struct {
	core     *chat.Core
	cfg      Config
	mu       sync.Mutex
	started  bool
	stopped  bool
	cancel   context.CancelFunc
	done     chan struct{}
	conn     *gonats.Conn
	subs     []*gonats.Subscription
	failure  error
	seenMu   sync.Mutex
	seen     map[string]bool
	order    []string
	locks    [256]sync.Mutex
	js       gonats.JetStreamContext
	ackWait  time.Duration
	statusMu sync.Mutex
}

var _ channel.Channel = (*Channel)(nil)
var _ channel.Inbound = (*Channel)(nil)
var _ channel.Lifecycle = (*Channel)(nil)

// New validates config without connecting. Start establishes subscriptions.
func New(r *runtime.Runner, cfg Config) (*Channel, error) {
	s, err := ResolveSubjects(cfg.RootSubject, Subjects{Tasks: cfg.Subject, Results: cfg.ResultSubject, Answers: cfg.AnswerSubject, Events: cfg.EventSubject, Commands: cfg.CommandSubject, Queries: cfg.QuerySubject})
	if err != nil {
		return nil, err
	}
	cfg.Subject, cfg.ResultSubject, cfg.AnswerSubject = s.Tasks, s.Results, s.Answers
	cfg.EventSubject, cfg.CommandSubject, cfg.QuerySubject = s.Events, s.Commands, s.Queries
	if cfg.RootSubject != "" && cfg.Stream == "" {
		cfg.Stream = DefaultInputStreamName(cfg.Subject)
	}
	if cfg.ResultStream == "" {
		cfg.ResultStream = DefaultResultStreamName(cfg.ResultSubject)
	}
	if cfg.EventStream == "" && cfg.EventSubject != "" {
		cfg.EventStream = DefaultEventStreamName(cfg.EventSubject)
	}
	if !safeToken(cfg.ResultStream) || (cfg.EventSubject != "" && !safeToken(cfg.EventStream)) {
		return nil, errors.New("bonnie: channel/nats: invalid output stream name")
	}
	if cfg.Stream == "" && (cfg.EventSubject != "" || cfg.CommandSubject != "" || cfg.QuerySubject != "") {
		return nil, errors.New("bonnie: channel/nats: status and control require JetStream")
	}
	if r == nil {
		return nil, errors.New("bonnie: channel/nats: runner is required")
	}
	if cfg.Conn != nil && (cfg.URL != "" || cfg.NKeySeed != "" || cfg.Token != "" || cfg.Username != "" || cfg.Password != "") {
		return nil, errors.New("bonnie: channel/nats: use URL and authentication fields or Conn, not both")
	}
	methods := 0
	for _, selected := range []bool{cfg.NKeySeed != "", cfg.Token != "", cfg.Username != "" || cfg.Password != ""} {
		if selected {
			methods++
		}
	}
	if methods > 1 {
		return nil, errors.New("bonnie: channel/nats: use only one authentication method")
	}
	if cfg.Password != "" && cfg.Username == "" {
		return nil, errors.New("bonnie: channel/nats: password requires username")
	}
	if cfg.NKeySeed != "" {
		if _, err := nkeyOption(cfg.NKeySeed); err != nil {
			return nil, err
		}
	}
	if cfg.Conn == nil {
		u, err := url.Parse(cfg.URL)
		if err != nil || u.Host == "" || (u.Scheme != "nats" && u.Scheme != "tls" && u.Scheme != "ws" && u.Scheme != "wss") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("bonnie: channel/nats: invalid URL")
		}
		if u.User != nil && methods > 0 {
			return nil, errors.New("bonnie: channel/nats: use URL credentials or authentication fields, not both")
		}
	}
	for _, s := range []string{cfg.Subject, cfg.AnswerSubject, cfg.ResultSubject} {
		if !validSubject(s) {
			return nil, errors.New("bonnie: channel/nats: subjects must be nonempty literal NATS subjects")
		}
	}
	if cfg.Subject == cfg.AnswerSubject || cfg.Subject == cfg.ResultSubject || cfg.AnswerSubject == cfg.ResultSubject {
		return nil, errors.New("bonnie: channel/nats: subjects must be distinct")
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 4
	}
	if cfg.Buffer == 0 {
		cfg.Buffer = 64
	}
	if cfg.Concurrency < 1 || cfg.Concurrency > 1024 || cfg.Buffer < 1 || cfg.Buffer > 65536 {
		return nil, errors.New("bonnie: channel/nats: invalid concurrency or buffer limit")
	}
	if cfg.Stream != "" && cfg.Consumer == "" {
		cfg.Consumer = DefaultConsumerName(cfg.Subject)
	}
	if err := validateJetStream(cfg); err != nil {
		return nil, err
	}
	return &Channel{core: chat.NewCore(r, "nats", channel.PolicyQueue), cfg: cfg, seen: make(map[string]bool)}, nil
}

// DefaultConsumerName returns the stable task consumer name for a subject.
// It uses "bonnie-" and the full SHA-256 digest of the exact subject. Workers
// on the same stream and subject share this consumer unless Consumer is set.
// It does not validate the subject; New performs that check.
func DefaultConsumerName(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return "bonnie-" + hex.EncodeToString(sum[:])
}

// nkeyOption validates without exposing the seed in an error. Recreate the
// key for each signature so reconnects work without retaining a decoded key.
func nkeyOption(seed string) (gonats.Option, error) {
	kp, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		return nil, errors.New("bonnie: channel/nats: invalid user NKey seed")
	}
	pub, err := kp.PublicKey()
	kp.Wipe()
	if err != nil || !nkeys.IsValidPublicUserKey(pub) {
		return nil, errors.New("bonnie: channel/nats: invalid user NKey seed")
	}
	return gonats.Nkey(pub, func(nonce []byte) ([]byte, error) {
		key, err := nkeys.FromSeed([]byte(seed))
		if err != nil {
			return nil, errors.New("bonnie: channel/nats: invalid user NKey seed")
		}
		defer key.Wipe()
		return key.Sign(nonce)
	}), nil
}

func validSubject(s string) bool {
	if s == "" || strings.ContainsAny(s, "*>\\") || strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	return !slices.Contains(strings.Split(s, "."), "")
}

// Name implements channel.Channel.
func (c *Channel) Name() string { return "nats" }

// PresenceEndpoints describes the configured nats endpoint without credentials.
func (c *Channel) PresenceEndpoints() []presence.Endpoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	ready := c.conn != nil && c.conn.IsConnected() && !c.stopped
	address := c.cfg.Subject
	if c.cfg.TargetedTasks {
		address += ".worker." + c.cfg.WorkerID
	}
	return []presence.Endpoint{{Channel: c.Name(), Address: address, Input: true, Delivery: true, Ready: ready}}
}

// WorkerIdentity implements [channel.WorkerIdentity].
func (c *Channel) WorkerIdentity() string { return c.cfg.WorkerID }

// Routes implements channel.Channel; NATS has no HTTP routes.
func (c *Channel) Routes() []channel.Route { return nil }

// From implements channel.Inbound. The address is a task_id.
func (c *Channel) From(address string) channel.SessionRef { return c.core.From(address) }

// Attach implements channel.Inbound.
func (c *Channel) Attach(runID string) channel.SessionRef { return c.core.Attach(runID) }

// Start connects and subscribes before it returns. The context owns all work.
func (c *Channel) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.stopped {
		return errors.New("bonnie: channel/nats: lifecycle already started or stopped")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	nc := c.cfg.Conn
	if nc == nil {
		var err error
		opts := []gonats.Option{gonats.Timeout(5 * time.Second)}
		if c.cfg.NKeySeed != "" {
			auth, authErr := nkeyOption(c.cfg.NKeySeed)
			if authErr != nil {
				return authErr
			}
			opts = append(opts, auth)
		}
		if c.cfg.Token != "" {
			opts = append(opts, gonats.Token(c.cfg.Token))
		}
		if c.cfg.Username != "" {
			opts = append(opts, gonats.UserInfo(c.cfg.Username, c.cfg.Password))
		}
		nc, err = gonats.Connect(c.cfg.URL, opts...)
		if err != nil {
			return errors.New("bonnie: channel/nats: connection failed")
		}
	}
	cleanup := func() {
		for _, sub := range c.subs {
			if err := sub.Unsubscribe(); err != nil {
				c.failure = errors.Join(c.failure, errors.New("bonnie: channel/nats: unsubscribe failed"))
			}
		}
		c.subs = nil
		if c.cfg.Conn == nil {
			nc.Close()
		}
	}
	if c.cfg.Stream != "" {
		if err := c.startJetStream(ctx, nc); err != nil {
			cleanup()
			return err
		}
		return nil
	}
	for _, subject := range []string{c.cfg.Subject, c.cfg.AnswerSubject} {
		sub, err := nc.SubscribeSync(subject)
		if err != nil {
			cleanup()
			return errors.New("bonnie: channel/nats: subscribe failed")
		}
		c.subs = append(c.subs, sub)
		if err := sub.SetPendingLimits(c.cfg.Buffer, maxMessageBytes*c.cfg.Buffer); err != nil {
			cleanup()
			return errors.New("bonnie: channel/nats: pending limit failed")
		}
	}
	ready, cancelReady := context.WithTimeout(ctx, 5*time.Second)
	err := nc.FlushWithContext(ready)
	cancelReady()
	if err != nil {
		cleanup()
		return errors.New("bonnie: channel/nats: subscription flush failed")
	}
	workCtx, cancel := context.WithCancel(ctx)
	c.started, c.cancel, c.conn, c.done = true, cancel, nc, make(chan struct{})
	jobs := make(chan *gonats.Msg, c.cfg.Buffer)
	var wg sync.WaitGroup
	for _, sub := range c.subs {
		wg.Go(func() {
			for {
				msg, err := sub.NextMsgWithContext(workCtx)
				if err != nil {
					if workCtx.Err() != nil {
						return
					}
					c.report("subscription receive failed")
					if errors.Is(err, gonats.ErrSlowConsumer) {
						continue
					}
					cancel()
					return
				}
				select {
				case jobs <- msg:
				case <-workCtx.Done():
					return
				}
			}
		})
	}
	for range c.cfg.Concurrency {
		wg.Go(func() {
			for {
				select {
				case <-workCtx.Done():
					return
				case msg := <-jobs:
					if workCtx.Err() != nil {
						return
					}
					c.handle(workCtx, msg)
				}
			}
		})
	}
	go func() {
		<-workCtx.Done()
		for _, sub := range c.subs {
			if err := sub.Unsubscribe(); err != nil {
				c.report("unsubscribe failed")
			}
		}
		wg.Wait()
		if c.cfg.Conn == nil {
			nc.Close()
		}
		close(c.done)
	}()
	return nil
}

// Shutdown stops admission, cancels turns, and waits for all owned goroutines.
func (c *Channel) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	c.stopped = true
	if !c.started {
		err := c.failure
		c.mu.Unlock()
		return err
	}
	c.cancel()
	done := c.done
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return fmt.Errorf("bonnie: channel/nats: shutdown: %w", ctx.Err())
	case <-done:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failure
}

func (c *Channel) report(message string) {
	err := errors.New("bonnie: channel/nats: " + message)
	c.mu.Lock()
	// Keep memory bounded even when a server repeatedly drops messages.
	if c.failure == nil {
		c.failure = err
	}
	c.mu.Unlock()
	fmt.Fprintln(os.Stderr, err)
}

func decode(data []byte, out any) error {
	if len(data) > maxMessageBytes {
		return errors.New("message too large")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("invalid JSON message")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid JSON message")
	}
	return nil
}

func validID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id && strings.IndexFunc(id, unicode.IsControl) < 0 && !strings.HasPrefix(id, "nats/")
}

func (c *Channel) claim(id string) bool {
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	if c.seen[id] {
		return false
	}
	if len(c.order) == dedupeLimit {
		delete(c.seen, c.order[0])
		c.order = c.order[1:]
	}
	c.seen[id] = true
	c.order = append(c.order, id)
	return true
}

func (c *Channel) handle(ctx context.Context, msg *gonats.Msg) {
	var task Task
	var answer Answer
	var err error
	isAnswer := msg.Subject == c.cfg.AnswerSubject
	if isAnswer {
		err = decode(msg.Data, &answer)
		task.TaskID = answer.TaskID
	} else {
		err = decode(msg.Data, &task)
	}
	if err == nil && ((!isAnswer && task.Version != 0 && task.Version != 1) || (isAnswer && answer.Version != 0 && answer.Version != 1)) {
		err = errors.New("unsupported version")
	}
	if err == nil && !validID(task.TaskID) {
		err = errors.New("invalid task_id")
	}
	if err == nil && !isAnswer && strings.TrimSpace(task.Text) == "" {
		err = errors.New("text is required")
	}
	if err == nil && isAnswer && (answer.ToolCallID == "" || len(answer.Responses) == 0) {
		err = errors.New("tool_call_id and responses are required")
	}
	if err != nil {
		c.publish(ctx, Result{TaskID: task.TaskID, Error: err.Error()})
		return
	}
	// Fixed stripes bound lock memory and serialize this adapter's check and
	// entry for a task. Callers must not also mutate these tasks through From.
	var hash uint32
	for _, b := range []byte(task.TaskID) {
		hash = hash*31 + uint32(b)
	}
	lock := &c.locks[hash%uint32(len(c.locks))]
	lock.Lock()
	defer lock.Unlock()
	if ctx.Err() != nil {
		return
	}
	var run *runtime.Run
	runID, bound, err := c.core.Lookup(ctx, task.TaskID)
	if err == nil && isAnswer {
		if !bound {
			err = errors.New("unknown task")
		} else {
			run, err = c.core.Runner().Snapshot(ctx, runID)
			if err == nil {
				if run.State != runtime.RunWaiting || run.Suspend == nil || run.Suspend.ToolCallID != answer.ToolCallID {
					err = errors.New("stale answer")
				} else {
					run, err = c.turn(ctx, runID, func() (*runtime.Run, error) {
						return c.core.Attach(runID).Respond(ctx, answer.Responses)
					})
				}
			}
		}
	} else if err == nil {
		if bound || !c.claim(task.TaskID) {
			err = errors.New("duplicate task")
		} else {
			ref := c.core.From(task.TaskID)
			runID, err = ref.RunID(ctx)
			if err == nil {
				run, err = c.turn(ctx, runID, func() (*runtime.Run, error) {
					return ref.Send(ctx, task.Text, channel.SendOptions{Context: task.Context, Kind: "task", TurnPolicy: channel.PolicyQueue})
				})
			}
		}
	}
	result := Result{TaskID: task.TaskID, RunID: runID}
	if run != nil {
		result.RunID, result.State, result.Response, result.Suspend = run.ID, run.State, run.Response, run.Suspend
		if err == nil {
			err = run.Err
		}
	}
	if err != nil {
		result.Error = err.Error()
	}
	c.publish(ctx, result)
}

func (c *Channel) publish(ctx context.Context, result Result) {
	// Shutdown can discard outcomes. It must not report its own cancellation
	// as a transport failure.
	if ctx.Err() != nil {
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		c.report("result encode failed")
		return
	}
	if err := c.conn.Publish(c.cfg.ResultSubject, data); err != nil {
		c.report("result publish failed")
		return
	}
	flush, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.conn.FlushWithContext(flush); err != nil && ctx.Err() == nil {
		c.report("result flush failed")
	}
}

// turn interrupts a detached runner turn when its owning channel lifecycle ends.
func (c *Channel) turn(ctx context.Context, runID string, execute func() (*runtime.Run, error)) (*runtime.Run, error) {
	finished := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		connection := time.NewTicker(100 * time.Millisecond)
		defer connection.Stop()
	watch:
		for {
			select {
			case <-finished:
				return
			case <-ctx.Done():
				break watch
			case <-connection.C:
				c.mu.Lock()
				nc := c.conn
				c.mu.Unlock()
				if nc != nil && !nc.IsConnected() {
					break watch
				}
			}
		}
		timer := time.NewTicker(time.Millisecond)
		defer timer.Stop()
		for {
			err := c.core.Runner().Interrupt(runID)
			if err != nil && !errors.Is(err, runtime.ErrRunNotActive) {
				c.report("turn interruption failed")
			}
			select {
			case <-finished:
				return
			case <-timer.C:
			}
		}
	}()
	run, err := execute()
	close(finished)
	<-watched
	return run, err
}
