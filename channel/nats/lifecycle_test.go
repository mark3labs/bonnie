package nats

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

type blockingAgent struct {
	active  *atomic.Int32
	peak    *atomic.Int32
	entered chan struct{}
}

func (a *blockingAgent) PromptResult(ctx context.Context, _ string) (*kit.TurnResult, error) {
	n := a.active.Add(1)
	defer a.active.Add(-1)
	for old := a.peak.Load(); n > old; old = a.peak.Load() {
		if a.peak.CompareAndSwap(old, n) {
			break
		}
	}
	a.entered <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*blockingAgent) InjectSteer(string) {}
func (*blockingAgent) Close() error       { return nil }

// Running and buffered tasks must stop with the lifecycle context. Workers
// never exceed the configured limit, and Shutdown waits for active agents.
func TestWorkerBoundAndCancellation(t *testing.T) {
	t.Parallel()
	nc := testServer(t)
	var active, peak atomic.Int32
	entered := make(chan struct{}, 8)
	r := runtime.NewRunner(runtime.NewMemoryJournal(), func(context.Context, *runtime.Session) (runtime.Agent, error) {
		return &blockingAgent{active: &active, peak: &peak, entered: entered}, nil
	})
	c, err := New(r, Config{Conn: nc, Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results", Concurrency: 2, Buffer: 16})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		send(t, nc, "tasks", Task{TaskID: string(rune('a' + i)), Text: "block"})
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not start")
		}
	}
	cancel()
	deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := c.Shutdown(deadline); err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 || peak.Load() != 2 {
		t.Fatalf("active=%d peak=%d", active.Load(), peak.Load())
	}
}

// A failed result publish is visible to the owner even when delivery cannot
// carry the error. No broker credentials are part of the reported error.
func TestPublishFailureVisible(t *testing.T) {
	t.Parallel()
	nc := testServer(t)
	c, err := New(testRunner(nil), Config{Conn: nc, Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"})
	if err != nil {
		t.Fatal(err)
	}
	c.conn = nc
	nc.Close()
	c.publish(context.Background(), Result{TaskID: "failed"})
	if err := c.Shutdown(context.Background()); err == nil {
		t.Fatal("publish failure was hidden")
	}
}

// A URL connection belongs to the adapter. A fresh adapter shares only the
// journal and rejects a repeated task without sending it to the model.
func TestOwnedConnectionAndDurableDuplicate(t *testing.T) {
	t.Parallel()
	nc := testServer(t)
	journal := runtime.NewMemoryJournal()
	model := fakemodel.New(fakemodel.Say("done"))
	// Use the same factory configuration with a journal that can be reopened.
	factory := runtime.KitAgent(model.Option(), func(o *kit.Options) {
		o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.DisableCoreTools, o.Quiet = true, true, true, true, true, true, true
	})
	firstRunner := runtime.NewRunner(journal, factory)
	cfg := Config{URL: nc.ConnectedUrl(), Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"}
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	first, err := New(firstRunner, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	send(t, nc, "tasks", Task{TaskID: "persisted", Text: "work"})
	completed := receive(t, sub)
	if completed.Error != "" {
		t.Fatalf("first: %+v", completed)
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !first.conn.IsClosed() {
		t.Fatal("owned connection still open")
	}
	second, err := New(runtime.NewRunner(journal, factory), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	send(t, nc, "tasks", Task{TaskID: "persisted", Text: "retry"})
	got := receive(t, sub)
	if got.Error != "duplicate task" || got.RunID != completed.RunID {
		t.Fatalf("restart duplicate: %+v", got)
	}
}
