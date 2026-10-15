package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Each restart owns only the journal, including a closed and reopened database.
func admissionJournal(t *testing.T, disk bool) (Journal, func() Journal) {
	t.Helper()
	if !disk {
		j := NewMemoryJournal()
		return j, func() Journal { return j }
	}
	dir := t.TempDir()
	j, err := OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	return j, func() Journal {
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		j, err = OpenSQLiteJournal(dir)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
}

func assertDeferred(t *testing.T, r *Runner, j Journal, in Input) {
	t.Helper()
	ctx := context.Background()
	before, err := j.Replay(ctx, "admission")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		run, err := r.Start(ctx, "admission", in)
		if run != nil || !errors.Is(err, ErrInputDeferred) {
			t.Fatalf("Start = %+v, %v", run, err)
		}
		if in.Text != "" && strings.Contains(err.Error(), in.Text) {
			t.Fatalf("error exposes input: %v", err)
		}
		after, err := j.Replay(ctx, "admission")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("refusal changed journal")
		}
	}
}

func TestInputAdmissionPhases(t *testing.T) {
	t.Parallel()
	inputs := []Input{
		{Text: "original secret"}, {Text: "NEW CHAT MESSAGE"}, {Text: " \t\n"},
		{Files: []kit.LLMFilePart{{MediaType: "text/plain"}}}, {Context: []string{"context"}},
		{Title: "title"}, {Origin: Origin{Channel: "chat"}}, {Origin: Origin{Kind: "dm"}}, {Trigger: &Trigger{}},
	}
	for _, disk := range []bool{false, true} {
		for _, state := range []RunState{RunPending, RunRunning, RunInterrupted} {
			for _, phase := range []string{"pending", "check", "continue", "accepted", ""} {
				if phase == "" && state != RunInterrupted {
					continue
				}
				t.Run(string(state)+"/"+phase+map[bool]string{false: "/memory", true: "/sqlite"}[disk], func(t *testing.T) {
					t.Parallel()
					ctx := context.Background()
					j, reopen := admissionJournal(t, disk)
					seed := NewRunner(j, nil)
					if err := seed.checkpoint(ctx, "admission", state); err != nil {
						t.Fatal(err)
					}
					if phase != "" {
						c := &completionRecord{Phase: phase, Prompt: "original secret", Candidate: CompletionCandidate{Response: "candidate", ContinuationsUsed: 2}, Feedback: CompletionFeedback{ContinueWith: "feedback"}, MessageSeq: 17, Usage: &kit.LLMUsage{TotalTokens: 23}}
						if err := seed.saveCompletion(ctx, NewSession("admission", j), c); err != nil {
							t.Fatal(err)
						}
					}
					j = reopen()
					r := NewRunner(j, func(context.Context, *Session) (Agent, error) {
						t.Fatal("agent constructed during refusal")
						return nil, nil
					})
					events, stop := r.Events().Subscribe("admission", 0)
					defer stop()
					for _, in := range inputs {
						assertDeferred(t, r, j, in)
					}
					select {
					case ev := <-events:
						t.Fatalf("refusal published event: %+v", ev)
					default:
					}
				})
			}
		}
	}
}

// Public reproduction: interrupt a blocking check, reject a new command, recover
// the old candidate, then deliver the retained command with a fresh budget.
func TestInputAdmissionInterruptedHook(t *testing.T) {
	t.Parallel()
	for _, disk := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "sqlite"}[disk], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			j, reopen := admissionJournal(t, disk)
			f, _ := fakeFactory(&kit.TurnResult{Response: "draft", TotalUsage: &kit.LLMUsage{TotalTokens: 7}})
			started := make(chan struct{})
			r := NewRunner(j, f, WithCompletionHook(func(ctx context.Context, _ CompletionCandidate) (CompletionFeedback, error) {
				close(started)
				<-ctx.Done()
				return CompletionFeedback{}, ctx.Err()
			}, 1))
			done := make(chan *Run, 1)
			go func() {
				run, err := r.Start(ctx, "admission", Input{Text: "original secret", Context: []string{"original context"}})
				if err != nil {
					t.Error(err)
				}
				done <- run
			}()
			defer func() { _ = r.Interrupt("admission") }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("check did not start")
			}
			if err := r.Interrupt("admission"); err != nil {
				t.Fatal(err)
			}
			select {
			case run := <-done:
				if run == nil || run.State != RunInterrupted {
					t.Fatalf("run = %+v", run)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("interrupt did not finish")
			}
			j = reopen()
			f2, a := fakeFactory(&kit.TurnResult{Response: "new answer"})
			checks := 0
			r2 := NewRunner(j, f2, WithCompletionHook(func(_ context.Context, c CompletionCandidate) (CompletionFeedback, error) {
				checks++
				if c.ContinuationsUsed != 0 {
					t.Errorf("budget = %d", c.ContinuationsUsed)
				}
				return CompletionFeedback{}, nil
			}, 1))
			assertDeferred(t, r2, j, Input{Text: "NEW CHAT MESSAGE", Context: []string{"new context"}})
			if a.call != 0 || checks != 0 {
				t.Fatal("refusal ran model or hook")
			}
			run, err := r2.StartRetry(ctx, "admission", Input{Text: "original secret", Context: []string{"original context"}})
			if err != nil || run == nil || run.Response != "draft" || run.Usage.TotalTokens != 7 || a.call != 0 || checks != 1 {
				t.Fatalf("recovery = %+v, %v", run, err)
			}
			run, err = r2.Start(ctx, "admission", Input{Text: "NEW CHAT MESSAGE"})
			if err != nil || run.Response != "new answer" || a.call != 1 || checks != 2 {
				t.Fatalf("new input = %+v, %v", run, err)
			}
			msgs := a.session.GetMessages()
			if len(msgs) != 4 || messageText(msgs[0]) != "original secret" || messageText(msgs[2]) != "NEW CHAT MESSAGE" {
				t.Fatalf("history = %+v", msgs)
			}
		})
	}
}

