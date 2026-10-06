package nats

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	transport "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

// A root provisions all durable data streams. Statuses include acceptance and
// state only. Queries still work after Start's readiness deadline has expired.
func TestRootStatusesAndQueries(t *testing.T) {
	t.Parallel()
	nc, _ := broker(t)
	c, err := New(nc, Config{RootSubject: "company.agents.code", CreateStream: true})
	if err != nil {
		t.Fatal(err)
	}
	model := fakemodel.New(fakemodel.Say("private response"))
	r := runtime.NewRunner(runtime.NewMemoryJournal(), runtime.KitAgent(model.Option(), func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.DisableCoreTools = true
		o.Quiet = true
	}))
	ch, err := transport.New(r, transport.Config{Conn: nc, RootSubject: "company.agents.code", WorkerID: "one", CreateStream: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := deadline(t)
	if err := ch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ch.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := c.Submit(ctx, Task{TaskID: "one", Text: "work"}); err != nil {
		t.Fatal(err)
	}
	finished := errors.New("finished")
	var events []StatusEvent
	err = c.ConsumeEvents(ctx, func(ctx context.Context, ev StatusEvent) error {
		events = append(events, ev)
		if ev.State == runtime.RunCompleted {
			return finished
		}
		return nil
	})
	if !errors.Is(err, finished) {
		t.Fatal(err)
	}
	if len(events) < 3 || events[0].Type != "task_accepted" || events[0].Seq != 0 {
		t.Fatalf("events: %+v", events)
	}
	target := events[0].Target
	for i, ev := range events {
		if ev.Target != target || ev.EventID == "" || ev.Time.IsZero() {
			t.Fatalf("event: %+v", ev)
		}
		if i > 0 && ev.Seq <= events[i-1].Seq {
			t.Fatalf("unordered: %+v", events)
		}
	}
	status, err := c.Status(ctx, target)
	if err != nil || status.Error != "" || status.State != runtime.RunCompleted || status.Active {
		t.Fatalf("status %+v: %v", status, err)
	}
	cancelled, err := c.Cancel(ctx, target)
	if err != nil || cancelled.Error == "" || cancelled.CancelRequested {
		t.Fatalf("cancel %+v: %v", cancelled, err)
	}
	wrong := target
	wrong.AttemptID = "wrong"
	status, err = c.Status(ctx, wrong)
	if err != nil || status.Error != "unknown task attempt" {
		t.Fatalf("wrong %+v: %v", status, err)
	}
}

// Root subject defaults and individual overrides have the same meaning in the
// publisher and worker. Invalid roots and overlapping worker routes fail early.
func TestRootValidationAndOverrides(t *testing.T) {
	t.Parallel()
	nc, _ := broker(t)
	for _, root := range []string{"a.*", "a.>", "a..b", ".a", "a ", "a."} {
		if _, err := New(nc, Config{RootSubject: root, CreateStream: true}); err == nil {
			t.Fatalf("accepted root %q", root)
		}
	}
	c, err := New(nc, Config{RootSubject: "tasks", TaskSubject: "custom.tasks", ResultSubject: "custom.results", CreateStream: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.TaskSubject != "custom.tasks" || c.cfg.AnswerSubject != "tasks.answers" || c.cfg.ResultSubject != "custom.results" {
		t.Fatalf("config %+v", c.cfg)
	}
	if _, err := New(nc, Config{RootSubject: "a", EventSubject: "a.answers.worker", CreateStream: true}); err == nil {
		t.Fatal("accepted overlap")
	}
}
