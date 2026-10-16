package runtime

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// continuingAgent is a fakeAgent that can also continue a saved
// conversation, as *kit.Kit can. It records how each turn was driven.
type continuingAgent struct {
	*fakeAgent
	mu        sync.Mutex
	continued int
	prompts   []string
}

func (a *continuingAgent) PromptResult(ctx context.Context, msg string) (*kit.TurnResult, error) {
	a.mu.Lock()
	a.prompts = append(a.prompts, msg)
	a.mu.Unlock()
	return a.fakeAgent.PromptResult(ctx, msg)
}

func (a *continuingAgent) ContinueResult(context.Context) (*kit.TurnResult, error) {
	a.mu.Lock()
	a.continued++
	a.mu.Unlock()
	if _, err := a.session.AppendMessage(kit.LLMMessage{
		Role:    kit.LLMMessageRole("assistant"),
		Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "continued"}},
	}); err != nil {
		return nil, err
	}
	return &kit.TurnResult{Response: "continued"}, nil
}

func (a *continuingAgent) calls() (continued int, prompts []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.continued, slices.Clone(a.prompts)
}

func continuingFactory() (AgentFactory, *continuingAgent) {
	a := &continuingAgent{fakeAgent: &fakeAgent{turns: []*kit.TurnResult{{Response: "answered"}}}}
	return func(_ context.Context, s *Session) (Agent, error) {
		a.session = s
		return a, nil
	}, a
}

func assistantText(text string) kit.LLMMessage {
	return kit.LLMMessage{Role: kit.LLMMessageRole("assistant"), Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: text}}}
}

// seed writes what a process that died mid-turn leaves behind.
type seed func(t *testing.T, j Journal, s *Session)

