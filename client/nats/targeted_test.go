package nats

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	transport "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

// A target can be offline at publication. Other workers must not take its
// task. Shared work still runs, and deduplication is separate for each route.
func TestTargetedDelivery(t *testing.T) {
	t.Parallel()
	nc, _ := broker(t)
	cfg := Config{RootSubject: "agent", TargetedTasks: true, CreateStream: true}
	c, err := New(nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := nc.SubscribeSync("agent.results")
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	start := func(worker string) {
		t.Helper()
		model := fakemodel.New(fakemodel.Say(worker), fakemodel.Say(worker))
		r := runtime.NewRunner(runtime.NewMemoryJournal(), runtime.KitAgent(model.Option(), func(o *kit.Options) {
			o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents = true, true, true, true, true
			o.DisableCoreTools, o.Quiet = true, true
		}))
		ch, err := transport.New(r, transport.Config{Conn: nc, RootSubject: "agent", WorkerID: worker, TargetedTasks: true, Concurrency: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := ch.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := ch.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	read := func() Outcome {
		t.Helper()
		msg, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var out Outcome
		if err := json.Unmarshal(msg.Data, &out); err != nil {
			t.Fatal(err)
		}
		if out.Error != "" || out.State != runtime.RunCompleted {
			t.Fatalf("outcome: %+v", out)
		}
		return out
	}
	start("one")
	task := Task{TaskID: "same", Text: "work"}
	if _, err := c.SubmitTo(t.Context(), "two", task); err != nil {
		t.Fatal(err)
	}
	if receipt, err := c.SubmitTo(t.Context(), "two", task); err != nil || !receipt.Duplicate {
		t.Fatalf("retry: %+v %v", receipt, err)
	}
	if receipt, err := c.Submit(t.Context(), task); err != nil || receipt.Duplicate {
		t.Fatalf("shared: %+v %v", receipt, err)
	}
	if out := read(); out.WorkerID != "one" {
		t.Fatalf("shared went to %+v", out)
	}
	if _, err := sub.NextMsg(600 * time.Millisecond); err == nil {
		t.Fatal("offline target's task was taken by another worker")
	}
	start("two")
	if out := read(); out.WorkerID != "two" {
		t.Fatalf("target went to %+v", out)
	}
	if receipt, err := c.SubmitTo(t.Context(), "one", task); err != nil || receipt.Duplicate {
		t.Fatalf("different target: %+v %v", receipt, err)
	}
	if out := read(); out.WorkerID != "one" {
		t.Fatalf("target went to %+v", out)
	}
	for _, worker := range []string{"", "bad.worker", "*"} {
		if _, err := c.SubmitTo(t.Context(), worker, task); err == nil {
			t.Fatalf("accepted worker %q", worker)
		}
	}
	c.cfg.TargetedTasks = false
	if _, err := c.SubmitTo(t.Context(), "one", task); err == nil {
		t.Fatal("accepted target without opt-in")
	}
}