type admissionReplayJournal struct {
	Journal
	failAt  int
	replays int
	err     error
}

func (j *admissionReplayJournal) Replay(ctx context.Context, id string) ([]Record, error) {
	j.replays++
	if j.replays == j.failAt {
		return nil, j.err
	}
	return j.Journal.Replay(ctx, id)
}

func TestInputAdmissionErrorsReleaseReservation(t *testing.T) {
	t.Parallel()
	for _, malformed := range []string{"replay", `{`, `{"phase":"unknown"}`, `{"phase":"pending","candidate":{"continuations_used":-1}}`} {
		t.Run(malformed, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			failure := errors.New("replay unavailable")
			j := &admissionReplayJournal{Journal: NewMemoryJournal(), err: failure}
			if _, err := j.Append(ctx, Record{RunID: "admission", Kind: RecordState, State: RunRunning}); err != nil {
				t.Fatal(err)
			}
			if malformed != "replay" {
				if _, err := j.Append(ctx, Record{RunID: "admission", Kind: RecordCompletion, Payload: []byte(malformed)}); err != nil {
					t.Fatal(err)
				}
			} else {
				j.failAt = 2
			}
			calls := 0
			r := NewRunner(j, func(_ context.Context, s *Session) (Agent, error) { calls++; return &fakeAgent{session: s}, nil })
			for range 2 {
				j.replays = 0
				run, err := r.Start(ctx, "admission", Input{Text: "new"})
				if run != nil || err == nil || errors.Is(err, ErrRunActive) || errors.Is(err, ErrInputDeferred) {
					t.Fatalf("Start = %+v, %v", run, err)
				}
				if malformed == "replay" && !errors.Is(err, failure) {
					t.Fatalf("lost replay error: %v", err)
				}
			}
			if calls != 0 {
				t.Fatal("agent constructed on replay error")
			}
			j.failAt = 0
			// Remove the corrupt test record by using a valid journal, not by changing
			// replay's error policy for historical malformed records.
			r.journal = NewMemoryJournal()
			if _, err := r.Start(ctx, "admission", Input{Text: "valid"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Without completion metadata, interruption alone owns the unfinished input.
// Redelivery supplies files when the input was not yet persisted.
func TestInputAdmissionPlainRecovery(t *testing.T) {
	t.Parallel()
	for _, saved := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsaved", true: "saved"}[saved], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			j := NewMemoryJournal()
			for _, rec := range []Record{{Kind: RecordTurn, Text: "old-turn"}, {Kind: RecordState, State: RunInterrupted}} {
				rec.RunID = "admission"
				if _, err := j.Append(ctx, rec); err != nil {
					t.Fatal(err)
				}
			}
			if saved {
				if _, err := NewSession("admission", j).AppendMessage(kit.NewLLMUserMessage("original secret")); err != nil {
					t.Fatal(err)
				}
			}
			a := &admissionContinuationAgent{fileRecordingAgent: &fileRecordingAgent{fakeAgent: &fakeAgent{turns: []*kit.TurnResult{{Response: "recovered"}}}}}
			r := NewRunner(j, func(_ context.Context, s *Session) (Agent, error) { a.session = s; return a, nil })
			assertDeferred(t, r, j, Input{Text: "original secret"})
			assertDeferred(t, r, j, Input{Files: []kit.LLMFilePart{{Data: []byte("new")}}})
			in := Input{Text: "original secret", Files: []kit.LLMFilePart{{Data: []byte("original file")}}}
			if _, err := r.StartRetry(ctx, "admission", in); err != nil {
				t.Fatal(err)
			}
			if saved != a.continued {
				t.Fatalf("continued = %v", a.continued)
			}
			if !saved && (len(a.files) != 1 || string(a.files[0].Data) != "original file") {
				t.Fatalf("files = %+v", a.files)
			}
			recs, err := j.Replay(ctx, "admission")
			if err != nil {
				t.Fatal(err)
			}
			users := 0
			for _, rec := range recs {
				if rec.Kind == RecordMessage && rec.Role == "user" {
					users++
				}
			}
			if users != 1 {
				t.Fatalf("user messages = %d", users)
			}
		})
	}
}

type admissionContinuationAgent struct {
	*fileRecordingAgent
	continued bool
}

func (a *admissionContinuationAgent) ContinueResult(context.Context) (*kit.TurnResult, error) {
	a.continued = true
	return &kit.TurnResult{Response: "recovered"}, nil
}

func TestInputAdmissionZeroValueAndClosedBoundaries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, boundary := range []RunState{RunCompleted, RunFailed, RunCancelled, RunRetired, RunWaiting, RunInterrupted} {
		t.Run(string(boundary), func(t *testing.T) {
			t.Parallel()
			j := NewMemoryJournal()
			r := NewRunner(j, nil)
			if err := r.saveCompletion(ctx, NewSession("admission", j), &completionRecord{Phase: "accepted", Candidate: CompletionCandidate{Response: "old"}}); err != nil {
				t.Fatal(err)
			}
			if err := r.checkpoint(ctx, "admission", boundary); err != nil {
				t.Fatal(err)
			}
			f, _ := fakeFactory(&kit.TurnResult{Response: "one"}, &kit.TurnResult{Response: "two"})
			r = NewRunner(j, f)
			if boundary == RunWaiting || boundary == RunRetired {
				want := ErrRunWaiting
				if boundary == RunRetired {
					want = ErrRunRetired
				}
				if _, err := r.Start(ctx, "admission", Input{Text: "new"}); !errors.Is(err, want) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if boundary == RunInterrupted {
				run, err := r.Start(ctx, "admission", Input{Files: []kit.LLMFilePart{}, Context: []string{}})
				if err != nil || run.Response != "old" {
					t.Fatalf("zero-value recovery = %+v, %v", run, err)
				}
				return
			}
			for range 2 {
				if _, err := r.Start(ctx, "admission", Input{Text: "same"}); err != nil {
					t.Fatal(err)
				}
			}
			recs, err := j.Replay(ctx, "admission")
			if err != nil {
				t.Fatal(err)
			}
			users := 0
			for _, rec := range recs {
				if rec.Kind == RecordMessage && rec.Role == "user" && rec.Text == "same" {
					users++
				}
			}
			if users != 2 {
				t.Fatalf("identical completed inputs = %d", users)
			}
		})
	}
}

// A durable cancellation command takes precedence and can append its terminal
// bookkeeping before admission. It closes the old completion budget.
func TestInputAdmissionRecoversCancellationFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	for _, rec := range []Record{
		{Kind: RecordTurn, Text: "old-turn"}, {Kind: RecordState, State: RunInterrupted},
		{Kind: RecordCompletion, Payload: []byte(`{"phase":"accepted","candidate":{"response":"old","continuations_used":2}}`)},
		{Kind: RecordCancel, Text: "old-turn"},
	} {
		rec.RunID = "admission"
		if _, err := j.Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	f, a := fakeFactory(&kit.TurnResult{Response: "new"})
	r := NewRunner(j, f)
	run, err := r.Start(ctx, "admission", Input{Text: "new input"})
	if err != nil || run.Response != "new" || a.call != 1 {
		t.Fatalf("Start = %+v, %v", run, err)
	}
	recs, err := j.Replay(ctx, "admission")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 5 || recs[4].Kind != RecordState || recs[4].State != RunCancelled {
		t.Fatalf("cancellation not recovered first: %+v", recs)
	}
}

// Restore would repair this old torn tool step. Refusal must not reach Restore.
func TestInputAdmissionPrecedesSessionRepair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := NewMemoryJournal()
	s := NewSession("admission", j)
	if _, err := s.AppendMessage(kit.LLMMessage{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMToolCallPart{ToolCallID: "orphan", ToolName: "effect", Input: "{}"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(ctx, Record{RunID: "admission", Kind: RecordState, State: RunInterrupted}); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(j, func(context.Context, *Session) (Agent, error) {
		t.Fatal("factory reached before refusal")
		return nil, nil
	})
	assertDeferred(t, r, j, Input{Text: "new"})
}
