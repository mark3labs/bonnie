package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/runtime"
)

type faultJournal struct {
	runtime.Journal
	mu    sync.Mutex
	fail  func(snapshot) bool
	after bool
}

var errWrite = errors.New("injected write failure")

func (j *faultJournal) Append(ctx context.Context, r runtime.Record) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var st snapshot
	if err := json.Unmarshal(r.Payload, &st); err != nil {
		return 0, err
	}
	if j.fail != nil && j.fail(st) {
		if j.after {
			if _, err := j.Journal.Append(ctx, r); err != nil {
				return 0, err
			}
		}
		return 0, errWrite
	}
	return j.Journal.Append(ctx, r)
}

// A failed result save must not let the function continue to its next effect.
// An ambiguous commit failure poisons this service; recovery uses the journal.
func TestStepResultWriteFailureStopsBeforeNextEffect(t *testing.T) {
	t.Parallel()
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_commit", true: "after_commit"}[after], func(t *testing.T) {
			t.Parallel()
			var first, second atomic.Int32
			d := Define("save", "1", func(ctx context.Context, _ int) (int, error) {
				_, err := Step(ctx, "first", func(context.Context, Attempt) (int, error) { first.Add(1); return 1, nil })
				if err != nil {
					return 0, err
				}
				return Step(ctx, "second", func(context.Context, Attempt) (int, error) { second.Add(1); return 2, nil })
			})
			h := newHarness(t, d)
			j := &faultJournal{Journal: h.j, after: after, fail: func(st snapshot) bool { return len(st.Operations) > 0 && st.Operations[0].State == "success" }}
			h.s.journal = j
			handle, err := Start(context.Background(), h.s, d, "save", 0)
			if err != nil {
				t.Fatal(err)
			}
			h.tick()
			if first.Load() != 1 || second.Load() != 0 {
				t.Fatalf("effects=%d,%d", first.Load(), second.Load())
			}
			if err := h.s.Tick(context.Background()); !errors.Is(err, errWrite) {
				t.Fatalf("poisoned service: %v", err)
			}
			h.restart(d)
			h.tick()
			if after {
				h.state(handle.ID(), Completed)
				if first.Load() != 1 || second.Load() != 1 {
					t.Fatal("replayed saved action")
				}
			} else {
				h.state(handle.ID(), Blocked)
				if second.Load() != 0 {
					t.Fatal("continued unknown action")
				}
			}
		})
	}
}

