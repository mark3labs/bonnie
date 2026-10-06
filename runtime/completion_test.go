package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Only the accepted response reaches EventResponse. Usage includes every model
// call, while completion checks see the count before their requested continuation.
func TestCompletionLoop(t *testing.T) {
	t.Parallel()
	f, agent := fakeFactory(
		&kit.TurnResult{Response: "draft", TotalUsage: &kit.LLMUsage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5, ReasoningTokens: 1, CacheCreationTokens: 4, CacheReadTokens: 6}},
		&kit.TurnResult{Response: "final", TotalUsage: &kit.LLMUsage{InputTokens: 7, OutputTokens: 8, TotalTokens: 15, ReasoningTokens: 2, CacheCreationTokens: 5, CacheReadTokens: 9}},
	)
	var candidates []CompletionCandidate
	r := NewRunner(nil, f, WithCompletionHook(func(_ context.Context, c CompletionCandidate) (CompletionFeedback, error) {
		candidates = append(candidates, c)
		if c.Response == "draft" {
			return CompletionFeedback{ContinueWith: "correct it"}, nil
		}
		return CompletionFeedback{}, nil
	}, 1))
	events, stop := r.Events().Subscribe("loop", 0)
	defer stop()
	run, err := r.Start(context.Background(), "loop", Input{Text: "start"})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != RunCompleted || run.Response != "final" || agent.call != 2 {
		t.Fatalf("run = %+v, calls = %d", run, agent.call)
	}
	want := []CompletionCandidate{{Response: "draft"}, {Response: "final", ContinuationsUsed: 1}}
	if !reflect.DeepEqual(candidates, want) {
		t.Fatalf("candidates = %+v", candidates)
	}
	usage := &kit.LLMUsage{InputTokens: 9, OutputTokens: 11, TotalTokens: 20, ReasoningTokens: 3, CacheCreationTokens: 9, CacheReadTokens: 15}
	if !reflect.DeepEqual(run.Usage, usage) {
		t.Fatalf("usage = %+v", run.Usage)
	}
	for {
		ev := <-events
		if ev.Type == EventResponse && ev.Text != "final" {
			t.Fatalf("unaccepted response: %+v", ev)
		}
		if ev.Type == EventState && ev.State == RunCompleted {
			break
		}
	}
}

