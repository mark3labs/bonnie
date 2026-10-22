package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// recordingObserver records what the runner tells it. Its carrier names the
// turn, so a test can see which turn's carrier a later turn received.
type recordingObserver struct {
	mu       sync.Mutex
	starts   []TurnStart
	ends     []TurnEnd
	attached []Agent
	detached int
}

type ctxKey struct{}

func (o *recordingObserver) StartTurn(ctx context.Context, t TurnStart) (context.Context, TurnObservation) {
	o.mu.Lock()
	o.starts = append(o.starts, t)
	o.mu.Unlock()
	return context.WithValue(ctx, ctxKey{}, t.TurnID), &recordingObservation{o: o, turnID: t.TurnID}
}

type recordingObservation struct {
	o      *recordingObserver
	turnID string
}

func (r *recordingObservation) Carrier() map[string]string {
	return map[string]string{"turn": r.turnID}
}

func (r *recordingObservation) Attach(a Agent) func() {
	r.o.mu.Lock()
	r.o.attached = append(r.o.attached, a)
	r.o.mu.Unlock()
	return func() {
		r.o.mu.Lock()
		r.o.detached++
		r.o.mu.Unlock()
	}
}

func (r *recordingObservation) End(e TurnEnd) {
	r.o.mu.Lock()
	r.o.ends = append(r.o.ends, e)
	r.o.mu.Unlock()
}

func (o *recordingObserver) snapshot() ([]TurnStart, []TurnEnd) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]TurnStart(nil), o.starts...), append([]TurnEnd(nil), o.ends...)
}

// ctxAgent records the context its turn ran in.
type ctxAgent struct {
	*fakeAgent
	turnID any
}

func (a *ctxAgent) PromptResult(ctx context.Context, msg string) (*kit.TurnResult, error) {
	a.turnID = ctx.Value(ctxKey{})
	return a.fakeAgent.PromptResult(ctx, msg)
}

func TestObserverFollowsTurnAndJournalsItsCarrier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	agent := &ctxAgent{fakeAgent: &fakeAgent{turns: []*kit.TurnResult{{Response: "hello"}}}}
	obs := &recordingObserver{}
	r := NewRunner(j, func(_ context.Context, s *Session) (Agent, error) {
		agent.session = s
		return agent, nil
	}, WithObserver(obs))

	run, err := r.Start(ctx, "obs-1", Input{Text: "hi", Title: "Greeting", Origin: Origin{Channel: "http"}})
	if err != nil {
		t.Fatal(err)
	}
	starts, ends := obs.snapshot()
	if len(starts) != 1 || len(ends) != 1 {
		t.Fatalf("starts = %d, ends = %d, want 1 each", len(starts), len(ends))
	}
	s := starts[0]
	if s.Kind != TurnNew || s.RunID != "obs-1" || s.TurnID != run.TurnID || s.Prompt != "hi" {
		t.Fatalf("start = %+v, run turn %q", s, run.TurnID)
	}
	if s.Title != "Greeting" || s.Origin.Channel != "http" || s.Previous != nil {
		t.Fatalf("start = %+v", s)
	}
	if ends[0].Run == nil || ends[0].Run.State != RunCompleted || ends[0].Err != nil {
		t.Fatalf("end = %+v", ends[0])
	}
	// The observer's context is the turn's context, so a span it starts is
	// the parent of whatever the agent does.
	if agent.turnID != run.TurnID {
		t.Fatalf("agent ran in context of turn %v, want %q", agent.turnID, run.TurnID)
	}
	if len(obs.attached) != 1 || obs.attached[0] != Agent(agent) || obs.detached != 1 {
		t.Fatalf("attached %d, detached %d", len(obs.attached), obs.detached)
	}

	recs, err := j.Replay(ctx, "obs-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := lastTurnCarrier(recs); got["turn"] != run.TurnID {
		t.Fatalf("journalled carrier = %v, want turn %q", got, run.TurnID)
	}
}

// The trace of an interrupted turn must continue in the process that
// recovers it. The second Runner shares only the journal.
func TestObserverRecoveryAcrossProcessBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()

	blocked := &fakeAgent{started: make(chan struct{}), block: make(chan struct{})}
	obsA := &recordingObserver{}
	runA := NewRunner(j, func(_ context.Context, s *Session) (Agent, error) {
		blocked.session = s
		return blocked, nil
	}, WithObserver(obsA))
	done := make(chan *Run, 1)
	go func() {
		run, _ := runA.Start(ctx, "obs-2", Input{Text: "long job"})
		done <- run
	}()
	<-blocked.started
	if err := runA.Interrupt("obs-2"); err != nil {
		t.Fatal(err)
	}
	var first *Run
	select {
	case first = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt did not stop the turn")
	}
	if _, ends := obsA.snapshot(); len(ends) != 1 || ends[0].Run.State != RunInterrupted {
		t.Fatalf("process A ends = %+v", ends)
	}

	fb, _ := fakeFactory(&kit.TurnResult{Response: "done"})
	obsB := &recordingObserver{}
	runB := NewRunner(j, fb, WithObserver(obsB))
	if _, err := runB.StartRetry(ctx, "obs-2", Input{Text: "long job"}); err != nil {
		t.Fatal(err)
	}
	starts, _ := obsB.snapshot()
	if len(starts) != 1 || starts[0].Kind != TurnRecovery {
		t.Fatalf("process B starts = %+v, want one recovery", starts)
	}
	if starts[0].Previous["turn"] != first.TurnID {
		t.Fatalf("recovery previous = %v, want the interrupted turn %q", starts[0].Previous, first.TurnID)
	}
}

