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
//   - Kit.OnContextPrepare, to inject a replayed context window on resume.
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

	// On resume, the replayed session already holds the full conversation, so
	// the default context window is correct. The hook is registered anyway as
	// the documented injection point for custom replay policy.
	k.OnContextPrepare(kit.HookPriorityHigh,
		func(h kit.ContextPrepareHook) *kit.ContextPrepareResult {
			return nil // nil keeps Kit's assembled context unchanged
		})
}

// Input starts or continues a run.
type Input struct {
	Text  string
	Files []kit.LLMFilePart
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

// NewRunner returns a runner. A nil journal defaults to [MemoryJournal]; a nil
// factory defaults to [KitAgent] with no extra options.
func NewRunner(j Journal, f AgentFactory) *Runner {
	if j == nil {
		j = NewMemoryJournal()
	}
	if f == nil {
		f = KitAgent()
	}
	return &Runner{
		journal: j,
		factory: f,
		bus:     NewEventBus(DefaultEventBuffer),
		active:  make(map[string]*activeTurn),
	}
}

// Journal exposes the underlying journal.
func (r *Runner) Journal() Journal { return r.journal }

// Events exposes the run event bus. Transports subscribe to it to stream a
// run; nothing in L1 requires a subscriber.
func (r *Runner) Events() *EventBus { return r.bus }

// Start begins a run, or continues an existing one that is not suspended. If
// runID is already known to the journal, its conversation is replayed first,
// so Start doubles as crash recovery.
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
	if _, err := r.journal.Append(turnCtx, Record{
		RunID: runID, Kind: RecordResume, Timestamp: now(), Text: answer,
	}); err != nil {
		return nil, err
	}
	r.bus.PublishData(runID, EventResume, answer, responses)
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
		if _, aerr := r.journal.Append(book, Record{
			RunID: runID, Kind: RecordSuspend, Timestamp: now(),
			Text: sus.Prompt, Payload: encodeSuspend(sus),
		}); aerr != nil {
			return nil, aerr
		}
		r.bus.PublishData(runID, EventSuspend, sus.Prompt, sus)
		if cerr := r.checkpoint(book, runID, RunWaiting); cerr != nil {
			return nil, cerr
		}
		return &Run{
			ID: runID, State: RunWaiting,
			Response: res.Response, Suspend: &sus, Usage: res.TotalUsage,
		}, nil
	}

	r.bus.PublishData(runID, EventResponse, res.Response, nil)
	if err := r.checkpoint(book, runID, RunCompleted); err != nil {
		return nil, err
	}
	return &Run{
		ID: runID, State: RunCompleted,
		Response: res.Response, Usage: res.TotalUsage,
	}, nil
}

// checkpoint moves the run state in the journal and announces it.
func (r *Runner) checkpoint(ctx context.Context, runID string, state RunState) error {
	if err := r.journal.Checkpoint(ctx, runID, state); err != nil {
		return err
	}
	r.bus.Publish(Event{RunID: runID, Type: EventState, State: state})
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