func TestCompletionLimitAndNewBudget(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, 1, 2} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			t.Parallel()
			f, a := fakeFactory(&kit.TurnResult{Response: "a"}, &kit.TurnResult{Response: "b"}, &kit.TurnResult{Response: "c"})
			var counts []int
			r := NewRunner(nil, f, WithCompletionHook(func(_ context.Context, c CompletionCandidate) (CompletionFeedback, error) {
				counts = append(counts, c.ContinuationsUsed)
				return CompletionFeedback{ContinueWith: "again"}, nil
			}, limit))
			run, err := r.Start(context.Background(), "limit", Input{})
			if !errors.Is(err, ErrContinuationLimit) || run.State != RunFailed || a.call != limit+1 {
				t.Fatalf("run = %+v, err = %v, calls = %d", run, err, a.call)
			}
			if len(counts) != limit+1 || counts[len(counts)-1] != limit {
				t.Fatalf("counts = %v", counts)
			}
			// Failure closes the logical turn. A new Start gets a fresh budget.
			f2, _ := fakeFactory(&kit.TurnResult{Response: "new"})
			r2 := NewRunner(r.Journal(), f2, WithCompletionHook(func(_ context.Context, c CompletionCandidate) (CompletionFeedback, error) {
				if c.ContinuationsUsed != 0 {
					t.Errorf("new budget = %d", c.ContinuationsUsed)
				}
				return CompletionFeedback{}, nil
			}, 0))
			if _, err := r2.Start(context.Background(), "limit", Input{Text: "new"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A continuation can wait for a human. A new Runner resumes the same budget,
// and does not check the suspended response or charge the human answer as a
// hook-requested continuation.
func TestCompletionSuspensionKeepsBudget(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	f, _ := fakeFactory(&kit.TurnResult{Response: "draft", TotalUsage: &kit.LLMUsage{TotalTokens: 2}}, &kit.TurnResult{Response: "question", HaltedByTool: "ask_human", FinalValue: SuspendRequest{Kind: SuspendQuestion, Prompt: "where?"}, TotalUsage: &kit.LLMUsage{TotalTokens: 3}})
	checks := 0
	r := NewRunner(j, f, WithCompletionHook(func(_ context.Context, _ CompletionCandidate) (CompletionFeedback, error) {
		checks++
		return CompletionFeedback{ContinueWith: "finish"}, nil
	}, 1))
	run, err := r.Start(context.Background(), "wait", Input{})
	if err != nil || run.State != RunWaiting || checks != 1 || run.Usage.TotalTokens != 5 {
		t.Fatalf("run = %+v, checks = %d, err = %v", run, checks, err)
	}
	f2, a := fakeFactory(&kit.TurnResult{Response: "answer", TotalUsage: &kit.LLMUsage{TotalTokens: 7}})
	r2 := NewRunner(j, f2, WithCompletionHook(func(_ context.Context, c CompletionCandidate) (CompletionFeedback, error) {
		if c.ContinuationsUsed != 1 {
			t.Errorf("budget reset: %+v", c)
		}
		return CompletionFeedback{ContinueWith: "one too many"}, nil
	}, 1))
	run, err = r2.Resume(context.Background(), "wait", []InputResponse{{Text: "here"}})
	if !errors.Is(err, ErrContinuationLimit) || run.State != RunFailed || a.call != 1 {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
}

// Cancel during a check leaves its candidate durable. A second Runner retries
// the check, not the model, and sees the same continuation count.
func TestCompletionCancelledCheckRecovery(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	f, _ := fakeFactory(&kit.TurnResult{Response: "draft"})
	started := make(chan struct{})
	r := NewRunner(j, f, WithCompletionHook(func(ctx context.Context, _ CompletionCandidate) (CompletionFeedback, error) {
		close(started)
		<-ctx.Done()
		return CompletionFeedback{}, ctx.Err()
	}, 1))
	done := make(chan *Run, 1)
	go func() {
		run, err := r.Start(context.Background(), "cancel", Input{})
		if err != nil {
			t.Errorf("Start: %v", err)
		}
		done <- run
	}()
	<-started
	if err := r.Cancel("cancel"); err != nil {
		t.Fatal(err)
	}
	if run := <-done; run == nil || run.State != RunCancelled {
		t.Fatalf("run = %+v", run)
	}
	f2, a := fakeFactory()
	r2 := NewRunner(j, f2, WithCompletionHook(func(_ context.Context, c CompletionCandidate) (CompletionFeedback, error) {
		if c.Response != "draft" || c.ContinuationsUsed != 0 {
			t.Errorf("candidate = %+v", c)
		}
		return CompletionFeedback{}, nil
	}, 1))
	run, err := r2.Start(context.Background(), "cancel", Input{Text: "must not replace pending work"})
	if err != nil || run.Response != "draft" || a.call != 0 {
		t.Fatalf("run = %+v, err = %v, calls = %d", run, err, a.call)
	}
}

// Seed a crash window with a first Runner. The second shares only its journal.
// Feedback is not checked twice after it is durable. A finished text message
// after the pending watermark avoids a second call to the model.
func TestCompletionCrashPhases(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"check", "continue", "pending", "accepted"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			j := NewMemoryJournal()
			r1 := NewRunner(j, nil)
			s := NewSession("crash", j)
			if err := r1.checkpoint(ctx, s.runID, RunRunning); err != nil {
				t.Fatal(err)
			}
			c := &completionRecord{Phase: phase, Candidate: CompletionCandidate{Response: "draft", ContinuationsUsed: 1}, Feedback: CompletionFeedback{ContinueWith: "fix"}, Prompt: "fix", Usage: &kit.LLMUsage{TotalTokens: 5}}
			if err := r1.saveCompletion(ctx, s, c); err != nil {
				t.Fatal(err)
			}
			if phase == "pending" {
				if _, err := s.AppendMessage(kit.NewLLMUserMessage("fix")); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AppendMessage(kit.LLMMessage{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "final"}}}); err != nil {
					t.Fatal(err)
				}
			}
			f, a := fakeFactory(&kit.TurnResult{Response: "final", TotalUsage: &kit.LLMUsage{TotalTokens: 7}})
			checks := 0
			r2 := NewRunner(j, f, WithCompletionHook(func(_ context.Context, candidate CompletionCandidate) (CompletionFeedback, error) {
				checks++
				wantCount := 1
				if phase == "continue" {
					wantCount = 2
				}
				if candidate.ContinuationsUsed != wantCount {
					t.Errorf("count = %d", candidate.ContinuationsUsed)
				}
				return CompletionFeedback{}, nil
			}, 2))
			run, err := r2.Start(ctx, "crash", Input{})
			if err != nil || run.State != RunCompleted {
				t.Fatalf("run = %+v, err = %v", run, err)
			}
			wantCalls, wantChecks, wantUsage := 0, 1, int64(5)
			if phase == "continue" {
				wantCalls, wantUsage = 1, 12
			}
			if phase == "accepted" {
				wantChecks = 0
			}
			if a.call != wantCalls || checks != wantChecks || run.Usage.TotalTokens != wantUsage {
				t.Fatalf("calls = %d, checks = %d, usage = %+v", a.call, checks, run.Usage)
			}
		})
	}
}

type completionTestAgent struct {
	*fakeAgent
	hook CompletionHook
}

func (a *completionTestAgent) Complete(ctx context.Context, c CompletionCandidate) (CompletionFeedback, error) {
	return a.hook(ctx, c)
}

func TestCompletionAgentAndFailures(t *testing.T) {
	t.Parallel()
	failure := errors.New("check failed")
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "accept", true: "error"}[fail], func(t *testing.T) {
			t.Parallel()
			checks := 0
			factory := func(_ context.Context, s *Session) (Agent, error) {
				return &completionTestAgent{fakeAgent: &fakeAgent{session: s, turns: []*kit.TurnResult{{Response: "answer"}}}, hook: func(_ context.Context, _ CompletionCandidate) (CompletionFeedback, error) {
					checks++
					if fail {
						return CompletionFeedback{}, failure
					}
					return CompletionFeedback{}, nil
				}}, nil
			}
			r := NewRunner(nil, factory, WithCompletionLimit(0))
			run, err := r.Start(context.Background(), "agent", Input{})
			if checks != 1 {
				t.Fatalf("checks = %d", checks)
			}
			if fail {
				if !errors.Is(err, failure) || run.State != RunFailed {
					t.Fatalf("run = %+v, err = %v", run, err)
				}
			} else if err != nil || run.State != RunCompleted {
				t.Fatalf("run = %+v, err = %v", run, err)
			}
		})
	}
}
