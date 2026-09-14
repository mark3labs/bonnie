// Package runtime is BONNIE's durable execution layer (L1).
//
// It turns a Kit agent turn — which is an in-process for-loop — into a durable
// run that survives process death and can park indefinitely awaiting human
// input without holding compute.
//
// The layer is built entirely on Kit's public SDK. It uses four seams:
//
//   - [kit.SessionManager] via [Session], so every appended message is
//     journalled before it is kept.
//   - Kit.OnStepFinish, to checkpoint at the agent-step boundary.
//   - Kit.OnContextPrepare, to put a turn's context in front of its prompt.
//   - kit.ToolOutput{Halt, FinalValue}, as the suspension contract.
//
// None of these require access to Kit's internal packages. BONNIE is a
// separate Go module precisely so the compiler enforces that.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Agent is the slice of Kit that the runner drives. *kit.Kit satisfies it.
// Keeping it an interface makes the durable executor testable without a live
// model, and documents exactly how small BONNIE's dependency on Kit is.
type Agent interface {
	PromptResult(ctx context.Context, message string) (*kit.TurnResult, error)
	InjectSteer(message string)
	Close() error
}

// Compile-time proof that the real Kit satisfies the seam. If Kit changes one
// of these signatures, the build breaks here rather than at a call site.
var _ Agent = (*kit.Kit)(nil)

// AgentFactory builds an agent bound to a restored session. The runner calls
// it once per Start or Resume, so a run that resumes in a fresh process gets a
// fresh agent wired to the replayed conversation.
type AgentFactory func(ctx context.Context, s *Session) (Agent, error)

// KitAgent is the default [AgentFactory]. It constructs a real Kit instance
// with the BONNIE session installed and the HITL tools registered.
//
// It deliberately applies the options to a [kit.Options] value and calls
// [kit.New], rather than calling [kit.NewAgent] and then SetSessionManager.
// NewAgent builds its own file-backed session during construction, so the
// later replacement would leave a stray Kit session file behind on every run.
// Passing SessionManager up front means Kit never creates one.
func KitAgent(opts ...kit.Option) AgentFactory {
	return func(ctx context.Context, s *Session) (Agent, error) {
		streaming := true
		o := &kit.Options{Streaming: &streaming}
		for _, fn := range opts {
			fn(o)
		}

		// BONNIE owns persistence. Kit must not open a session of its own.
		o.SessionManager = s
		o.ExtraTools = append(o.ExtraTools, AskTool(), ApprovalTool())

		k, err := kit.New(ctx, o)
		if err != nil {
			return nil, fmt.Errorf("bonnie: build agent: %w", err)
		}
		attachCheckpoints(k, s)
		return k, nil
	}
}

// attachCheckpoints wires Kit's hooks to the journal. This is where durability
// actually happens.
func attachCheckpoints(k *kit.Kit, s *Session) {
	// Checkpoint at the step boundary. On replay we know which steps already
	// ran, so their side effects are not repeated.
	k.OnStepFinish(func(e kit.StepFinishEvent) {
		_, _ = s.journal.Append(context.Background(), Record{
			RunID:     s.runID,
			Kind:      RecordStep,
			Timestamp: now(),
			Text:      e.FinishReason,
			Payload: encodeSuspend(SuspendRequest{
				Kind:   "step",
				Prompt: fmt.Sprintf("step %d", e.StepNumber),
			}),
		})
	})

	// The context-prepare hook is where a turn's context reaches the model.
	// The replayed session already holds the full conversation, so the
	// window is correct as assembled; the hook adds the per-turn context and
	// the run's origin in front of the prompt, for this one call. A turn
	// with neither returns nil, which keeps Kit's window unchanged.
	k.OnContextPrepare(kit.HookPriorityHigh,
		func(h kit.ContextPrepareHook) *kit.ContextPrepareResult {
			msgs := prepareContext(h.Messages, s.Origin(), s.TurnContext())
			if msgs == nil {
				return nil
			}
			return &kit.ContextPrepareResult{Messages: msgs}
		})
}

// Input starts or continues a run.
type Input struct {
	// Text is the user's message: the one thing that enters the
	// conversation as a user turn.
	Text  string
	Files []kit.LLMFilePart
	// Context is what the model should know for this turn and this turn
	// only: the event that fired, the diff a comment refers to, who is
	// speaking. It is journalled as a [RecordContext] and shown to the
	// model in front of Text; it never becomes conversation history.
	Context []string
	// Title names the run in operator-facing listings. It is recorded on
	// the first turn that carries one and ignored after that.
	Title string
	// Origin says where the conversation lives. It is recorded on the
	// first turn that carries one and ignored after that.
	Origin Origin
}

