package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
)

// Recovery must execute an accepted callback even if the first process stopped
// before calling it. Empty Prepare results must still be Skipped, not Completed.
func TestRecoverPendingHostWork(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"run", "empty prepare"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			j := runtime.NewMemoryJournal()
			calls := make(chan Fire, 2)
			d := Definition{Name: "host", Cron: "* * * * *", TimeZone: "UTC"}
			want := Completed
			if mode == "run" {
				d.Run = func(_ context.Context, f Fire) error { calls <- f; return nil }
			} else {
				want = Skipped
				d.Prepare = func(_ context.Context, f Fire) ([]Dispatch, error) { calls <- f; return nil, nil }
			}
			e, err := New(runtime.NewRunner(j, nil), nil, d)
			if err != nil {
				t.Fatal(err)
			}
			fire := Fire{Name: "host", ID: "pending-host", ScheduledAt: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC), Kind: "cron"}
			if err := e.save(ctx, Occurrence{Fire: fire, State: Pending}); err != nil {
				t.Fatal(err)
			}
			if err := e.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			agent := channeltest.NewScriptAgent()
			e2, err := New(runtime.NewRunner(j, agent.Factory()), nil, d)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := e2.Shutdown(ctx); err != nil {
					t.Errorf("shutdown engine: %v", err)
				}
			})
			if err := e2.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			waitState(t, e2, fire.ID, want)
			if err := e2.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if err := e2.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			h, err := e2.History(ctx, "host")
			if err != nil {
				t.Fatal(err)
			}
			if len(h) != 1 || !h[0].Prepared || len(h[0].Work) != 0 || h[0].Error != "" {
				t.Fatalf("history = %+v", h)
			}
			if len(calls) != 1 {
				t.Fatalf("callback calls = %d, want 1", len(calls))
			}
			if got := <-calls; got != fire {
				t.Fatalf("fire = %+v, want %+v", got, fire)
			}
			if agent.Calls() != 0 {
				t.Fatalf("agent calls = %d, want 0", agent.Calls())
			}
		})
	}
}
