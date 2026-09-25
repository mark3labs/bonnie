package runtime

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// This file drives BONNIE's executor with a REAL *kit.Kit whose model answers
// from a script (internal/fakemodel). The rest of the package uses fakeAgent,
// which never runs Kit, so these are the only standard tests that prove the
// four seams in runner.go do what the package doc claims: the step hook, the
// atomic step append, the context-prepare hook, and a halting tool. Before
// this file they were proven only by the live tests behind the integration
// tag, which cost money and skip on a machine with no key.

// hermetic keeps a real Kit from reading the machine it runs on: no
// ~/.kit.yml, no AGENTS.md, no skills, extensions or agent definitions, and
// no core tools. Without it a developer's own configuration — an MCP server
// in ~/.kit.yml, say — becomes part of the test.
func hermetic() kit.Option {
	return func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.DisableCoreTools = true
		o.Quiet = true
	}
}

// scriptedKit is [KitAgent] with a scripted model, built as a host builds it.
func scriptedKit(m *fakemodel.Model, opts ...kit.Option) AgentFactory {
	return KitAgent(append([]kit.Option{hermetic(), m.Option()}, opts...)...)
}

// stepLog is a journal that records each AppendStep batch, so a test can see
// how a real Kit step reached the journal.
type stepLog struct {
	*MemoryJournal
	mu      sync.Mutex
	batches [][]Record
}

func (l *stepLog) AppendStep(ctx context.Context, recs []Record) ([]int, error) {
	l.mu.Lock()
	l.batches = append(l.batches, slices.Clone(recs))
	l.mu.Unlock()
	return l.MemoryJournal.AppendStep(ctx, recs)
}

func (l *stepLog) steps() [][]Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.batches)
}

var _ StepJournal = (*stepLog)(nil)

// TestKitSuspendAndResumeAcrossProcessBoundary is the offline twin of
// TestLiveSuspendAndResume: a real Kit parks on ask_human, and a second
// Runner that shares only the reopened SQLite journal resumes it.
//
// The resumed request is the evidence. It is what the second process really
// sends to the provider, so the tool call and its result must both be in it,
// with the same ID — a replay that dropped either would be rejected by every
// provider, and fakeAgent cannot show that because it never builds a request.
//
// The halt ends the turn: the model is asked once in process A, not again
// after ask_human returns (kit v0.113.3, mark3labs/kit#147).
func TestKitSuspendAndResumeAcrossProcessBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	model := fakemodel.New(
		fakemodel.Call("ask_human", `{"question":"Which region?"}`),
		fakemodel.Say("Deploying to eu-west-1."),
	)

	// Process A: the run parks.
	journalA, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteJournal: %v", err)
	}
	runA, err := NewRunner(journalA, scriptedKit(model)).Start(ctx, "kit-1", Input{Text: "Deploy the app."})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if runA.State != RunWaiting {
		t.Fatalf("state = %q, want %q", runA.State, RunWaiting)
	}
	if runA.Suspend == nil || runA.Suspend.Prompt != "Which region?" || runA.Suspend.ToolCallID == "" {
		t.Fatalf("suspend = %+v, want the question with its tool call ID", runA.Suspend)
	}
	if n := len(model.Requests()); n != 1 {
		t.Fatalf("process A asked the model %d times, want 1: the loop went on after ask_human", n)
	}
	if err := journalA.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Process B: nothing is shared but the files on disk.
	journalB, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = journalB.Close() })
	runB, err := NewRunner(journalB, scriptedKit(model)).Resume(ctx, "kit-1", []InputResponse{{Text: "eu-west-1"}})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if runB.State != RunCompleted || runB.Response != "Deploying to eu-west-1." {
		t.Fatalf("resumed run = %q %q, want completed with the scripted reply", runB.State, runB.Response)
	}

	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("the model was asked %d times, want 2", len(reqs))
	}
	resumed := reqs[1]
	if !resumed.HasTool("ask_human") {
		t.Fatal("the resumed turn offers no ask_human: the halt leaked into the next turn")
	}
	var calls, results []string
	for _, m := range resumed.Messages {
		calls = append(calls, toolCallIDs(m)...)
		results = append(results, toolResultIDs(m)...)
	}
	want := []string{runA.Suspend.ToolCallID}
	if !slices.Equal(calls, want) || !slices.Equal(results, want) {
		t.Fatalf("resumed request has tool calls %v and results %v, want %v for both: "+
			"the replay lost half of the halted step", calls, results, want)
	}
	if !strings.Contains(resumed.Text(), "eu-west-1") {
		t.Fatal("the operator's answer did not reach the resumed request")
	}
	assertNoOrphan(t, resumed.Messages)
}