// Run is a snapshot of a durable run after a turn boundary.
type Run struct {
	ID       string
	State    RunState
	Response string
	// Suspend is non-nil when State is [RunWaiting].
	Suspend *SuspendRequest
	Usage   *kit.LLMUsage
	Err     error
}

// Runner executes durable runs against a [Journal].
//
// One Runner owns the runs it is executing: [Runner.Cancel] and
// [Runner.Steer] reach only into turns this Runner started. A run suspended
// by one Runner is resumed by another through the journal, which is the whole
// point of L1.
type Runner struct {
	journal Journal
	factory AgentFactory
	bus     *EventBus

	mu     sync.Mutex
	active map[string]*activeTurn
}

// activeTurn is the handle a Runner keeps on a turn it is executing, so an
// operator can steer or stop it from another goroutine.
type activeTurn struct {
	cancel context.CancelFunc

	mu    sync.Mutex
	agent Agent
}

func (a *activeTurn) setAgent(ag Agent) {
	a.mu.Lock()
	a.agent = ag
	a.mu.Unlock()
}

func (a *activeTurn) steer(msg string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agent == nil {
		return false
	}
	a.agent.InjectSteer(msg)
	return true
}

// ErrRunActive is returned when a run is already executing.
var ErrRunActive = errors.New("bonnie: run already active")

// ErrRunNotActive is returned when Cancel or Steer targets a run that this
// Runner is not currently executing.
var ErrRunNotActive = errors.New("bonnie: run is not active")

// ErrNotWaiting is returned when Resume targets a run that is not suspended.
var ErrNotWaiting = errors.New("bonnie: run is not waiting for input")

// RunnerOption configures a [Runner].
type RunnerOption func(*Runner)

// NewRunner returns a runner. A nil journal defaults to [MemoryJournal]; a nil
// factory defaults to [KitAgent] with no extra options.
func NewRunner(j Journal, f AgentFactory, opts ...RunnerOption) *Runner {
	if j == nil {
		j = NewMemoryJournal()
	}
	if f == nil {
		f = KitAgent()
	}
	r := &Runner{
		journal: j,
		factory: f,
		bus:     NewEventBus(DefaultEventBuffer),
		active:  make(map[string]*activeTurn),
	}
	for _, opt := range opts {
		opt(r)
	}
	r.bus.Anchor(r.durableSeq)
	return r
}

// durableSeq reports the journal position for a run, or 0 when the journal
// cannot say. It is what anchors events to records; a journal without
// [Positioner] leaves its events unstamped, and the stream then serves the
// live backlog only.
func (r *Runner) durableSeq(runID string) int {
	p, ok := r.journal.(Positioner)
	if !ok {
		return 0
	}
	n, err := p.Position(context.Background(), runID)
	if err != nil {
		return 0
	}
	return n
}

// WithEventBuffer sets how many recent events per run the bus keeps for
// reconnecting clients. The default is [DefaultEventBuffer]. A cursor that
// has fallen further behind is served from the journal, so a smaller buffer
// costs memory, not correctness.
func WithEventBuffer(n int) RunnerOption {
	return func(r *Runner) { r.bus = NewEventBus(n) }
}

// Journal exposes the underlying journal.
func (r *Runner) Journal() Journal { return r.journal }

// Events exposes the run event bus. Transports subscribe to it to stream a
// run; nothing in L1 requires a subscriber.
func (r *Runner) Events() *EventBus { return r.bus }

