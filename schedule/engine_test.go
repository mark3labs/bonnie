package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestEngineBackgroundTriggerAndProvenance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := runtime.NewMemoryJournal()
	agent := channeltest.NewScriptAgent()
	runner := runtime.NewRunner(j, agent.Factory())
	e, err := New(runner, nil, Definition{Name: "daily", Cron: "0 9 * * *", TimeZone: "UTC", Revision: "v1", Prompt: "create a report"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := e.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	first, err := e.Trigger(ctx, "daily", "once", at, "manual")
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.Trigger(ctx, "daily", "once", at, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if first.Fire.ID != again.Fire.ID || first.Fire.ID != "once" {
		t.Fatalf("idempotent Trigger = %+v, %+v", first, again)
	}
	if _, err := e.Trigger(ctx, "daily", "once", at.Add(time.Minute), "manual"); err == nil {
		t.Fatal("Trigger accepted conflicting occurrence ID")
	}
	waitState(t, e, "once", Completed)
	if agent.Calls() != 1 {
		t.Fatalf("model calls = %d, want 1", agent.Calls())
	}
	history, err := e.History(ctx, "daily")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || len(history[0].Work) != 1 {
		t.Fatalf("history = %+v", history)
	}
	recs, err := j.Replay(ctx, history[0].Work[0].Receipt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var trigger *runtime.Trigger
	for _, rec := range recs {
		if rec.Kind == runtime.RecordTrigger {
			var got runtime.Trigger
			if err := jsonUnmarshal(rec.Payload, &got); err != nil {
				t.Fatal(err)
			}
			trigger = &got
		}
	}
	if trigger == nil || trigger.ScheduleName != "daily" || trigger.OccurrenceID != "once" || trigger.Kind != "manual" || !trigger.ScheduledAt.Equal(at) {
		t.Fatalf("run trigger provenance = %+v", trigger)
	}
}

func TestEngineValidatesDefinitions(t *testing.T) {
	t.Parallel()
	r := runtime.NewRunner(runtime.NewMemoryJournal(), channeltest.NewScriptAgent().Factory())
	cases := []Definition{
		{Name: "", Cron: "* * * * *", TimeZone: "UTC", Prompt: "x"},
		{Name: "bad/name", Cron: "* * * * *", TimeZone: "UTC", Prompt: "x"},
		{Name: "both", Cron: "* * * * *", TimeZone: "UTC", Prompt: "x", Prepare: func(context.Context, Fire) ([]Dispatch, error) { return nil, nil }},
		{Name: "neither", Cron: "* * * * *", TimeZone: "UTC"},
		{Name: "bad-cron", Cron: "bad", TimeZone: "UTC", Prompt: "x"},
		{Name: "bad-zone", Cron: "* * * * *", TimeZone: "bad", Prompt: "x"},
		{Name: "bad-catchup", Cron: "* * * * *", TimeZone: "UTC", Prompt: "x", CatchUp: "all"},
		{Name: "bad-overlap", Cron: "* * * * *", TimeZone: "UTC", Prompt: "x", Overlap: "queue"},
	}
	for _, d := range cases {
		if _, err := New(r, nil, d); err == nil {
			t.Errorf("New accepted invalid definition %+v", d)
		}
	}
	if _, err := New(r, nil, Definition{Name: "dupe", Cron: "* * * * *", TimeZone: "UTC", Prompt: "a"}, Definition{Name: "dupe", Cron: "* * * * *", TimeZone: "UTC", Prompt: "b"}); err == nil {
		t.Fatal("New accepted duplicate names")
	}
	if _, err := New(r, nil, Definition{Name: "missing-channel", Cron: "* * * * *", TimeZone: "UTC", Prompt: "x", Destination: Destination{Channel: "absent"}}); err == nil {
		t.Fatal("New accepted an unmounted tracked destination")
	}
}

func TestEnginePrepareNoWorkAndOverlapSkipWaiting(t *testing.T) {
	t.Parallel()
	t.Run("prepare no work", func(t *testing.T) {
		t.Parallel()
		agent := channeltest.NewScriptAgent()
		e, err := New(runtime.NewRunner(runtime.NewMemoryJournal(), agent.Factory()), nil, Definition{Name: "empty", Cron: "* * * * *", TimeZone: "UTC", Prepare: func(context.Context, Fire) ([]Dispatch, error) { return nil, nil }})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := e.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		if _, err := e.Trigger(context.Background(), "empty", "empty-id", time.Now(), "manual"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool {
			history, err := e.History(context.Background(), "empty")
			return err == nil && len(history) == 1 && history[0].State == Skipped
		})
		if agent.Calls() != 0 {
			t.Fatalf("agent calls = %d, want 0", agent.Calls())
		}
	})
	t.Run("waiting blocks overlap", func(t *testing.T) {
		t.Parallel()
		agent := channeltest.NewScriptAgent()
		agent.Say(&kit.TurnResult{Response: "question", FinalValue: runtime.SuspendRequest{Kind: "ask", Prompt: "question"}})
		e, err := New(runtime.NewRunner(runtime.NewMemoryJournal(), agent.Factory()), nil, Definition{Name: "waiter", Cron: "* * * * *", TimeZone: "UTC", Prompt: "ask"})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := e.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		at := time.Now()
		if _, err := e.Trigger(context.Background(), "waiter", "first", at, "manual"); err != nil {
			t.Fatal(err)
		}
		waitState(t, e, "first", Waiting)
		next, err := e.Trigger(context.Background(), "waiter", "second", at.Add(time.Minute), "manual")
		if err != nil {
			t.Fatal(err)
		}
		if next.State != Skipped {
			t.Fatalf("overlap state = %s, want skipped", next.State)
		}
		if agent.Calls() != 1 {
			t.Fatalf("agent calls = %d, want 1", agent.Calls())
		}
	})
}

type trackedFake struct {
	mu        sync.Mutex
	runs      int
	delivers  int
	fail      bool
	delivered chan struct{}
}

func (f *trackedFake) PrepareDispatch(_ context.Context, id string, target any) (channel.DispatchReceipt, error) {
	return channel.DispatchReceipt{DispatchID: id, RunID: id, Address: "target"}, nil
}
func (f *trackedFake) RunDispatch(_ context.Context, r channel.DispatchReceipt, text string, opts channel.SendOptions) (*runtime.Run, error) {
	f.mu.Lock()
	f.runs++
	f.mu.Unlock()
	return &runtime.Run{ID: r.RunID, State: runtime.RunCompleted, Response: "result"}, nil
}
func (f *trackedFake) DeliverDispatch(_ context.Context, _ channel.DispatchReceipt, _ *runtime.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivers++
	if f.fail {
		f.fail = false
		return errors.New("delivery offline")
	}
	if f.delivered != nil {
		select {
		case f.delivered <- struct{}{}:
		default:
		}
	}
	return nil
}

func TestEngineRetryDeliveryAfterSQLiteReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	j, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	agent := channeltest.NewScriptAgent()
	receiver := &trackedFake{fail: true}
	def := Definition{Name: "outbox", Cron: "* * * * *", TimeZone: "UTC", Prompt: "deliver", Destination: Destination{Channel: "track", Target: "target"}}
	e, err := New(runtime.NewRunner(j, agent.Factory()), map[string]channel.TrackedReceiver{"track": receiver}, def)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Trigger(ctx, "outbox", "delivery", time.Now(), "manual"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		h, _ := e.History(ctx, "outbox")
		return len(h) == 1 && len(h[0].Work) == 1 && h[0].Work[0].Result != nil && h[0].State == Running
	})
	if err := e.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j2, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := j2.Close(); err != nil {
			t.Error(err)
		}
	}()
	e2, err := New(runtime.NewRunner(j2, agent.Factory()), map[string]channel.TrackedReceiver{"track": receiver}, def)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := e2.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	waitFor(t, func() bool {
		if err := e2.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		history, err := e2.History(ctx, "outbox")
		if err != nil {
			t.Fatal(err)
		}
		return len(history) == 1 && history[0].State == Completed
	})
	h, err := e2.History(ctx, "outbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || !h[0].Work[0].Delivered {
		t.Fatalf("delivery state = %+v", h)
	}
	receiver.mu.Lock()
	runs, deliveries := receiver.runs, receiver.delivers
	receiver.mu.Unlock()
	if runs != 1 {
		t.Fatalf("agent dispatch runs = %d, want exactly 1", runs)
	}
	if deliveries != 2 {
		t.Fatalf("delivery attempts = %d, want failed attempt and retry", deliveries)
	}
	if agent.Calls() != 0 {
		t.Fatalf("background agent unexpectedly called for channel dispatch: %d", agent.Calls())
	}
}

func waitState(t *testing.T, e *Engine, id string, want State) {
	t.Helper()
	waitFor(t, func() bool {
		h, err := e.History(context.Background(), "")
		if err != nil {
			return false
		}
		for _, o := range h {
			if o.Fire.ID == id {
				return o.State == want
			}
		}
		return false
	})
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for schedule work")
}

// jsonUnmarshal keeps this test focused on the public journal payload.
func jsonUnmarshal(data []byte, dst any) error { return json.Unmarshal(data, dst) }
