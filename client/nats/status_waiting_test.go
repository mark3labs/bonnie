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

// Waiting statuses and queries expose the suspension through the existing
// result/query surfaces. Answers retain the attempt identity on resume.
func TestRootWaitingStatusAndResume(t *testing.T) {
	t.Parallel()
	nc, _ := broker(t)
	c, err := New(nc, Config{RootSubject: "waiting", CreateStream: true})
	if err != nil {
		t.Fatal(err)
	}
	model := fakemodel.New(fakemodel.Call("ask_human", `{"question":"Where?"}`), fakemodel.Say("done"))
	r := runtime.NewRunner(runtime.NewMemoryJournal(), runtime.KitAgent(model.Option(), func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.DisableCoreTools = true
		o.Quiet = true
	}, kit.WithTools(runtime.AskTool())))
	ch, err := transport.New(r, transport.Config{Conn: nc, RootSubject: "waiting", WorkerID: "one", CreateStream: true})
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
	if _, err := c.Submit(ctx, Task{TaskID: "task", Text: "work"}); err != nil {
		t.Fatal(err)
	}
	finished := errors.New("finished")
	var target Target
	err = c.Consume(ctx, func(ctx context.Context, out Outcome) error {
		if out.Error != "" {
			t.Fatalf("outcome %+v", out)
		}
		if out.State == runtime.RunWaiting {
			target = Target{TaskID: out.TaskID, AttemptID: out.AttemptID, RunID: out.RunID, WorkerID: out.WorkerID}
			status, err := c.Status(ctx, target)
			if err != nil || status.Error != "" || status.State != runtime.RunWaiting || status.Suspend == nil {
				t.Fatalf("snapshot %+v %v", status, err)
			}
			_, err = c.Answer(ctx, out, []runtime.InputResponse{{Text: "London"}})
			return err
		}
		if out.State == runtime.RunCompleted {
			return finished
		}
		t.Fatalf("unexpected %+v", out)
		return nil
	})
	if !errors.Is(err, finished) {
		t.Fatal(err)
	}
	waiting := false
	err = c.ConsumeEvents(ctx, func(_ context.Context, ev StatusEvent) error {
		if ev.Target != target {
			t.Fatalf("changed attempt %+v", ev)
		}
		if ev.State == runtime.RunWaiting {
			waiting = true
		}
		if ev.State == runtime.RunCompleted {
			return finished
		}
		return nil
	})
	if !errors.Is(err, finished) || !waiting {
		t.Fatalf("waiting %v error %v", waiting, err)
	}
}
