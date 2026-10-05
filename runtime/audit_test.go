package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
)

// Journal inputs and replay results must not share payload storage with the journal.
func TestAuditJournalOwnershipAndState(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		ctx := context.Background()
		rec := Record{RunID: "r", Kind: RecordState, State: RunWaiting, Payload: []byte(`{"a":1}`)}
		if _, err := j.Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
		rec.Payload[5] = '2'
		batch := []Record{{RunID: "r", Kind: RecordState, State: RunCompleted, Payload: []byte(`{"a":3}`)}}
		if _, err := j.(StepJournal).AppendStep(ctx, batch); err != nil {
			t.Fatal(err)
		}
		batch[0].Payload[5] = '4'
		recs, err := j.Replay(ctx, "r")
		if err != nil {
			t.Fatal(err)
		}
		if string(recs[0].Payload) != `{"a":1}` || string(recs[1].Payload) != `{"a":3}` {
			t.Fatal("input aliases journal")
		}
		recs[0].Payload[5] = '5'
		again, err := j.Replay(ctx, "r")
		if err != nil {
			t.Fatal(err)
		}
		if string(again[0].Payload) != `{"a":1}` {
			t.Fatal("replay aliases journal")
		}
		state, err := j.State(ctx, "r")
		if err != nil || state != RunCompleted {
			t.Fatalf("state %s: %v", state, err)
		}
	})
}

// A state read must happen after the local run slot is acquired.
// A journal that changes state during the read exposes the old check-then-acquire order.
type auditStateJournal struct {
	*MemoryJournal
	runner   *Runner
	state    RunState
	acquired bool
}

func (j *auditStateJournal) State(ctx context.Context, id string) (RunState, error) {
	j.acquired = j.runner.IsActive(id)
	return j.state, nil
}
func TestAuditStateChecksHoldSlot(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"start", "resume", "clear", "compact"} {
		t.Run(op, func(t *testing.T) {
			j := &auditStateJournal{MemoryJournal: NewMemoryJournal(), state: RunRetired}
			r := NewRunner(j, fakeFactoryOnly())
			j.runner = r
			ctx := context.Background()
			var err error
			switch op {
			case "start":
				_, err = r.Start(ctx, "r", Input{})
			case "resume":
				_, err = r.Resume(ctx, "r", nil)
			case "clear":
				err = r.Clear(ctx, "r")
			case "compact":
				err = r.Compact(ctx, "r")
			}
			if !errors.Is(err, ErrRunRetired) || !j.acquired || r.IsActive("r") {
				t.Fatalf("err=%v acquired=%v", err, j.acquired)
			}
		})
	}
	j := NewMemoryJournal()
	if err := j.Checkpoint(context.Background(), "r", RunWaiting); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(j, fakeFactoryOnly())
	before, _ := j.Position(context.Background(), "r")
	if _, err := r.Start(context.Background(), "r", Input{Text: "bypass", Title: "bad"}); !errors.Is(err, ErrRunWaiting) {
		t.Fatal(err)
	}
	after, _ := j.Position(context.Background(), "r")
	if before != after {
		t.Fatal("refused Start changed journal")
	}
}

// Branch selection and message anchors must survive without a later message append.
func TestAuditBranchAndMessageSeqRestore(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		j := f.open(t)
		ctx := context.Background()
		s := NewSession("r", j)
		a, err := s.AppendMessage(user("a"))
		if err != nil {
			t.Fatal(err)
		}
		seq := s.LastMessageSeq()
		if _, err := s.AppendMessage(assistant("b")); err != nil {
			t.Fatal(err)
		}
		if err := s.Branch(a); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendExtensionData("test", "data"); err != nil {
			t.Fatal(err)
		}
		restored, err := Restore(ctx, "r", f.reopen(t, j))
		if err != nil {
			t.Fatal(err)
		}
		if restored.LastMessageSeq() != seq || len(restored.GetMessages()) != 1 {
			t.Fatal("branch or anchor lost")
		}
		if err := restored.Branch(""); err != nil {
			t.Fatal(err)
		}
		empty, err := Restore(ctx, "r", restored.journal)
		if err != nil {
			t.Fatal(err)
		}
		if empty.LastMessageSeq() != 0 || len(empty.GetMessages()) != 0 {
			t.Fatal("root branch lost")
		}
	})
}

