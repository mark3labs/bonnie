package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/runtime"
)

// Each restart test closes SQLite, opens it again, and constructs a service
// with no shared execution state. Only the directory crosses that boundary.
type harness struct {
	t     *testing.T
	dir   string
	j     *runtime.SQLiteJournal
	s     *Service
	clock time.Time
}

func newHarness(t *testing.T, defs ...Workflow) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h.open(defs...)
	t.Cleanup(func() { h.close() })
	return h
}
func (h *harness) open(defs ...Workflow) {
	h.t.Helper()
	var err error
	h.j, err = runtime.OpenSQLiteJournal(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	h.s, err = NewService(h.j, defs...)
	if err != nil {
		h.t.Fatal(err)
	}
	h.s.now = func() time.Time { return h.clock }
}
func (h *harness) close() {
	h.t.Helper()
	if h.s != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := h.s.Shutdown(ctx); err != nil {
			h.t.Fatal(err)
		}
		h.s = nil
	}
	if h.j != nil {
		if err := h.j.Close(); err != nil {
			h.t.Fatal(err)
		}
		h.j = nil
	}
}
func (h *harness) restart(defs ...Workflow) { h.t.Helper(); h.close(); h.open(defs...) }
func (h *harness) tick() {
	h.t.Helper()
	if err := h.s.Tick(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.s.wg.Wait()
}
func (h *harness) state(id string, want State) Status {
	h.t.Helper()
	st, err := h.s.Status(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	if st.State != want {
		h.t.Fatalf("state = %s, want %s; error=%s", st.State, want, st.Error)
	}
	return st
}
func result[T any](t *testing.T, s *Service, id string) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	handle, err := Open[T](ctx, s, id)
	if err != nil {
		t.Fatal(err)
	}
	value, err := handle.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestReplaySleepSignalAndAdmissionAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var calls atomic.Int32
	d := Define("order", "1", func(ctx context.Context, input int) (string, error) {
		n, err := Step(ctx, "charge", func(context.Context, Attempt) (int, error) { calls.Add(1); return input * 2, nil })
		if err != nil {
			return "", err
		}
		if err := Sleep(ctx, "settle", time.Hour); err != nil {
			return "", err
		}
		approved, err := Wait[bool](ctx, "approval")
		if err != nil {
			return "", err
		}
		if !approved || n != 14 {
			return "", errors.New("invalid state")
		}
		return "done", nil
	})
	h := newHarness(t, d)
	handle, err := Start(ctx, h.s, d, "request-1", 7)
	if err != nil {
		t.Fatal(err)
	}
	id := handle.ID()
	if err := handle.Signal(ctx, "approval", "signal-1", true); err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.state(id, Waiting)
	if calls.Load() != 1 {
		t.Fatal("callback not called once")
	}
	h.restart(d)
	same, err := Start(ctx, h.s, d, "request-1", 7)
	if err != nil || same.ID() != id {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := Start(ctx, h.s, d, "request-1", 8); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if err := same.Signal(ctx, "approval", "signal-1", true); err != nil {
		t.Fatal(err)
	}
	if err := same.Signal(ctx, "approval", "signal-1", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("signal conflict: %v", err)
	}
	h.clock = h.clock.Add(30 * time.Minute)
	h.tick()
	h.state(id, Waiting)
	h.clock = h.clock.Add(30 * time.Minute)
	h.tick()
	h.state(id, Completed)
	if got := result[string](t, h.s, id); got != "done" {
		t.Fatal(got)
	}
	if calls.Load() != 1 {
		t.Fatal("replayed effect")
	}
	h.restart(d)
	if got := result[string](t, h.s, id); got != "done" {
		t.Fatal(got)
	}
}

func TestRetryDeadlineAndStableIdentityAcrossRestart(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var attempts []Attempt
	d := Define("retry", "1", func(ctx context.Context, _ struct{}) (int, error) {
		return Step(ctx, "send", func(_ context.Context, a Attempt) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, a)
			if a.Number < 3 {
				return 0, errors.New("temporary")
			}
			return 42, nil
		}, RepeatSafe(), WithRetry(RetryPolicy{MaxAttempts: 3, InitialDelay: time.Minute, MaxDelay: 2 * time.Minute}))
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "retry", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.state(handle.ID(), Waiting)
	h.restart(d)
	h.tick()
	mu.Lock()
	n := len(attempts)
	mu.Unlock()
	if n != 1 {
		t.Fatal("retry ran before deadline")
	}
	h.clock = h.clock.Add(time.Minute)
	h.tick()
	h.state(handle.ID(), Waiting)
	h.restart(d)
	h.clock = h.clock.Add(2 * time.Minute)
	h.tick()
	h.state(handle.ID(), Completed)
	if got := result[int](t, h.s, handle.ID()); got != 42 {
		t.Fatal(got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 3 {
		t.Fatal(attempts)
	}
	for i, a := range attempts {
		if a.Number != i+1 || a.IdempotencyKey == "" || a.IdempotencyKey != attempts[0].IdempotencyKey {
			t.Fatal(attempts)
		}
	}
}

func TestUnsafePendingBlocksUntilReconciled(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := Define("unsafe", "1", func(ctx context.Context, _ int) (int, error) {
		return Step(ctx, "external", func(context.Context, Attempt) (int, error) { calls.Add(1); panic("process stopped after action") })
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "unsafe", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := handle.ID()
	h.tick()
	h.restart(d)
	h.tick()
	st := h.state(id, Blocked)
	if st.BlockedKey != "external" || calls.Load() != 1 {
		t.Fatalf("status=%+v calls=%d", st, calls.Load())
	}
	if err := Resolve(context.Background(), h.s, id, "external", "wrong type", nil); !errors.Is(err, ErrReplay) {
		t.Fatalf("resolve type: %v", err)
	}
	if err := Resolve(context.Background(), h.s, id, "external", 99, nil); err != nil {
		t.Fatal(err)
	}
	h.restart(d)
	h.tick()
	h.state(id, Completed)
	if got := result[int](t, h.s, id); got != 99 || calls.Load() != 1 {
		t.Fatalf("got=%d calls=%d", got, calls.Load())
	}
}

func TestRepeatSafePendingRecoversWithBackoff(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var ids []Attempt
	d := Define("safe", "1", func(ctx context.Context, _ bool) (int, error) {
		return Step(ctx, "write", func(_ context.Context, a Attempt) (int, error) {
			ids = append(ids, a)
			if calls.Add(1) == 1 {
				panic("crash")
			}
			return 5, nil
		}, RepeatSafe(), WithRetry(RetryPolicy{MaxAttempts: 2, InitialDelay: time.Minute}))
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "safe", true)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.restart(d)
	h.tick()
	h.state(handle.ID(), Waiting)
	if calls.Load() != 1 {
		t.Fatal("repeated before recovery delay")
	}
	h.clock = h.clock.Add(time.Minute)
	h.tick()
	h.state(handle.ID(), Completed)
	if len(ids) != 2 || ids[0].IdempotencyKey != ids[1].IdempotencyKey || ids[1].Number != 2 {
		t.Fatal(ids)
	}
}

func TestWaitCancellationDoesNotCancelExecution(t *testing.T) {
	t.Parallel()
	d := Define("wait", "1", func(ctx context.Context, _ int) (int, error) { return Wait[int](ctx, "number") })
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "wait", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.state(handle.ID(), Waiting)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := handle.Result(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	h.state(handle.ID(), Waiting)
	h.restart(d)
	if err := h.s.Signal(context.Background(), handle.ID(), "number", "one", 23); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if got := result[int](t, h.s, handle.ID()); got != 23 {
		t.Fatal(got)
	}
}

func TestCancelPersistsAndStopsCallback(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	stopped := make(chan struct{})
	d := Define("cancel", "1", func(ctx context.Context, _ int) (int, error) {
		return Step(ctx, "work", func(ctx context.Context, _ Attempt) (int, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return 0, ctx.Err()
		})
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "cancel", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("callback did not start")
	}
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("callback not cancelled")
	}
	h.s.wg.Wait()
	h.restart(d)
	h.tick()
	h.state(handle.ID(), Cancelled)
	opened, err := Open[int](context.Background(), h.s, handle.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.Result(context.Background()); !errors.Is(err, ErrCancelled) {
		t.Fatal(err)
	}
}

func TestReplayRejectsChangedOperations(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"key", "type", "order", "shorter", "options", "duplicate", "duration"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			base := Define("identity", "1", func(ctx context.Context, _ int) (int, error) {
				_, err := Step(ctx, "first", func(context.Context, Attempt) (int, error) { return 1, nil })
				if err != nil {
					return 0, err
				}
				return 0, Sleep(ctx, "pause", time.Hour)
			})
			h := newHarness(t, base)
			handle, err := Start(context.Background(), h.s, base, change, 0)
			if err != nil {
				t.Fatal(err)
			}
			h.tick()
			modified := Define("identity", "1", func(ctx context.Context, _ int) (int, error) {
				if change == "shorter" {
					return 0, nil
				}
				if change == "order" {
					return 0, Sleep(ctx, "pause", time.Hour)
				}
				key := "first"
				if change == "key" {
					key = "changed"
				}
				if change == "type" {
					_, err := Step(ctx, key, func(context.Context, Attempt) (string, error) { return "", nil })
					return 0, err
				}
				opts := []StepOption{}
				if change == "options" {
					opts = append(opts, RepeatSafe())
				}
				_, err := Step(ctx, key, func(context.Context, Attempt) (int, error) { t.Error("replayed callback"); return 0, nil }, opts...)
				if err != nil {
					return 0, err
				}
				if change == "duplicate" {
					_, err = Step(ctx, "first", func(context.Context, Attempt) (int, error) { return 0, nil })
					return 0, err
				}
				duration := time.Hour
				if change == "duration" {
					duration = 2 * time.Hour
				}
				return 0, Sleep(ctx, "pause", duration)
			})
			h.restart(modified)
			h.clock = h.clock.Add(time.Hour)
			h.tick()
			st := h.state(handle.ID(), Failed)
			if !strings.Contains(st.Error, ErrReplay.Error()) {
				t.Fatal(st.Error)
			}
		})
	}
}

func TestVersionIsPinnedAndMissingDefinitionDoesNotRun(t *testing.T) {
	t.Parallel()
	old := Define("version", "1", func(ctx context.Context, _ int) (int, error) {
		if err := Sleep(ctx, "pause", time.Minute); err != nil {
			return 0, err
		}
		return 1, nil
	})
	h := newHarness(t, old)
	handle, err := Start(context.Background(), h.s, old, "version", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	newVersion := Define("version", "2", func(context.Context, int) (int, error) { t.Error("wrong version"); return 2, nil })
	h.restart(newVersion)
	h.clock = h.clock.Add(time.Hour)
	h.tick()
	h.state(handle.ID(), Waiting)
	if _, err := Start(context.Background(), h.s, newVersion, "version", 0); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	h.restart(old, newVersion)
	h.tick()
	if got := result[int](t, h.s, handle.ID()); got != 1 {
		t.Fatal(got)
	}
}

func TestTypedSignalDecodeErrorAndDedupAcrossRestart(t *testing.T) {
	t.Parallel()
	d := Define("decode", "1", func(ctx context.Context, _ int) (int, error) { return Wait[int](ctx, "value") })
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "decode", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Signal(context.Background(), "value", "one", "not a number"); err != nil {
		t.Fatal(err)
	}
	h.restart(d)
	h.tick()
	h.state(handle.ID(), Failed)
	if err := h.s.Signal(context.Background(), handle.ID(), "value", "one", "not a number"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Signal(context.Background(), handle.ID(), "value", "two", 1); !errors.Is(err, ErrTerminal) {
		t.Fatal(err)
	}
}

func TestPermanentErrorAndAttemptTimeout(t *testing.T) {
	t.Parallel()
	for _, permanent := range []bool{true, false} {
		t.Run(map[bool]string{true: "permanent", false: "timeout"}[permanent], func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			d := Define("errors", "1", func(ctx context.Context, _ int) (int, error) {
				return Step(ctx, "attempt", func(ctx context.Context, _ Attempt) (int, error) {
					calls.Add(1)
					if permanent {
						return 0, Permanent(errors.New("denied"))
					}
					<-ctx.Done()
					return 0, ctx.Err()
				}, RepeatSafe(), WithRetry(RetryPolicy{MaxAttempts: 3}), WithAttemptTimeout(time.Millisecond))
			})
			h := newHarness(t, d)
			handle, err := Start(context.Background(), h.s, d, "errors", 0)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				h.tick()
			}
			h.state(handle.ID(), Failed)
			want := int32(3)
			if permanent {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("calls=%d want=%d", calls.Load(), want)
			}
		})
	}
}

func TestSavedSignalResultReplaysWithoutAnotherSignal(t *testing.T) {
	t.Parallel()
	d := Define("mail", "1", func(ctx context.Context, _ int) (int, error) {
		n, err := Wait[int](ctx, "first")
		if err != nil {
			return 0, err
		}
		if err := Sleep(ctx, "pause", time.Minute); err != nil {
			return 0, err
		}
		return n, nil
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "mail", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Signal(context.Background(), "first", "one", 17); err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.state(handle.ID(), Waiting)
	h.restart(d)
	h.clock = h.clock.Add(time.Minute)
	h.tick()
	if got := result[int](t, h.s, handle.ID()); got != 17 {
		t.Fatal(got)
	}
	h.s.mu.Lock()
	st := h.s.runs[handle.ID()]
	h.s.mu.Unlock()
	if len(st.Messages) != 1 || !st.Messages[0].Consumed {
		t.Fatal("signal consumption not saved")
	}
}

func TestStepErrorsReplayAsSavedText(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := Define("handled", "1", func(ctx context.Context, _ int) (string, error) {
		_, err := Step(ctx, "rejected", func(context.Context, Attempt) (int, error) { calls.Add(1); return 0, Permanent(errors.New("declined")) })
		if err == nil || err.Error() != "declined" {
			return "", errors.New("different saved error")
		}
		if err := Sleep(ctx, "later", time.Minute); err != nil {
			return "", err
		}
		return "handled", nil
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "handled", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.restart(d)
	h.clock = h.clock.Add(time.Minute)
	h.tick()
	if got := result[string](t, h.s, handle.ID()); got != "handled" || calls.Load() != 1 {
		t.Fatal(got, calls.Load())
	}
}

func TestJSONRoundTripIsAppliedOnFirstStepResult(t *testing.T) {
	t.Parallel()
	d := Define("json", "1", func(ctx context.Context, _ int) (string, error) {
		value, err := Step[any](ctx, "number", func(context.Context, Attempt) (any, error) { return int(12), nil })
		if err != nil {
			return "", err
		}
		if _, ok := value.(float64); !ok {
			return "", errors.New("first result differs from JSON replay")
		}
		return "same", nil
	})
	h := newHarness(t, d)
	handle, err := Start(context.Background(), h.s, d, "json", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	if got := result[string](t, h.s, handle.ID()); got != "same" {
		t.Fatal(got)
	}
}

func TestNoOperationsOutsideExecutionContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := Step(ctx, "x", func(context.Context, Attempt) (int, error) { t.Error("callback outside workflow"); return 0, nil }); !errors.Is(err, ErrNotWorkflow) {
		t.Fatal(err)
	}
	if _, err := Wait[int](ctx, "x"); !errors.Is(err, ErrNotWorkflow) {
		t.Fatal(err)
	}
	if err := Sleep(ctx, "x", 0); !errors.Is(err, ErrNotWorkflow) {
		t.Fatal(err)
	}
}

func TestServiceDefinitionValidation(t *testing.T) {
	t.Parallel()
	j := runtime.NewMemoryJournal()
	good := Define("valid", "1", func(context.Context, int) (int, error) { return 0, nil })
	var missing *Definition[int, int]
	for _, definitions := range [][]Workflow{{nil}, {missing}, {Define[int, int]("bad", "1", nil)}, {Define("", "1", func(context.Context, int) (int, error) { return 0, nil })}, {good, good}} {
		if _, err := NewService(j, definitions...); err == nil {
			t.Fatal("accepted invalid definitions")
		}
	}
	if _, err := NewService(nil, good); err == nil {
		t.Fatal("accepted nil journal")
	}
}
