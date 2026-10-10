package nats

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/runtime"
)

// A second Runner shares only the journal. Acceptance and saved state changes
// that were never published are recovered, in order, without executing work.
func TestStatusRecoveryAcrossRunners(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	journal := runtime.NewMemoryJournal()
	cfg := Config{Conn: nc, RootSubject: "factory", AgentID: "one", CreateStream: true}
	first, err := New(runtime.NewRunner(journal, nil), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := first.admitTask(ctx, Result{TaskID: "task", AttemptID: "attempt"}, "run"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []runtime.RunState{runtime.RunRunning, runtime.RunCompleted} {
		if _, err := journal.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordState, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	second := jsChannel(t, runtime.NewRunner(journal, nil), cfg)
	sub, err := js.PullSubscribe("factory.events", "test", gonats.BindStream(DefaultEventStreamName("factory.events")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Error(err)
		}
	}()
	for i, state := range []runtime.RunState{runtime.RunPending, runtime.RunRunning, runtime.RunCompleted} {
		msgs, err := sub.Fetch(1, gonats.Context(ctx))
		if err != nil {
			t.Fatal(err)
		}
		var ev StatusEvent
		if err := json.Unmarshal(msgs[0].Data, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.State != state || ev.Seq != i {
			t.Fatalf("event %+v", ev)
		}
		if err := msgs[0].AckSync(gonats.Context(ctx)); err != nil {
			t.Fatal(err)
		}
	}
	// A later sweep does not publish acknowledged journal events again.
	if err := second.flushStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate a publish confirmed before its local cursor commit. Reusing
	// the event identity lets the broker suppress it within its duplicate window.
	admissions, _, err := second.admissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := admissions[0]
	if err := second.publishStatus(ctx, StatusEvent{Version: 1, Target: a.Target, Type: "task_accepted", State: runtime.RunPending, Time: a.Time}); err != nil {
		t.Fatal(err)
	}
	info, err := js.StreamInfo(DefaultEventStreamName("factory.events"))
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 3 {
		t.Fatalf("messages %d", info.State.Msgs)
	}
}

// Control subscriptions do not share execution slots. Cancellation works when
// all execution slots are occupied, and its final state is a separate event.
func TestStatusCancelBusyAgent(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	var active, peak atomic.Int32
	entered := make(chan struct{}, 1)
	r := runtime.NewRunner(runtime.NewMemoryJournal(), func(context.Context, *runtime.Session) (runtime.Agent, error) {
		return &blockingAgent{active: &active, peak: &peak, entered: entered}, nil
	})
	c := jsChannel(t, r, Config{Conn: nc, RootSubject: "busy", AgentID: "one", Concurrency: 1, CreateStream: true})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	jsSend(t, js, "busy.tasks", Task{Version: 1, TaskID: "task", Text: "work"})
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	admissions, _, err := c.admissions(ctx)
	if err != nil || len(admissions) != 1 {
		t.Fatalf("admissions %+v %v", admissions, err)
	}
	target := admissions[0].Target
	data, err := json.Marshal(StatusRequest{Version: 1, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := nc.RequestWithContext(ctx, "busy.commands.one", data)
	if err != nil {
		t.Fatal(err)
	}
	var status Status
	if err := json.Unmarshal(msg.Data, &status); err != nil {
		t.Fatal(err)
	}
	if status.Error != "" || !status.CancelRequested {
		t.Fatalf("reply %+v", status)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := c.inspect(ctx, StatusRequest{Version: 1, Target: target}, false)
		if snapshot.State == runtime.RunCancelled && !snapshot.Active {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("not cancelled: %+v", snapshot)
		case <-ticker.C:
		}
	}
}