// Real Kit file input must reach the model and survive a second Runner.
func TestAuditFilesReachKitAndReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	model := fakemodel.New(fakemodel.Say("seen"), fakemodel.Say("still seen"))
	file := kit.LLMFilePart{MediaType: "image/png", Data: []byte("image")}
	if _, err := NewRunner(j, scriptedKit(model)).Start(ctx, "r", Input{Text: "look", Files: []kit.LLMFilePart{file}}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRunner(j, scriptedKit(model)).Start(ctx, "r", Input{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	for _, req := range model.Requests() {
		found := false
		for _, m := range req.Messages {
			for _, p := range m.Content {
				if got, ok := p.(kit.LLMFilePart); ok && reflect.DeepEqual(got.Data, file.Data) {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("file missing from model request")
		}
	}
	if _, err := NewRunner(nil, fakeFactoryOnly()).Start(ctx, "unsupported", Input{Files: []kit.LLMFilePart{file}}); !errors.Is(err, ErrFilesUnsupported) {
		t.Fatalf("file refusal: %v", err)
	}
}

// New Runners must use the clear and compaction markers, not the old window.
func TestAuditControlsAcrossRunnerBoundary(t *testing.T) {
	t.Parallel()
	eachJournal(t, func(t *testing.T, f journalFactory) {
		for _, op := range []string{"clear", "compact"} {
			j := f.open(t)
			ctx := context.Background()
			model := fakemodel.New(fakemodel.Say("old answer"))
			if _, err := NewRunner(j, scriptedKit(model)).Start(ctx, op, Input{Text: "old question"}); err != nil {
				t.Fatal(err)
			}
			r := NewRunner(j, func(_ context.Context, s *Session) (Agent, error) {
				a := &compactingAgent{}
				a.session = s
				return a, nil
			})
			var err error
			if op == "clear" {
				err = r.Clear(ctx, op)
			} else {
				err = r.Compact(ctx, op)
			}
			if err != nil {
				t.Fatal(err)
			}
			j = f.reopen(t, j)
			next := fakemodel.New(fakemodel.Say("new answer"))
			if _, err := NewRunner(j, scriptedKit(next)).Start(ctx, op, Input{Text: "new question"}); err != nil {
				t.Fatal(err)
			}
			text := next.Requests()[0].Text()
			if strings.Contains(text, "old question") || strings.Contains(text, "old answer") {
				t.Fatal("old window reached model")
			}
			if op == "compact" && !strings.Contains(text, "summary of everything") {
				t.Fatal("summary missing")
			}
		}
	})
}

// Pause replay after its snapshot while a real turn publishes to the subscribed bus.
type auditReplayJournal struct {
	*MemoryJournal
	once             atomic.Bool
	entered, release chan struct{}
}

func (j *auditReplayJournal) Replay(ctx context.Context, id string) ([]Record, error) {
	recs, err := j.MemoryJournal.Replay(ctx, id)
	if j.once.CompareAndSwap(false, true) {
		close(j.entered)
		<-j.release
	}
	return recs, err
}
func TestAuditRealKitReplayHandoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := NewMemoryJournal()
	if _, err := NewRunner(base, scriptedKit(fakemodel.New(fakemodel.Say("first")))).Start(ctx, "r", Input{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	j := &auditReplayJournal{MemoryJournal: base, entered: make(chan struct{}), release: make(chan struct{})}
	r := NewRunner(j, scriptedKit(fakemodel.New(fakemodel.Say("second"))))
	events, stop := r.StreamEvents("r", 0)
	defer stop()
	<-j.entered
	if _, err := r.Start(ctx, "r", Input{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	close(j.release)
	counts := map[string]int{}
	deadline := time.After(5 * time.Second)
	for counts["completed"] < 2 {
		select {
		case ev := <-events:
			if ev.Type == EventResponse {
				counts[ev.Text]++
			}
			if ev.Type == EventState && ev.State == RunCompleted {
				counts["completed"]++
			}
		case <-deadline:
			t.Fatalf("handoff timed out: %v", counts)
		}
	}
	if counts["first"] != 1 || counts["second"] != 1 {
		t.Fatalf("handoff counts: %v", counts)
	}
}

// A failed atomic loss write must be retryable; old marker-only losses need recovery.
type auditFailStepJournal struct {
	*MemoryJournal
	fail bool
}

func (j *auditFailStepJournal) AppendStep(ctx context.Context, recs []Record) ([]int, error) {
	if j.fail {
		return nil, errors.New("write failed")
	}
	return j.MemoryJournal.AppendStep(ctx, recs)
}
func TestAuditSandboxLossRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := &auditFailStepJournal{MemoryJournal: NewMemoryJournal(), fail: true}
	s := NewSession("r", j)
	if err := s.RecordSandboxOpen(ctx, "docker", "id"); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteSandboxUnavailable(ctx, "docker", "id", true); err == nil {
		t.Fatal("expected failure")
	}
	if _, _, gone, _ := s.LastSandbox(); gone || len(s.GetMessages()) != 0 {
		t.Fatal("failed write changed live state")
	}
	j.fail = false
	if err := s.NoteSandboxUnavailable(ctx, "docker", "id", true); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(ctx, "r", j)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.NoteSandboxUnavailable(ctx, "docker", "id", true); err != nil {
		t.Fatal(err)
	}
	if len(restored.GetMessages()) != 1 {
		t.Fatal("loss note missing or repeated")
	}

	eachJournal(t, func(t *testing.T, f journalFactory) {
		store := f.open(t)
		legacy := NewSession("legacy", store)
		if err := legacy.RecordSandboxOpen(ctx, "docker", "lost"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Append(ctx, Record{RunID: "legacy", Kind: RecordSandbox, Payload: encodeSandbox(sandboxPayload{Backend: "docker", SandboxID: "lost", Gone: true})}); err != nil {
			t.Fatal(err)
		}
		store = f.reopen(t, store)
		model := fakemodel.New(fakemodel.Say("recovered"))
		if _, err := NewRunner(store, scriptedKit(model)).Start(ctx, "legacy", Input{Text: "continue"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(model.Requests()[0].Text(), "files from earlier turns are gone") {
			t.Fatal("second Runner did not recover loss note")
		}
		again, err := Restore(ctx, "legacy", store)
		if err != nil {
			t.Fatal(err)
		}
		notes := 0
		for _, msg := range again.GetMessages() {
			if strings.HasPrefix(messageText(msg), "[bonnie] The sandbox workspace") {
				notes++
			}
		}
		if notes != 1 {
			t.Fatalf("notes = %d", notes)
		}
	})
}

// Cancel during a real Kit tool must keep the completed call/result pair.
func TestAuditKitCancellationKeepsCompletedWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	entered := make(chan struct{})
	finish := make(chan struct{})
	tool := kit.NewTool("work", "Complete work", func(ctx context.Context, _ struct{}) (kit.ToolOutput, error) {
		close(entered)
		<-ctx.Done()
		<-finish
		return kit.TextResult("finished work"), nil
	})
	model := fakemodel.New(fakemodel.Call("work", `{}`))
	r := NewRunner(j, scriptedKit(model, kit.WithExtraTools(tool)))
	type result struct {
		run *Run
		err error
	}
	done := make(chan result, 1)
	go func() { run, err := r.Start(ctx, "r", Input{Text: "work"}); done <- result{run, err} }()
	<-entered
	if err := r.Cancel("r"); err != nil {
		t.Fatal(err)
	}
	close(finish)
	select {
	case got := <-done:
		if got.err != nil || got.run.State != RunCancelled {
			t.Fatalf("cancel result: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel timed out")
	}
	next := fakemodel.New(fakemodel.Say("continued"))
	if _, err := NewRunner(j, scriptedKit(next)).Start(ctx, "r", Input{Text: "continue"}); err != nil {
		t.Fatal(err)
	}
	calls, results := map[string]bool{}, map[string]bool{}
	for _, msg := range next.Requests()[0].Messages {
		for _, id := range toolCallIDs(msg) {
			calls[id] = true
		}
		for _, id := range toolResultIDs(msg) {
			results[id] = true
		}
	}
	if len(calls) != 1 || !reflect.DeepEqual(calls, results) {
		t.Fatalf("completed step lost: calls=%v results=%v", calls, results)
	}
}
