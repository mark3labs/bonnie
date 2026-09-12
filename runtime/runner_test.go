package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// fakeAgent stands in for a live model so the durable executor can be tested
// without credentials or network.
//
// A non-nil block channel makes the turn hang until the channel is closed or
// the context is cancelled, which is the only way cancellation is observable
// in a test.
type fakeAgent struct {
	turns   []*kit.TurnResult
	call    int
	session *Session
	// onPrompt lets a test write the messages a real turn would have
	// journalled, such as the tool call and result of a halting tool.
	onPrompt func(*Session) error

	mu      sync.Mutex
	steers  []string
	block   chan struct{}
	started chan struct{}
}

func (f *fakeAgent) PromptResult(ctx context.Context, msg string) (*kit.TurnResult, error) {
	if f.started != nil {
		close(f.started)
		f.started = nil
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.session != nil {
		if _, err := f.session.AppendMessage(kit.NewLLMUserMessage(msg)); err != nil {
			return nil, err
		}
	}
	if f.call >= len(f.turns) {
		return &kit.TurnResult{Response: "done"}, nil
	}
	res := f.turns[f.call]
	f.call++
	if f.session != nil && res.Response != "" {
		assistant := kit.LLMMessage{
			Role:    kit.LLMMessageRole("assistant"),
			Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: res.Response}},
		}
		if _, err := f.session.AppendMessage(assistant); err != nil {
			return nil, err
		}
	}
	if f.onPrompt != nil && f.session != nil {
		if err := f.onPrompt(f.session); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (f *fakeAgent) InjectSteer(msg string) {
	f.mu.Lock()
	f.steers = append(f.steers, msg)
	f.mu.Unlock()
}

// steered returns a copy of the injected messages, so a test can read them
// while the turn is still running.
func (f *fakeAgent) steered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.steers...)
}

func (f *fakeAgent) Close() error { return nil }

func fakeFactory(turns ...*kit.TurnResult) (AgentFactory, *fakeAgent) {
	agent := &fakeAgent{turns: turns}
	return func(_ context.Context, s *Session) (Agent, error) {
		agent.session = s
		return agent, nil
	}, agent
}

func TestRunCompletes(t *testing.T) {
	t.Parallel()
	f, _ := fakeFactory(&kit.TurnResult{Response: "hello"})
	r := NewRunner(NewMemoryJournal(), f)

	run, err := r.Start(context.Background(), "run-1", Input{Text: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.State != RunCompleted {
		t.Fatalf("state = %q, want %q", run.State, RunCompleted)
	}
	if run.Response != "hello" {
		t.Fatalf("response = %q, want %q", run.Response, "hello")
	}
}

func TestRunSuspendsOnHaltingTool(t *testing.T) {
	t.Parallel()
	want := SuspendRequest{Kind: SuspendQuestion, Prompt: "Which region?"}
	f, _ := fakeFactory(&kit.TurnResult{
		Response:     "I need more information.",
		FinalValue:   want,
		HaltedByTool: "ask_human",
	})
	r := NewRunner(NewMemoryJournal(), f)

	run, err := r.Start(context.Background(), "run-2", Input{Text: "deploy"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.State != RunWaiting {
		t.Fatalf("state = %q, want %q", run.State, RunWaiting)
	}
	if run.Suspend == nil || run.Suspend.Prompt != want.Prompt {
		t.Fatalf("suspend = %+v, want prompt %q", run.Suspend, want.Prompt)
	}
}

// TestResumeAcrossProcessBoundary is the core durability claim: a run that
// suspends can be continued by a completely new Runner with a new agent,
// sharing only the journal.
func TestResumeAcrossProcessBoundary(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()
	ctx := context.Background()

	// "Process A" suspends the run on a halting tool, which is what a real
	// ask_human turn leaves in the journal: an assistant message carrying the
	// call, and the tool message carrying its result.
	fa, agentA := fakeFactory(&kit.TurnResult{
		Response:     "Which region?",
		FinalValue:   SuspendRequest{Kind: SuspendQuestion, Prompt: "Which region?", ToolCallID: "tc-1"},
		HaltedByTool: "ask_human",
	})
	runA := NewRunner(journal, fa)
	agentA.onPrompt = func(s *Session) error {
		if _, err := s.AppendMessage(toolCall("Which region?", "tc-1", "ask_human", `{"question":"Which region?"}`)); err != nil {
			return err
		}
		_, err := s.AppendMessage(toolResult("tc-1", "Awaiting operator response."))
		return err
	}
	if _, err := runA.Start(ctx, "run-3", Input{Text: "deploy the app"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// "Process B" — new runner, new agent, same journal.
	fb, agentB := fakeFactory(&kit.TurnResult{Response: "Deployed to eu-west-1."})
	runB := NewRunner(journal, fb)

	run, err := runB.Resume(ctx, "run-3", []InputResponse{{Text: "eu-west-1"}})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if run.State != RunCompleted {
		t.Fatalf("state = %q, want %q", run.State, RunCompleted)
	}
	if run.Response != "Deployed to eu-west-1." {
		t.Fatalf("response = %q", run.Response)
	}

	// The replayed session must carry the pre-suspension conversation,
	// including the tool call that parked the run. A replay that keeps only
	// text would let the resumed agent call the tool a second time.
	msgs := agentB.session.GetMessages()
	if len(msgs) < 2 {
		t.Fatalf("replayed %d messages, want the pre-suspension history", len(msgs))
	}
	if got := messageText(msgs[0]); got != "deploy the app" {
		t.Fatalf("first replayed message = %q, want %q", got, "deploy the app")
	}
	assertNoOrphan(t, msgs)

	var calls, results int
	for _, m := range msgs {
		calls += len(toolCallIDs(m))
		results += len(toolResultIDs(m))
	}
	if calls != 1 || results != 1 {
		t.Fatalf("replayed %d tool calls and %d results, want 1 of each", calls, results)
	}
}

func TestResumeRejectsRunningRun(t *testing.T) {
	t.Parallel()
	f, _ := fakeFactory(&kit.TurnResult{Response: "ok"})
	r := NewRunner(NewMemoryJournal(), f)
	ctx := context.Background()

	if _, err := r.Start(ctx, "run-4", Input{Text: "hi"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err := r.Resume(ctx, "run-4", []InputResponse{{Text: "x"}})
	if !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("err = %v, want ErrNotWaiting", err)
	}
}

func TestUnknownRunIsNotFound(t *testing.T) {
	t.Parallel()
	r := NewRunner(NewMemoryJournal(), nil)
	_, err := r.Resume(context.Background(), "nope", nil)
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

func TestSuspensionFromPointer(t *testing.T) {
	t.Parallel()
	want := SuspendRequest{Kind: SuspendApproval, Prompt: "rm -rf /"}
	got, ok := suspensionFrom(&kit.TurnResult{FinalValue: &want})
	if !ok || got.Prompt != want.Prompt {
		t.Fatalf("suspensionFrom(ptr) = %+v, %v", got, ok)
	}
	if _, ok := suspensionFrom(&kit.TurnResult{Response: "plain"}); ok {
		t.Fatal("plain turn must not report a suspension")
	}
}