// TestKitToolStepIsOneJournalWriteAndNeverRepeats: a real Kit hands a
// tool-calling step to Session.AppendStep (kit.StepAppender), so the call and
// its result reach the journal in ONE write. And a later turn replays that
// step instead of running the tool again, which is the claim "a side effect
// is not repeated".
func TestKitToolStepIsOneJournalWriteAndNeverRepeats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := &stepLog{MemoryJournal: NewMemoryJournal()}

	var ran atomic.Int32
	deploy := kit.NewTool("deploy", "Deploy the app.",
		func(context.Context, struct{}) (kit.ToolOutput, error) {
			ran.Add(1)
			return kit.ToolOutput{Content: "deployed"}, nil
		})
	model := fakemodel.New(
		fakemodel.Call("deploy", `{}`),
		fakemodel.Say("Done."),
		fakemodel.Say("Still done."),
	)
	factory := scriptedKit(model, kit.WithExtraTools(deploy))

	run, err := NewRunner(journal, factory).Start(ctx, "kit-2", Input{Text: "Deploy."})
	if err != nil || run.State != RunCompleted {
		t.Fatalf("Start = %+v, %v; want completed", run, err)
	}
	if ran.Load() != 1 {
		t.Fatalf("the tool ran %d times, want 1", ran.Load())
	}

	var atomicStep bool
	for _, batch := range journal.steps() {
		var calls, results int
		for _, rec := range batch {
			msg, err := decodeMessage(rec)
			if err != nil {
				continue
			}
			calls += len(toolCallIDs(msg))
			results += len(toolResultIDs(msg))
		}
		if calls == 1 && results == 1 {
			atomicStep = true
		}
	}
	if !atomicStep {
		t.Fatalf("no AppendStep batch held the tool call with its result (batches: %d): "+
			"the step reached the journal in pieces, which leaves a crash window", len(journal.steps()))
	}

	// A new Runner continues the run. The step is history now: the model
	// sees it, and the tool does not run again.
	if _, err := NewRunner(journal, factory).Start(ctx, "kit-2", Input{Text: "Status?"}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if ran.Load() != 1 {
		t.Fatalf("the tool ran %d times after the replay, want 1: a side effect was repeated", ran.Load())
	}
	last := model.Requests()[len(model.Requests())-1]
	if !strings.Contains(last.Text(), "Deploy.") {
		t.Fatal("the continued turn did not replay the earlier conversation")
	}
	if model.Remaining() != 0 {
		t.Fatalf("%d scripted replies unused", model.Remaining())
	}
}

// TestKitContextReachesTheModelNotTheConversation: the context-prepare hook
// shows a turn's context to the model for that turn only. It is in the
// request, it is not in the replayed conversation, and the next turn does not
// see it.
func TestKitContextReachesTheModelNotTheConversation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := NewMemoryJournal()
	model := fakemodel.New(fakemodel.Say("Seen."), fakemodel.Say("Again."))
	factory := scriptedKit(model)

	const event = "event: push to main by octocat"
	if _, err := NewRunner(journal, factory).Start(ctx, "kit-3", Input{
		Text: "What happened?", Context: []string{event},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := NewRunner(journal, factory).Start(ctx, "kit-3", Input{Text: "And now?"}); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("the model was asked %d times, want 2", len(reqs))
	}
	if !strings.Contains(reqs[0].Text(), event) {
		t.Fatal("the turn's context did not reach the model")
	}
	if strings.Contains(reqs[1].Text(), event) {
		t.Fatal("the context of turn 1 reached turn 2: it became conversation history")
	}

	restored, err := Restore(ctx, "kit-3", journal)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, m := range restored.GetMessages() {
		if strings.Contains(messageText(m), event) {
			t.Fatal("the context was journalled as a conversation message")
		}
	}
}

// TestKitApprovalBlocksTheActionUntilAnswered is the human-in-the-loop claim
// made precise: once the model asks for approval, nothing else runs in that
// turn. The action runs only after the operator approves, in the resumed turn.
//
// Before kit v0.113.3 Kit did not end the loop on Halt (mark3labs/kit#147):
// the model was asked again with every tool, and one that went ahead
// deployed before anyone approved. BONNIE carried a hook-based guard until
// the fix landed; this test is what proved both the defect and the fix.
func TestKitApprovalBlocksTheActionUntilAnswered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	journal := NewMemoryJournal()

	var ran atomic.Int32
	deploy := kit.NewTool("deploy", "Deploy the app.",
		func(context.Context, struct{}) (kit.ToolOutput, error) {
			ran.Add(1)
			return kit.ToolOutput{Content: "deployed"}, nil
		})
	model := fakemodel.New(
		fakemodel.Call("request_approval", `{"action":"deploy to production"}`),
		// A model that does not wait: it would go ahead if it were asked
		// again in the same turn.
		fakemodel.Call("deploy", `{}`),
	)
	factory := scriptedKit(model, kit.WithExtraTools(deploy))

	run, err := NewRunner(journal, factory).Start(ctx, "kit-4", Input{Text: "Deploy to production."})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.State != RunWaiting {
		t.Fatalf("state = %q, want %q", run.State, RunWaiting)
	}
	if ran.Load() != 0 {
		t.Fatal("the action ran before the operator answered the approval request")
	}
	if n := len(model.Requests()); n != 1 {
		t.Fatalf("the model was asked %d times in the approval turn, want 1: "+
			"the loop went on after request_approval", n)
	}

	// The operator approves. The action may run now, once.
	model2 := fakemodel.New(fakemodel.Call("deploy", `{}`), fakemodel.Say("Deployed."))
	run, err = NewRunner(journal, scriptedKit(model2, kit.WithExtraTools(deploy))).
		Resume(ctx, "kit-4", []InputResponse{Approve("")})
	if err != nil || run.State != RunCompleted {
		t.Fatalf("Resume = %+v, %v; want completed", run, err)
	}
	if ran.Load() != 1 {
		t.Fatalf("the approved action ran %d times, want 1", ran.Load())
	}
}