// StreamEvents yields a run's events after the given cursor, with no gap.
//
// When the cursor still reaches the in-memory backlog, this is the bus and
// nothing else. When it has fallen off the edge — the client was away longer
// than [DefaultEventBuffer] events, or the whole process restarted — the
// journal is replayed first and the live stream is joined afterwards,
// dropping events whose Seq the replay already covered. Every event is
// anchored to a journal position, so the two paths produce the same event at
// the same Seq and the handoff is a filter, not a negotiation.
//
// What replay cannot revive is ephemeral by nature: the deltas and
// progress events forwarded from Kit mid-turn are live-only. A client that
// needs the conversation reads it from the run or the journal; the events
// tell it where the run stands.
//
// The returned function unsubscribes and closes the channel. Call it when
// the client goes away: it also releases the forwarding goroutine, which may
// be parked on a send the client stopped reading.
func (r *Runner) StreamEvents(runID string, after int) (<-chan Event, func()) {
	out := make(chan Event)
	live, unsubscribe := r.bus.Subscribe(runID, after)

	// done releases every send this stream owns. An HTTP client that
	// disconnects between two events leaves the forwarder parked on
	// out <- ev, and the subscriber's pump parked behind it, so a server
	// that streams to the outside world leaked two goroutines and their
	// queued events per disconnect. Closing the channel is what a reader
	// going away has to mean.
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() { close(done) })
		unsubscribe()
	}

	// Journal-backed catch-up only applies when the journal can anchor a
	// position; otherwise this is the live backlog, exactly as before.
	position := r.durableSeq(runID)
	oldest := r.bus.Oldest(runID)
	needReplay := position > after && (oldest == 0 || after+1 < oldest)

	go func() {
		defer close(out)
		emitted := after

		if needReplay {
			switch err := r.replayEvents(runID, after, out, done, &emitted); {
			case errors.Is(err, errStreamClosed):
				// The client left during catch-up. Nothing to serve.
				return
			case err != nil:
				// The journal could not be read; degrade to the live
				// backlog rather than fail a stream that may still
				// deliver everything from here on. Observability, not
				// truth, is what a lost event costs.
				_ = err
			}
		}

		// Without replay, preserve the bus order. Several live Kit events can
		// share one journal anchor, so advancing emitted after the first one
		// would hide every later event at that anchor (usually tool calls).
		// A replay still filters everything it covered because those live-only
		// events cannot be placed correctly in the replayed history.
		for ev := range live {
			if needReplay && ev.Seq <= emitted {
				continue
			}
			if !needReplay && ev.Seq != 0 && ev.Seq <= after {
				continue
			}
			if ev.Seq > emitted {
				emitted = ev.Seq
			}
			if !sendEvent(out, done, ev) {
				return
			}
		}
	}()

	return out, stop
}

// errStreamClosed reports that the reader of a stream went away. It is not a
// fault: it ends the catch-up quietly.
var errStreamClosed = errors.New("bonnie: event stream closed by its reader")

// sendEvent delivers one event unless the stream's reader has gone. It
// reports whether the send happened.
func sendEvent(out chan<- Event, done <-chan struct{}, ev Event) bool {
	select {
	case out <- ev:
		return true
	case <-done:
		return false
	}
}

// replayEvents projects journal records into events and pushes them to out,
// advancing *emitted past every record covered. It stops at the journal's
// current end; the live stream takes over from there. It returns
// [errStreamClosed] when the reader of out went away.
//
// The projection is the durable counterpart of the runner's own publishes:
// a state record becomes an EventState; a suspend record becomes
// EventSuspend with its payload; a resume record becomes EventResume. A
// response is emitted where the runner publishes it — at the end of a
// completed turn — as the last assistant text before the closing state
// record, anchored to the last message record of the turn. Everything else
// in the journal is conversation state, not an event.
func (r *Runner) replayEvents(runID string, after int, out chan<- Event, done <-chan struct{}, emitted *int) error {
	recs, err := r.journal.Replay(context.Background(), runID)
	if err != nil {
		return err
	}

	var (
		lastMsgSeq   int
		lastResponse string
	)
	for i := range recs {
		rec := recs[i]
		switch rec.Kind {
		case RecordMessage:
			if rec.Seq > *emitted {
				lastMsgSeq = rec.Seq
			}
			if string(rec.Role) == "assistant" {
				msg, derr := decodeMessage(rec)
				if derr != nil {
					return derr
				}
				lastResponse = messageText(msg)
			}

		case RecordSuspend:
			if rec.Seq <= *emitted {
				continue
			}
			ev := Event{
				RunID: runID, Type: EventSuspend, Seq: rec.Seq,
				Text: rec.Text, Data: rec.Payload,
			}
			if !sendEvent(out, done, ev) {
				return errStreamClosed
			}
			*emitted = rec.Seq

		case RecordResume:
			if rec.Seq <= *emitted {
				continue
			}
			ev := Event{RunID: runID, Type: EventResume, Seq: rec.Seq, Text: rec.Text}
			if !sendEvent(out, done, ev) {
				return errStreamClosed
			}
			*emitted = rec.Seq

		case RecordState:
			// The response of a completed turn is published just before
			// its closing state, anchored to the last message record —
			// replay reproduces that order and those Seqs exactly.
			if rec.State == RunCompleted && lastResponse != "" && lastMsgSeq > *emitted {
				ev := Event{RunID: runID, Type: EventResponse, Seq: lastMsgSeq, Text: lastResponse}
				if !sendEvent(out, done, ev) {
					return errStreamClosed
				}
				*emitted = lastMsgSeq
			}
			if rec.Seq > *emitted {
				ev := Event{RunID: runID, Type: EventState, Seq: rec.Seq, State: rec.State}
				if !sendEvent(out, done, ev) {
					return errStreamClosed
				}
				*emitted = rec.Seq
			}
			lastResponse = ""
		}
	}
	return nil
}

