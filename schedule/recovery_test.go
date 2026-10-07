package schedule

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
)

func TestRecoverPreparedOccurrenceSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	j, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	prepared := 0
	def := Definition{Name: "prepared", Cron: "* * * * *", TimeZone: "UTC", Prepare: func(context.Context, Fire) ([]Dispatch, error) {
		prepared++
		return []Dispatch{{Key: "saved", Input: runtime.Input{Text: "saved"}}}, nil
	}}
	mk := func(j runtime.Journal) *Engine {
		e, err := New(runtime.NewRunner(j, channeltest.NewScriptAgent().Factory()), nil, def)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := mk(j)
	if _, err = e.Trigger(ctx, "prepared", "id", time.Now(), "manual"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		h, _ := e.History(ctx, "prepared")
		return len(h) == 1 && h[0].Prepared && h[0].Work[0].Result != nil
	})
	if err = e.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Errorf("close journal: %v", err)
		}
	})
	e = mk(j)
	t.Cleanup(func() {
		if err := e.Shutdown(ctx); err != nil {
			t.Errorf("shutdown engine: %v", err)
		}
	})
	if err = e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, "id", Completed)
	if prepared != 1 {
		t.Fatalf("Prepare calls=%d, want 1", prepared)
	}
}

func TestRecoveryUsesSavedTriggerBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := runtime.NewMemoryJournal()
	receiver := &trackedFake{}
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	did := stableID("occurrence/key", at)
	o := Occurrence{Fire: Fire{Name: "job", ID: "occurrence", ScheduledAt: at, Kind: "manual"}, State: Running, Prepared: true, Work: []Work{{Dispatch: Dispatch{Key: "key", Input: runtime.Input{Text: "scheduled"}, Destination: Destination{Channel: "tracked", Target: "target"}}, Receipt: channel.DispatchReceipt{DispatchID: did, RunID: did}, Reserved: true}}}
	payload, _ := json.Marshal(o)
	if _, err := j.Append(ctx, runtime.Record{RunID: journalID, Kind: runtime.RecordExtensionData, ExtType: "schedule.occurrence", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	trigger, _ := json.Marshal(&runtime.Trigger{DispatchID: did})
	for _, r := range []runtime.Record{{RunID: did, Kind: runtime.RecordTrigger, Payload: trigger}, {RunID: did, Kind: runtime.RecordState, State: runtime.RunCompleted}, {RunID: did, Kind: runtime.RecordTrigger, Payload: json.RawMessage("null")}, {RunID: did, Kind: runtime.RecordState, State: runtime.RunCompleted}} {
		if _, err := j.Append(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	e, err := New(runtime.NewRunner(j, nil), map[string]channel.TrackedReceiver{"tracked": receiver}, Definition{Name: "job", Cron: "* * * * *", TimeZone: "UTC", Prompt: "unused", Destination: Destination{Channel: "tracked", Target: "target"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Shutdown(ctx); err != nil {
			t.Errorf("shutdown engine: %v", err)
		}
	})
	if err = e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, "occurrence", Completed)
	h, err := e.History(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h[0].Work[0].Result == nil {
		t.Fatalf("recovered history=%+v", h)
	}
	receiver.mu.Lock()
	runs := receiver.runs
	receiver.mu.Unlock()
	if runs != 0 {
		t.Fatalf("dispatch runs=%d, want 0", runs)
	}
}

func TestTickSameMinuteAndCatchUp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, catch string
		seed        bool
		want        int
	}{{"same minute", "", false, 1}, {"latest", "latest", true, 2}, {"skip", "skip", true, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e, err := New(runtime.NewRunner(runtime.NewMemoryJournal(), channeltest.NewScriptAgent().Factory()), nil, Definition{Name: "daily", Cron: "0 9 * * *", TimeZone: "UTC", CatchUp: tc.catch, Prompt: "x"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := e.Shutdown(ctx); err != nil {
					t.Errorf("shutdown engine: %v", err)
				}
			})
			at := time.Date(2026, 10, 7, 9, 0, 30, 0, time.UTC)
			if tc.seed {
				if _, err = e.Trigger(ctx, "daily", "old", time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), "cron"); err != nil {
					t.Fatal(err)
				}
				waitState(t, e, "old", Completed)
			}
			if err = e.Tick(ctx, at); err != nil {
				t.Fatal(err)
			}
			if err = e.Tick(ctx, at); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { h, _ := e.History(ctx, "daily"); return len(h) >= tc.want })
			h, _ := e.History(ctx, "daily")
			if len(h) != tc.want {
				t.Fatalf("occurrences=%d, want %d: %+v", len(h), tc.want, h)
			}
			if tc.seed && tc.catch == "skip" && h[len(h)-1].Fire.ScheduledAt.Sub(h[0].Fire.ScheduledAt) < 24*time.Hour {
				t.Fatalf("skip catch-up fired old occurrence: %+v", h)
			}
		})
	}
}

func TestParseScheduleDST(t *testing.T) {
	t.Parallel()
	s, loc, err := parseSchedule("30 2 * * *", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	next := s.Next(time.Date(2026, 3, 8, 0, 0, 0, 0, loc))
	if next.Day() != 9 || next.Hour() != 2 {
		t.Fatalf("missing DST time next=%v", next)
	}
	s, loc, err = parseSchedule("30 1 * * *", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	first := s.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, loc))
	second := s.Next(first)
	if first.Hour() != 1 || second.Hour() != 1 || first.Equal(second) {
		t.Fatalf("repeated DST times=%v, %v", first, second)
	}
}