func TestPendingSaveFailureNeverCallsCallback(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := Define("pending", "1", func(ctx context.Context, _ int) (int, error) {
		return Step(ctx, "action", func(context.Context, Attempt) (int, error) { calls.Add(1); return 1, nil })
	})
	h := newHarness(t, d)
	h.s.journal = &faultJournal{Journal: h.j, fail: func(st snapshot) bool { return len(st.Operations) > 0 && st.Operations[0].State == "pending" }}
	handle, err := Start(context.Background(), h.s, d, "pending", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	if calls.Load() != 0 {
		t.Fatal("called before pending save")
	}
	h.restart(d)
	h.tick()
	h.state(handle.ID(), Completed)
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}

func TestAdmissionAmbiguousCommitRecoversIdempotently(t *testing.T) {
	t.Parallel()
	d := Define("admit", "1", func(context.Context, int) (int, error) { return 1, nil })
	h := newHarness(t, d)
	h.s.journal = &faultJournal{Journal: h.j, after: true, fail: func(st snapshot) bool { return st.State == Pending }}
	if _, err := Start(context.Background(), h.s, d, "admission", 5); !errors.Is(err, errWrite) {
		t.Fatal(err)
	}
	h.restart(d)
	handle, err := Start(context.Background(), h.s, d, "admission", 5)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.state(handle.ID(), Completed)
	records, err := h.j.Replay(context.Background(), handle.ID())
	if err != nil {
		t.Fatal(err)
	}
	admissions := 0
	for _, r := range records {
		var st snapshot
		if err := json.Unmarshal(r.Payload, &st); err != nil {
			t.Fatal(err)
		}
		if st.State == Pending {
			admissions++
		}
	}
	if admissions != 1 {
		t.Fatal(admissions)
	}
}

func TestConcurrentAdmissionSignalAndTicks(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := Define("concurrent", "1", func(ctx context.Context, _ int) (int, error) {
		_, err := Step(ctx, "once", func(context.Context, Attempt) (int, error) { calls.Add(1); return 0, nil })
		if err != nil {
			return 0, err
		}
		return Wait[int](ctx, "answer")
	})
	h := newHarness(t, d)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			handle, err := Start(ctx, h.s, d, "same", 1)
			if err != nil {
				t.Error(err)
				return
			}
			if err := handle.Signal(ctx, "answer", "same", 8); err != nil {
				t.Error(err)
			}
			if err := h.s.Tick(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	h.s.wg.Wait()
	h.tick()
	handle, err := Start(ctx, h.s, d, "same", 1)
	if err != nil {
		t.Fatal(err)
	}
	h.state(handle.ID(), Completed)
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	if got := result[int](t, h.s, handle.ID()); got != 8 {
		t.Fatal(got)
	}
}

func TestServiceRunParksAndRecoversAfterShutdown(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	var count atomic.Int32
	d := Define("lifecycle", "1", func(ctx context.Context, _ int) (int, error) {
		return Step(ctx, "action", func(ctx context.Context, _ Attempt) (int, error) {
			if count.Add(1) == 1 {
				close(started)
				<-ctx.Done()
				return 0, ctx.Err()
			}
			return 7, nil
		}, RepeatSafe(), WithRetry(RetryPolicy{MaxAttempts: 2}))
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "lifecycle", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.s.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	h.restart(d)
	h.tick()
	h.tick()
	h.state(handle.ID(), Completed)
	if count.Load() != 2 {
		t.Fatal(count.Load())
	}
}

func TestUnsafeRetryOptionsRejectedBeforeEffects(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := Define("options", "1", func(ctx context.Context, _ int) (int, error) {
		return Step(ctx, "unsafe", func(context.Context, Attempt) (int, error) { calls.Add(1); return 0, nil }, WithRetry(RetryPolicy{MaxAttempts: 2}))
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "options", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.state(handle.ID(), Failed)
	if calls.Load() != 0 {
		t.Fatal("unsafe retry accepted")
	}
}

func TestDuplicateOperationKeyRejected(t *testing.T) {
	t.Parallel()
	d := Define("keys", "1", func(ctx context.Context, _ int) (int, error) {
		if err := Sleep(ctx, "same", 0); err != nil {
			return 0, err
		}
		return Wait[int](ctx, "same")
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "keys", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.state(handle.ID(), Failed)
}

// A completion failure after a saved step must not cause a second callback.
func TestCompletionFailureReplaysSavedSuccess(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := Define("completion", "1", func(ctx context.Context, _ int) (int, error) {
		return Step(ctx, "once", func(context.Context, Attempt) (int, error) { calls.Add(1); return 11, nil })
	})
	h := newHarness(t, d)
	h.s.journal = &faultJournal{Journal: h.j, fail: func(st snapshot) bool { return st.State == Completed }}
	handle, err := Start(context.Background(), h.s, d, "completion", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	h.restart(d)
	h.tick()
	h.state(handle.ID(), Completed)
	if got := result[int](t, h.s, handle.ID()); got != 11 || calls.Load() != 1 {
		t.Fatal("saved step repeated")
	}
}

func TestShutdownTimeoutDoesNotPermitConcurrentRecovery(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	d := Define("slow", "1", func(ctx context.Context, _ int) (int, error) {
		return Step(ctx, "slow", func(context.Context, Attempt) (int, error) { close(started); <-release; return 3, nil })
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "slow", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.s.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := h.s.Tick(context.Background()); err == nil {
		t.Fatal("closed service executed new work")
	}
	close(release)
	h.s.wg.Wait()
	h.restart(d)
	h.tick()
	h.state(handle.ID(), Blocked)
}

func TestReservedWorkflowRunsAreNotConversationRecovery(t *testing.T) {
	t.Parallel()
	d := Define("reserved", "1", func(ctx context.Context, _ int) (int, error) { return Wait[int](ctx, "value") })
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "reserved", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	if !runtime.IsReservedRun(handle.ID()) {
		t.Fatal("workflow ID is not reserved")
	}
	runner := runtime.NewRunner(h.j, func(context.Context, *runtime.Session) (runtime.Agent, error) {
		t.Error("conversation factory called for workflow")
		return nil, errors.New("must not execute")
	})
	if _, err := runner.Continue(context.Background(), handle.ID()); !errors.Is(err, runtime.ErrNothingToContinue) {
		t.Fatal(err)
	}
}

func TestRepeatSafeInterruptedAttemptLimit(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := Define("limit", "1", func(ctx context.Context, _ int) (int, error) {
		return Step(ctx, "unknown", func(context.Context, Attempt) (int, error) { calls.Add(1); panic("crash") }, RepeatSafe())
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "limit", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.restart(d)
	h.tick()
	h.state(handle.ID(), Failed)
	if calls.Load() != 1 {
		t.Fatal("attempt limit exceeded")
	}
}

func TestInvalidJSONResultFailsWithoutRepeatingEffect(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := Define("encoding", "1", func(ctx context.Context, _ int) (int, error) {
		_, err := Step(ctx, "value", func(context.Context, Attempt) (any, error) { calls.Add(1); return make(chan int), nil }, RepeatSafe(), WithRetry(RetryPolicy{MaxAttempts: 3}))
		return 0, err
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "encoding", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.state(handle.ID(), Failed)
	if calls.Load() != 1 {
		t.Fatal("encoding error retried")
	}
}