// Start begins a run, or continues an existing one that is not suspended. If
// runID is already known to the journal, its conversation is replayed first,
// so Start doubles as crash recovery.
//
// The input's title and origin are recorded on the run the first time they
// are seen; its context is journalled and handed to the model for this turn
// only.
func (r *Runner) Start(ctx context.Context, runID string, in Input) (*Run, error) {
	turnCtx, act, err := r.acquire(ctx, runID)
	if err != nil {
		return nil, err
	}
	defer r.release(runID)

	s, err := r.session(turnCtx, runID)
	if err != nil {
		return nil, err
	}
	if err := s.recordTitle(in.Title); err != nil {
		return nil, err
	}
	if err := s.recordOrigin(in.Origin); err != nil {
		return nil, err
	}
	if err := s.journalContext(turnCtx, in.Context); err != nil {
		return nil, err
	}
	s.SetTurnContext(in.Context)
	return r.turn(turnCtx, act, s, in.Text)
}

// Resume delivers input to a suspended run and continues it. The run may have
// been suspended by a different process.
func (r *Runner) Resume(ctx context.Context, runID string, responses []InputResponse) (*Run, error) {
	state, err := r.journal.State(ctx, runID)
	if err != nil {
		return nil, err
	}
	if state != RunWaiting {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotWaiting, runID, state)
	}
	turnCtx, act, err := r.acquire(ctx, runID)
	if err != nil {
		return nil, err
	}
	defer r.release(runID)

	s, err := Restore(turnCtx, runID, r.journal)
	if err != nil {
		return nil, err
	}

	answer := renderResponses(responses)
	seq, err := r.journal.Append(turnCtx, Record{
		RunID: runID, Kind: RecordResume, Timestamp: now(), Text: answer,
	})
	if err != nil {
		return nil, err
	}
	r.bus.Publish(Event{RunID: runID, Type: EventResume, Seq: seq, Text: answer})
	return r.turn(turnCtx, act, s, answer)
}

// Cancel stops the turn a run is executing now. Completed steps stay in the
// journal — Kit persists a step's messages before it checks the context (see
// docs/SPEC.md §3.3) — so a cancelled run restores to a provider-valid
// conversation and can be continued with [Runner.Start].
//
// It returns [ErrRunNotActive] when this Runner is not executing the run.
func (r *Runner) Cancel(runID string) error {
	r.mu.Lock()
	act, ok := r.active[runID]
	r.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrRunNotActive, runID)
	}
	act.cancel()
	return nil
}

// Steer injects a message into a turn that is already running, so the next
// step of the agent loop sees it. This is the cheap half of a steer policy:
// the turn keeps its work instead of restarting.
//
// It returns [ErrRunNotActive] when this Runner is not executing the run.
func (r *Runner) Steer(runID, message string) error {
	r.mu.Lock()
	act, ok := r.active[runID]
	r.mu.Unlock()

	if !ok || !act.steer(message) {
		return fmt.Errorf("%w: %s", ErrRunNotActive, runID)
	}
	return nil
}

// IsActive reports whether this Runner is executing a turn for the run.
func (r *Runner) IsActive(runID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[runID]
	return ok
}

// Snapshot reports what the journal knows about a run without executing
// anything. Transports use it to answer "where is this run now?" — including
// for a run this process never started.
//
// It returns [ErrRunNotFound] when the run is unknown.
func (r *Runner) Snapshot(ctx context.Context, runID string) (*Run, error) {
	state, err := r.journal.State(ctx, runID)
	if err != nil {
		return nil, err
	}
	recs, err := r.journal.Replay(ctx, runID)
	if err != nil {
		return nil, err
	}

	run := &Run{ID: runID, State: state}
	for _, rec := range slices.Backward(recs) {
		if run.Response == "" && rec.Kind == RecordMessage && rec.Role == "assistant" {
			run.Response = rec.Text
		}
		if state == RunWaiting && run.Suspend == nil && rec.Kind == RecordSuspend {
			var sus SuspendRequest
			if err := json.Unmarshal(rec.Payload, &sus); err == nil {
				run.Suspend = &sus
			}
		}
		if run.Response != "" && (state != RunWaiting || run.Suspend != nil) {
			break
		}
	}
	return run, nil
}

