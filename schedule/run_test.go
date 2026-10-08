package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
)

// All combinations must require exactly one execution mode, including Run.
func TestDefinitionExecutionModes(t *testing.T) {
	t.Parallel()
	for mask := range 8 {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			t.Parallel()
			d := Definition{Name: "job", Cron: "* * * * *", TimeZone: "UTC"}
			modes := 0
			if mask&1 != 0 {
				d.Prompt = "prompt"
				modes++
			}
			if mask&2 != 0 {
				d.Prepare = func(context.Context, Fire) ([]Dispatch, error) { return nil, nil }
				modes++
			}
			if mask&4 != 0 {
				d.Run = func(context.Context, Fire) error { return nil }
				modes++
			}
			e, err := New(runtime.NewRunner(runtime.NewMemoryJournal(), nil), nil, d)
			if modes != 1 {
				if err == nil {
					if shutdownErr := e.Shutdown(context.Background()); shutdownErr != nil {
						t.Error(shutdownErr)
					}
					t.Fatal("accepted invalid execution modes")
				}
				if !strings.Contains(err.Error(), "exactly one of Prompt, Prepare, and Run") {
					t.Fatalf("validation error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Functions and prompts must stay out of the public JSON response.
			data, err := json.Marshal(e.List())
			if err != nil {
				t.Fatal(err)
			}
			var defs []map[string]any
			if err := json.Unmarshal(data, &defs); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"Prompt", "Prepare", "Run", "prompt", "prepare", "run"} {
				if _, ok := defs[0][key]; ok {
					t.Fatalf("execution mode serialized: %s", data)
				}
			}
		})
	}
}

// A callback result is terminal only after its snapshot is saved. Reopen SQLite
// with a new Runner and Engine to test recovery with no process-local state.
func TestRunRecoverySQLite(t *testing.T) {
	t.Parallel()
	for _, callbackFails := range []bool{false, true} {
		for _, saveFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("callback_error=%t/save_error=%t", callbackFails, saveFails), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				dir := t.TempDir()
				j, err := runtime.OpenSQLiteJournal(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := j.Close(); err != nil {
						t.Errorf("close journal: %v", err)
					}
				})
				callbackErr := errors.New("host action failed")
				at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.FixedZone("host", 3600))
				wantFire := Fire{Name: "host", ID: stableID("host", at), ScheduledAt: at.UTC(), Kind: "external"}
				calls := make(chan Fire, 4)
				def := Definition{Name: "host", Cron: "* * * * *", TimeZone: "UTC", Revision: "v1", Destination: Destination{Channel: "tracked", Target: "unused"}, Run: func(ctx context.Context, fire Fire) error {
					if ctx == nil || ctx.Err() != nil {
						return errors.New("invalid callback context")
					}
					calls <- fire
					if callbackFails {
						return callbackErr
					}
					return nil
				}}
				agent := channeltest.NewScriptAgent()
				receiver := &trackedFake{}
				broken := &rejectRunResultJournal{Journal: j, reject: saveFails}
				newEngine := func(journal runtime.Journal) *Engine {
					e, err := New(runtime.NewRunner(journal, agent.Factory()), map[string]channel.TrackedReceiver{"tracked": receiver}, def)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := e.Shutdown(ctx); err != nil {
							t.Errorf("shutdown engine: %v", err)
						}
					})
					return e
				}
				e := newEngine(broken)
				accepted, err := e.Trigger(ctx, "host", "", at, "external")
				if err != nil {
					t.Fatal(err)
				}
				if accepted.Fire != wantFire || accepted.State != Pending {
					t.Fatalf("accepted occurrence = %+v", accepted)
				}
				if err := e.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
				wantState := Completed
				if callbackFails {
					wantState = Failed
				}
				assertHistory := func(e *Engine, state State) {
					t.Helper()
					history, err := e.History(ctx, "host")
					if err != nil {
						t.Fatal(err)
					}
					if len(history) != 1 {
						t.Fatalf("history = %+v", history)
					}
					o := history[0]
					if o.Fire != wantFire || o.Revision != "v1" || o.State != state || len(o.Work) != 0 || o.Prepared != (state == Completed) {
						t.Fatalf("occurrence = %+v, want state %s", o, state)
					}
					wantError := ""
					if state == Failed {
						wantError = callbackErr.Error()
					}
					if o.Error != wantError {
						t.Fatalf("occurrence error = %q, want %q", o.Error, wantError)
					}
				}
				if saveFails {
					assertHistory(e, Pending)
					if broken.rejected.Load() != 1 {
						t.Fatal("did not reject the callback result write")
					}
				} else {
					assertHistory(e, wantState)
				}
				if err := j.Close(); err != nil {
					t.Fatal(err)
				}
				j, err = runtime.OpenSQLiteJournal(dir)
				if err != nil {
					t.Fatal(err)
				}
				e2 := newEngine(j)
				if err := e2.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				waitState(t, e2, wantFire.ID, wantState)
				assertHistory(e2, wantState)
				// A repeated trigger and reconciliation must not repeat a saved result.
				if _, err := e2.Trigger(ctx, "host", "", at, "external"); err != nil {
					t.Fatal(err)
				}
				if err := e2.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				if err := e2.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
				wantCalls := 1
				if saveFails {
					wantCalls = 2
				}
				if len(calls) != wantCalls {
					t.Fatalf("callback calls = %d, want %d", len(calls), wantCalls)
				}
				for range wantCalls {
					if fire := <-calls; fire != wantFire {
						t.Fatalf("callback fire = %+v, want %+v", fire, wantFire)
					}
				}
				if agent.Calls() != 0 {
					t.Fatalf("agent calls = %d, want 0", agent.Calls())
				}
				receiver.mu.Lock()
				runs, deliveries := receiver.runs, receiver.delivers
				receiver.mu.Unlock()
				if runs != 0 || deliveries != 0 {
					t.Fatalf("channel runs = %d, deliveries = %d, want 0", runs, deliveries)
				}
			})
		}
	}
}

// Reject the write after Run returns to model a crash before saving its result.
// Leave the accepted Pending snapshot intact for the next engine.
type rejectRunResultJournal struct {
	runtime.Journal
	reject   bool
	rejected atomic.Int32
}

func (j *rejectRunResultJournal) Append(ctx context.Context, rec runtime.Record) (int, error) {
	if j.reject && rec.ExtType == "schedule.occurrence" {
		var o Occurrence
		if err := json.Unmarshal(rec.Payload, &o); err != nil {
			return 0, err
		}
		if o.State == Completed || o.State == Failed {
			j.rejected.Add(1)
			return 0, errors.New("crash before callback result save")
		}
	}
	return j.Journal.Append(ctx, rec)
}