func TestObserverResumeCarriesAnswersAndTheTurnThatWaited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	fa, agentA := fakeFactory(&kit.TurnResult{
		Response:     "May I deploy?",
		FinalValue:   SuspendRequest{Kind: SuspendApproval, Prompt: "May I deploy?", ToolCallID: "tc-1"},
		HaltedByTool: "request_approval",
	})
	agentA.onPrompt = func(s *Session) error {
		if _, err := s.AppendMessage(toolCall("May I deploy?", "tc-1", "request_approval", `{"action":"deploy"}`)); err != nil {
			return err
		}
		_, err := s.AppendMessage(toolResult("tc-1", "Awaiting operator response."))
		return err
	}
	obsA := &recordingObserver{}
	waited, err := NewRunner(j, fa, WithObserver(obsA)).Start(ctx, "obs-3", Input{Text: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ends := obsA.snapshot(); len(ends) != 1 || ends[0].Run.State != RunWaiting || ends[0].Run.Suspend == nil {
		t.Fatalf("waiting end = %+v", ends)
	}

	fb, _ := fakeFactory(&kit.TurnResult{Response: "Deployed."})
	obsB := &recordingObserver{}
	approved := true
	if _, err := NewRunner(j, fb, WithObserver(obsB)).Resume(ctx, "obs-3", []InputResponse{{Approved: &approved, Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	starts, _ := obsB.snapshot()
	if len(starts) != 1 || starts[0].Kind != TurnResume {
		t.Fatalf("resume starts = %+v", starts)
	}
	s := starts[0]
	if s.Previous["turn"] != waited.TurnID {
		t.Fatalf("resume previous = %v, want the turn that waited %q", s.Previous, waited.TurnID)
	}
	if len(s.Responses) != 1 || s.Responses[0].Approved == nil || !*s.Responses[0].Approved {
		t.Fatalf("resume responses = %+v", s.Responses)
	}
}

func TestObserverParentFromInputTrace(t *testing.T) {
	t.Parallel()
	f, _ := fakeFactory(&kit.TurnResult{Response: "ok"})
	obs := &recordingObserver{}
	r := NewRunner(NewMemoryJournal(), f, WithObserver(obs))
	parent := map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}
	if _, err := r.Start(context.Background(), "obs-4", Input{Text: "hi", Trace: parent}); err != nil {
		t.Fatal(err)
	}
	starts, _ := obs.snapshot()
	if len(starts) != 1 || starts[0].Parent["traceparent"] != parent["traceparent"] {
		t.Fatalf("starts = %+v", starts)
	}
}

// A queued submission journals its trace context, so the turn that runs it
// keeps its parent even when another process runs it.
func TestObserverSubmissionKeepsItsTrace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	parent := map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}
	if _, err := NewRunner(j, nil).Submit(ctx, "obs-5", "req-1", Input{Text: "queued", Trace: parent}, BusyQueue); err != nil {
		t.Fatal(err)
	}
	f, _ := fakeFactory(&kit.TurnResult{Response: "ok"})
	obs := &recordingObserver{}
	r := NewRunner(j, f, WithObserver(obs))
	if err := r.deliverSubmission(ctx, "obs-5"); err != nil {
		t.Fatal(err)
	}
	starts, _ := obs.snapshot()
	if len(starts) != 1 || starts[0].Parent["traceparent"] != parent["traceparent"] {
		t.Fatalf("starts = %+v", starts)
	}
}

// Replay, snapshots, and rebuilt streams are not turns: an observer must
// never see them, or a tracer would emit a finished run again.
func TestObserverIgnoresReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, _ := fakeFactory(&kit.TurnResult{Response: "ok"})
	obs := &recordingObserver{}
	r := NewRunner(NewMemoryJournal(), f, WithObserver(obs))
	if _, err := r.Start(ctx, "obs-6", Input{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Snapshot(ctx, "obs-6"); err != nil {
		t.Fatal(err)
	}
	events, stop := r.StreamEvents("obs-6", 0)
	stop()
	for range events {
	}
	if _, err := Restore(ctx, "obs-6", r.Journal()); err != nil {
		t.Fatal(err)
	}
	if starts, ends := obs.snapshot(); len(starts) != 1 || len(ends) != 1 {
		t.Fatalf("starts = %d, ends = %d after replay, want 1 each", len(starts), len(ends))
	}
}

// A journal written before tracing has turn records without a payload. The
// observer must get no carrier from them, not an error.
func TestLastTurnCarrierToleratesOldRecords(t *testing.T) {
	t.Parallel()
	recs := []Record{
		{Kind: RecordTurn, Text: "t1", Payload: encodeTurnPayload(map[string]string{"turn": "t1"})},
		{Kind: RecordTurn, Text: "t2"},
	}
	if got := lastTurnCarrier(recs); got != nil {
		t.Fatalf("carrier = %v, want none for a record without payload", got)
	}
	if got := lastTurnCarrier(recs[:1]); got["turn"] != "t1" {
		t.Fatalf("carrier = %v", got)
	}
	if got := lastTurnCarrier(nil); got != nil {
		t.Fatalf("carrier = %v", got)
	}
}