func appendRecords(recs ...Record) seed {
	return func(t *testing.T, j Journal, s *Session) {
		t.Helper()
		for _, rec := range recs {
			rec.RunID = s.runID
			if _, err := j.Append(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func appendMessages(msgs ...kit.LLMMessage) seed {
	return func(t *testing.T, _ Journal, s *Session) {
		t.Helper()
		for _, m := range msgs {
			if _, err := s.AppendMessage(m); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func turnStarted(state RunState) seed {
	return appendRecords(Record{Kind: RecordTurn, Text: "turn-1"}, Record{Kind: RecordState, State: state})
}

// A run that a crash or a restart stopped mid-turn continues in a new process
// that shares only the journal, without new input, and does the least work
// that ends the turn. This is the durability claim of Runner.Recover.
func TestRecoverContinuesUnfinishedRunsAcrossProcessBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		seeds     []seed
		state     RunState
		response  string
		continued int
		prompts   []string
		err       error
	}{
		{
			name:      "running with saved input",
			seeds:     []seed{turnStarted(RunRunning), appendMessages(kit.NewLLMUserMessage("do the work"))},
			state:     RunCompleted,
			response:  "continued",
			continued: 1,
		},
		{
			name:      "interrupted with saved input",
			seeds:     []seed{turnStarted(RunRunning), appendMessages(kit.NewLLMUserMessage("do the work")), appendRecords(Record{Kind: RecordState, State: RunInterrupted})},
			state:     RunCompleted,
			response:  "continued",
			continued: 1,
		},
		{
			name:     "final answer saved, state lost",
			seeds:    []seed{turnStarted(RunRunning), appendMessages(kit.NewLLMUserMessage("q"), assistantText("the answer"))},
			state:    RunCompleted,
			response: "the answer",
		},
		{
			name: "suspension saved, waiting state lost",
			seeds: []seed{turnStarted(RunRunning), appendMessages(kit.NewLLMUserMessage("q")),
				appendRecords(Record{Kind: RecordSuspend, Text: "approve?", Payload: encodeSuspend(SuspendRequest{Kind: "approval", Prompt: "approve?"})})},
			state: RunWaiting,
		},
		{
			name: "answer saved, model never saw it",
			seeds: []seed{turnStarted(RunRunning), appendMessages(kit.NewLLMUserMessage("q")),
				appendRecords(
					Record{Kind: RecordSuspend, Text: "approve?", Payload: encodeSuspend(SuspendRequest{Kind: "approval", Prompt: "approve?"})},
					Record{Kind: RecordState, State: RunWaiting},
					Record{Kind: RecordResume, Text: "approved"},
					Record{Kind: RecordTurn, Text: "turn-2"},
					Record{Kind: RecordState, State: RunRunning},
				)},
			state:    RunCompleted,
			response: "answered",
			prompts:  []string{"approved"},
		},
		{
			name:  "input never saved",
			seeds: []seed{turnStarted(RunRunning)},
			state: RunFailed,
			err:   ErrRecoveryInputLost,
		},
		{
			name: "durable cancellation wins",
			seeds: []seed{turnStarted(RunRunning), appendMessages(kit.NewLLMUserMessage("q")),
				appendRecords(Record{Kind: RecordCancel, Text: "turn-1"})},
			state: RunCancelled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			j, reopen := admissionJournal(t, true)
			s := NewSession("r", j)
			for _, apply := range tc.seeds {
				apply(t, j, s)
			}
			// The second process shares only the journal.
			j = reopen()
			f, agent := continuingFactory()
			r := NewRunner(j, f)
			var mu sync.Mutex
			var done []RecoveredRun
			if err := r.Recover(ctx, RecoverOptions{Done: func(run RecoveredRun) {
				mu.Lock()
				done = append(done, run)
				mu.Unlock()
			}}); err != nil {
				t.Fatal(err)
			}
			state, err := j.State(ctx, "r")
			if err != nil || state != tc.state {
				t.Fatalf("state = %s, %v; want %s", state, err, tc.state)
			}
			continued, prompts := agent.calls()
			if continued != tc.continued || !slices.Equal(prompts, tc.prompts) {
				t.Fatalf("continued = %d, prompts = %q; want %d, %q", continued, prompts, tc.continued, tc.prompts)
			}
			if tc.state == RunCancelled {
				// A cancelled run has a stop reason: it is not reported.
				if len(done) != 0 {
					t.Fatalf("done = %+v", done)
				}
				return
			}
			if len(done) != 1 || done[0].ID != "r" || done[0].Run == nil || done[0].Run.State != tc.state || !errors.Is(done[0].Err, tc.err) {
				t.Fatalf("done = %+v", done)
			}
			if done[0].Run.Response != tc.response {
				t.Fatalf("response = %q, want %q", done[0].Run.Response, tc.response)
			}
			if tc.state == RunWaiting && (done[0].Run.Suspend == nil || done[0].Run.Suspend.Prompt != "approve?") {
				t.Fatalf("suspend = %+v", done[0].Run.Suspend)
			}
			// No input is sent twice.
			recs, err := j.Replay(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			users := 0
			for _, rec := range recs {
				if rec.Kind == RecordMessage && rec.Role == "user" {
					users++
				}
			}
			if want := 1 + len(tc.prompts); tc.state != RunFailed && users != want {
				t.Fatalf("user messages = %d, want %d", users, want)
			}
			// A second recovery finds nothing left to do.
			if unfinished, err := r.Unfinished(ctx); err != nil || len(unfinished) != 0 {
				t.Fatalf("unfinished = %v, %v", unfinished, err)
			}
		})
	}
}

// Runs with a stop reason, runs the scheduler owns, reserved runs, and runs
// another owner claims are not continued.
func TestRecoverLeavesOtherRunsAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	for id, state := range map[string]RunState{
		"completed": RunCompleted, "waiting": RunWaiting, "failed": RunFailed,
		"pending": RunPending, ReservedRunPrefix + "x": RunRunning, "owned": RunRunning,
	} {
		if err := j.Checkpoint(ctx, id, state); err != nil {
			t.Fatal(err)
		}
		if _, err := NewSession(id, j).AppendMessage(kit.NewLLMUserMessage("q")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewSession("owned", j).AppendExtensionData(ExtOrigin, `{"channel":"broker"}`); err != nil {
		t.Fatal(err)
	}
	f, agent := continuingFactory()
	r := NewRunner(j, f)
	queued, err := r.Submit(ctx, "queued", "", Input{Text: "q"}, BusyQueue)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Checkpoint(ctx, "queued", RunRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Continue(ctx, "queued"); !errors.Is(err, ErrNothingToContinue) {
		t.Fatalf("Continue on scheduler work = %v", err)
	}
	var skipped []RecoveredRun
	if err := r.Recover(ctx, RecoverOptions{
		Skip: func(run RecoveredRun) bool {
			skipped = append(skipped, run)
			return run.Origin.Channel == "broker"
		},
		Done: func(run RecoveredRun) { t.Errorf("continued %+v", run) },
	}); err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 2 || skipped[0].ID != "owned" || skipped[1].ID != "queued" {
		t.Fatalf("candidates = %+v", skipped)
	}
	if continued, prompts := agent.calls(); continued != 0 || len(prompts) != 0 {
		t.Fatalf("model calls = %d, %q", continued, prompts)
	}
	items, err := r.Submissions(ctx, "queued")
	if err != nil || items[0].ID != queued.ID || items[0].State != SubmissionQueued {
		t.Fatalf("submission = %+v, %v", items, err)
	}
}

// promptFirstAgent saves the user message before it blocks, as Kit does, so
// the input is durable when the turn is interrupted.
type promptFirstAgent struct {
	*continuingAgent
	started chan struct{}
	once    sync.Once
}

func (a *promptFirstAgent) PromptResult(ctx context.Context, msg string) (*kit.TurnResult, error) {
	if _, err := a.session.AppendMessage(kit.NewLLMUserMessage(msg)); err != nil {
		return nil, err
	}
	a.once.Do(func() { close(a.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// A graceful restart is the same as a crash, only cleaner: Drain interrupts
// the turn, refuses new turns, and the next process continues the run.
func TestDrainInterruptsAndNextProcessContinues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j, reopen := admissionJournal(t, true)
	first := &promptFirstAgent{continuingAgent: &continuingAgent{fakeAgent: &fakeAgent{}}, started: make(chan struct{})}
	r := NewRunner(j, func(_ context.Context, s *Session) (Agent, error) { first.session = s; return first, nil })
	result := make(chan *Run, 1)
	go func() {
		run, err := r.Start(ctx, "r", Input{Text: "long task"})
		if err != nil {
			t.Error(err)
		}
		result <- run
	}()
	<-first.started
	drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.Drain(drainCtx); err != nil {
		t.Fatal(err)
	}
	if run := <-result; run == nil || run.State != RunInterrupted {
		t.Fatalf("run = %+v", run)
	}
	if _, err := r.Start(ctx, "other", Input{Text: "late"}); !errors.Is(err, ErrRunnerDraining) {
		t.Fatalf("Start while draining = %v", err)
	}

	j = reopen()
	f, agent := continuingFactory()
	r = NewRunner(j, f)
	if err := r.Recover(ctx, RecoverOptions{}); err != nil {
		t.Fatal(err)
	}
	if state, err := j.State(ctx, "r"); err != nil || state != RunCompleted {
		t.Fatalf("state = %s, %v", state, err)
	}
	if continued, prompts := agent.calls(); continued != 1 || len(prompts) != 0 {
		t.Fatalf("continued = %d, prompts = %q", continued, prompts)
	}
}

// Stopping the scheduler is a shutdown. The submission stays running, and the
// next scheduler finishes it. A cancel here used to abort it for good.
func TestSchedulerShutdownLeavesSubmissionForNextProcess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j, reopen := admissionJournal(t, true)
	f, _, _, started := blockingFactory(&kit.TurnResult{Response: "x"})
	r := NewRunner(j, f)
	item, err := r.Submit(ctx, "sub", "1", Input{Text: "hello"}, BusyQueue)
	if err != nil {
		t.Fatal(err)
	}
	schedCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.RunScheduler(schedCtx) }()
	<-started
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("scheduler = %v", err)
	}
	if state, err := j.State(ctx, "sub"); err != nil || state != RunInterrupted {
		t.Fatalf("state = %s, %v", state, err)
	}
	items, err := r.Submissions(ctx, "sub")
	if err != nil || items[0].State != SubmissionRunning {
		t.Fatalf("submission = %+v, %v", items, err)
	}

	j = reopen()
	f, _ = fakeFactory(&kit.TurnResult{Response: "done"})
	r = NewRunner(j, f)
	startTestScheduler(t, r)
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	settled, err := r.WaitSubmission(wait, "sub", item.ID)
	if err != nil || settled.State != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if state, err := j.State(ctx, "sub"); err != nil || state != RunCompleted {
		t.Fatalf("state = %s, %v", state, err)
	}
}

// An interrupted parent continues after the restart, so it must not cancel
// the children it owns. Only a turn that stops for good does.
func TestInterruptedParentKeepsChildren(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	f, _, _, started := blockingFactory()
	r := NewRunner(j, f)
	result := make(chan *Run, 1)
	go func() {
		run, _ := r.Start(ctx, "parent", Input{Text: "fan out"})
		result <- run
	}()
	<-started
	child, err := r.SpawnChild(ctx, "parent", "k", Input{Text: "work"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Interrupt("parent"); err != nil {
		t.Fatal(err)
	}
	if run := <-result; run == nil || run.State != RunInterrupted {
		t.Fatalf("run = %+v", run)
	}
	items, err := r.Submissions(ctx, child.ID)
	if err != nil || len(items) != 1 || items[0].State != SubmissionQueued {
		t.Fatalf("child = %+v, %v", items, err)
	}
}

func TestInterruptedIsNotTerminal(t *testing.T) {
	t.Parallel()
	if RunInterrupted.IsTerminal() || RunRunning.IsTerminal() || RunWaiting.IsTerminal() {
		t.Fatal("a state without a stop reason is terminal")
	}
}