// turn runs one agent turn and classifies the outcome.
func (r *Runner) turn(ctx context.Context, act *activeTurn, s *Session, prompt string) (*Run, error) {
	runID := s.runID
	// Terminal bookkeeping must outlive a cancelled turn, or a cancel would
	// leave the run stuck in "running" for ever.
	book := context.WithoutCancel(ctx)

	if err := r.checkpoint(book, runID, RunRunning); err != nil {
		return nil, err
	}

	agent, err := r.factory(ctx, s)
	if err != nil {
		_ = r.checkpoint(book, runID, RunFailed)
		return nil, err
	}
	defer func() { _ = agent.Close() }()
	act.setAgent(agent)

	stop := forwardAgentEvents(agent, r.bus, runID)
	defer stop()

	res, err := agent.PromptResult(ctx, prompt)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			if cerr := r.checkpoint(book, runID, RunCancelled); cerr != nil {
				return nil, cerr
			}
			return &Run{ID: runID, State: RunCancelled}, nil
		}
		_ = r.checkpoint(book, runID, RunFailed)
		return &Run{ID: runID, State: RunFailed, Err: err}, err
	}

	if sus, ok := suspensionFrom(res); ok {
		sseq, aerr := r.journal.Append(book, Record{
			RunID: runID, Kind: RecordSuspend, Timestamp: now(),
			Text: sus.Prompt, Payload: encodeSuspend(sus),
		})
		if aerr != nil {
			return nil, aerr
		}
		susData, _ := json.Marshal(sus)
		r.bus.Publish(Event{
			RunID: runID, Type: EventSuspend, Seq: sseq,
			Text: sus.Prompt, Data: susData,
		})
		if cerr := r.checkpoint(book, runID, RunWaiting); cerr != nil {
			return nil, cerr
		}
		return &Run{
			ID: runID, State: RunWaiting,
			Response: res.Response, Suspend: &sus, Usage: res.TotalUsage,
		}, nil
	}

	// The response anchors to the message record the turn's last text
	// belongs to. The replay projection derives the same event from that
	// record, so a reconnecting client sees the same response at the same
	// Seq whichever path served it.
	r.bus.Publish(Event{
		RunID: runID, Type: EventResponse,
		Seq: s.LastMessageSeq(), Text: res.Response,
	})
	if err := r.checkpoint(book, runID, RunCompleted); err != nil {
		return nil, err
	}
	return &Run{
		ID: runID, State: RunCompleted,
		Response: res.Response, Usage: res.TotalUsage,
	}, nil
}

// checkpoint moves the run state in the journal and announces it. The state
// event anchors to the state record, so a replay lands on the same event.
func (r *Runner) checkpoint(ctx context.Context, runID string, state RunState) error {
	if err := r.journal.Checkpoint(ctx, runID, state); err != nil {
		return err
	}
	r.bus.Publish(Event{RunID: runID, Type: EventState, Seq: r.durableSeq(runID), State: state})
	return nil
}

// session restores an existing run or creates a new one.
func (r *Runner) session(ctx context.Context, runID string) (*Session, error) {
	s, err := Restore(ctx, runID, r.journal)
	if errors.Is(err, ErrRunNotFound) {
		return NewSession(runID, r.journal), nil
	}
	return s, err
}

// acquire claims the run for one turn and returns a context that
// [Runner.Cancel] can stop.
func (r *Runner) acquire(ctx context.Context, runID string) (context.Context, *activeTurn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, busy := r.active[runID]; busy {
		return nil, nil, fmt.Errorf("%w: %s", ErrRunActive, runID)
	}
	turnCtx, cancel := context.WithCancel(ctx)
	act := &activeTurn{cancel: cancel}
	r.active[runID] = act
	return turnCtx, act, nil
}

func (r *Runner) release(runID string) {
	r.mu.Lock()
	act, ok := r.active[runID]
	delete(r.active, runID)
	r.mu.Unlock()

	if ok {
		act.cancel() // release the context's resources
	}
}

func renderResponses(responses []InputResponse) string {
	if len(responses) == 0 {
		return ""
	}
	if len(responses) == 1 {
		return responses[0].Text
	}
	var out strings.Builder
	for i, resp := range responses {
		if i > 0 {
			out.WriteString("\n")
		}
		out.WriteString(resp.Text)
	}
	return out.String()
}
