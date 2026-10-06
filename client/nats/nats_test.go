package nats

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats-server/v2/server"
	gonats "github.com/nats-io/nats.go"

	transport "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

func broker(t *testing.T) (*gonats.Conn, gonats.JetStreamContext) {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	nc, err := gonats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	return nc, js
}
func config() Config {
	return Config{TaskSubject: "tasks", ResultSubject: "results", AnswerSubject: "answers", ResultStream: "OUTPUT", ResultConsumer: "reader", CreateStream: true}
}
func client(t *testing.T, nc *gonats.Conn) *Client {
	t.Helper()
	c, err := New(nc, config())
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func publishResult(t *testing.T, js gonats.JetStreamContext, result Result) {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish("results", data); err != nil {
		t.Fatal(err)
	}
}
func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The real channel runs a hermetic Kit, waits for an answer, and resumes.
func TestEndToEnd(t *testing.T) {
	t.Parallel()
	nc, js := broker(t)
	c := client(t, nc)
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
	ch, err := transport.New(r, transport.Config{Conn: nc, Subject: "tasks", ResultSubject: "results", AnswerSubject: "answers", Stream: "INPUT", Consumer: "workers", WorkerID: "one", CreateStream: true, Concurrency: 1})
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
	task := Task{Version: 1, TaskID: "task/one", Text: "work"}
	receipt, err := c.Submit(ctx, task)
	if err != nil || receipt.TaskID != task.TaskID || receipt.Stream != "INPUT" || receipt.Sequence == 0 {
		t.Fatalf("receipt %+v: %v", receipt, err)
	}
	retry, err := c.Submit(ctx, task)
	if err != nil || !retry.Duplicate || retry.Sequence != receipt.Sequence {
		t.Fatalf("retry %+v: %v", retry, err)
	}
	finished := errors.New("finished")
	count := 0
	err = c.Consume(ctx, func(ctx context.Context, result Outcome) error {
		count++
		if result.TaskID != task.TaskID || result.Error != "" {
			t.Errorf("result %+v", result)
		}
		if result.State == runtime.RunWaiting {
			responses := []runtime.InputResponse{{Text: "here"}}
			a, err := c.Answer(ctx, result, responses)
			if err != nil {
				return err
			}
			b, err := c.Answer(ctx, result, responses)
			if err != nil {
				return err
			}
			if !b.Duplicate || a.Sequence != b.Sequence {
				t.Errorf("answer receipts %+v %+v", a, b)
			}
			return nil
		}
		if result.State != runtime.RunCompleted || result.Response != "done" {
			t.Errorf("completed %+v", result)
		}
		return finished
	})
	if !errors.Is(err, finished) || count != 2 {
		t.Fatalf("consume count %d: %v", count, err)
	}
	si, err := js.StreamInfo("INPUT")
	if err != nil || si.State.Msgs != 2 {
		t.Fatalf("input stream %+v: %v", si, err)
	}
}

// A new client binds the same durable after handler failure. No result is lost.
func TestDurableRetry(t *testing.T) {
	t.Parallel()
	nc, js := broker(t)
	c := client(t, nc)
	publishResult(t, js, Result{Version: 1, TaskID: "a", Response: "ok"})
	failure := errors.New("handler failed")
	if err := c.Consume(deadline(t), func(context.Context, Outcome) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	cfg := config()
	cfg.CreateStream = false
	next, err := New(nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(deadline(t))
	calls := 0
	err = next.Consume(ctx, func(_ context.Context, o Outcome) error {
		calls++
		if o.TaskID != "a" {
			t.Errorf("outcome %+v", o)
		}
		// Cancel after AckSync has time to complete.
		time.AfterFunc(100*time.Millisecond, cancel)
		return nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls %d: %v", calls, err)
	}
	ci, err := js.ConsumerInfo("OUTPUT", "reader")
	if err != nil || ci.NumAckPending != 0 || ci.AckFloor.Stream != 1 {
		t.Fatalf("consumer %+v: %v", ci, err)
	}
	if nc.IsClosed() {
		t.Fatal("client closed caller connection")
	}
}

// A second puller must not receive the result during a long handler.
func TestLongHandler(t *testing.T) {
	t.Parallel()
	nc, js := broker(t)
	cfg := config()
	if _, err := js.AddStream(&gonats.StreamConfig{Name: "OUTPUT", Subjects: []string{"results"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddConsumer("OUTPUT", &gonats.ConsumerConfig{Durable: "reader", FilterSubject: "results", AckPolicy: gonats.AckExplicitPolicy, AckWait: 150 * time.Millisecond, MaxAckPending: 1}); err != nil {
		t.Fatal(err)
	}
	c, err := New(nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	publishResult(t, js, Result{Version: 1, TaskID: "a"})
	other, err := js.PullSubscribe("results", "reader", gonats.Bind("OUTPUT", "reader"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := other.Unsubscribe(); err != nil {
			t.Error(err)
		}
	}()
	finished := errors.New("done")
	err = c.Consume(deadline(t), func(context.Context, Outcome) error {
		_, err := other.Fetch(1, gonats.MaxWait(650*time.Millisecond))
		if !errors.Is(err, gonats.ErrTimeout) {
			t.Errorf("result redelivered during handler: %v", err)
		}
		return finished
	})
	if !errors.Is(err, finished) {
		t.Fatal(err)
	}
	ci, err := js.ConsumerInfo("OUTPUT", "reader")
	if err != nil || ci.NumRedelivered != 0 {
		t.Fatalf("consumer %+v: %v", ci, err)
	}
}

func TestValidationAndBrokerErrors(t *testing.T) {
	t.Parallel()
	nc, js := broker(t)
	cfg := config()
	cfg.CreateStream = false
	if _, err := New(nc, cfg); !errors.Is(err, gonats.ErrStreamNotFound) {
		t.Fatalf("missing stream: %v", err)
	}
	if _, err := New(nil, config()); err == nil {
		t.Fatal("nil connection accepted")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.TaskSubject = "tasks.*" },
		func(c *Config) { c.AnswerSubject = "bad..base" },
		func(c *Config) { c.ResultSubject = "answers.one" },
		func(c *Config) { c.ResultStream = "bad.name" },
		func(c *Config) { c.ResultConsumer = "bad.name" },
	} {
		cfg := config()
		mutate(&cfg)
		if _, err := New(nc, cfg); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	c := client(t, nc)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Submit(ctx, Task{Version: 1, TaskID: "a", Text: "work"}); err == nil {
		t.Fatal("publish without input stream succeeded")
	}
	for _, task := range []Task{{Version: 2, TaskID: "a", Text: "work"}, {Version: -1, TaskID: "a", Text: "work"}, {Version: 1, Text: "work"}, {Version: 1, TaskID: "a", Text: " "}} {
		if _, err := c.Submit(t.Context(), task); err == nil {
			t.Fatalf("accepted %+v", task)
		}
	}
	waiting := Outcome{Version: 1, TaskID: "a", RunID: "run", WorkerID: "one", AnswerSubject: "answers.one", State: runtime.RunWaiting, Suspend: &runtime.SuspendRequest{ToolCallID: "call"}}
	for _, mutate := range []func(*Outcome){
		func(o *Outcome) { o.AnswerSubject = "tasks" },
		func(o *Outcome) { o.WorkerID = "one.other"; o.AnswerSubject = "answers.one.other" },
		func(o *Outcome) { o.WorkerID = "*"; o.AnswerSubject = "answers.*" },
		func(o *Outcome) { o.State = runtime.RunCompleted },
	} {
		bad := waiting
		mutate(&bad)
		if _, err := c.Answer(t.Context(), bad, []runtime.InputResponse{{Text: "yes"}}); err == nil {
			t.Fatalf("accepted route %+v", bad)
		}
	}
	answerCtx, stop := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer stop()
	if _, err := c.Answer(answerCtx, waiting, []runtime.InputResponse{{Text: "yes"}}); err == nil {
		t.Fatal("answer without stream succeeded")
	}
	if _, err := js.AddStream(&gonats.StreamConfig{Name: "INPUT", Subjects: []string{"tasks", "answers.*"}}); err != nil {
		t.Fatal(err)
	}
	nc.Close()
	if _, err := c.Submit(t.Context(), Task{Version: 1, TaskID: "a", Text: "work"}); err == nil {
		t.Fatal("closed connection accepted")
	}
	if err := c.Consume(t.Context(), func(context.Context, Outcome) error { return nil }); err == nil {
		t.Fatal("consume closed connection succeeded")
	}
}

// JetStream deduplicates across a stream, not just within each subject.
// Version 0 and version 1 retries must have the same identity and wire version.
func TestSubmitSharedStream(t *testing.T) {
	t.Parallel()
	nc, js := broker(t)
	if _, err := js.AddStream(&gonats.StreamConfig{Name: "INPUT", Subjects: []string{"tasks.one", "tasks.two"}}); err != nil {
		t.Fatal(err)
	}
	ctx := deadline(t)
	var ids []string
	for _, subject := range []string{"tasks.one", "tasks.two"} {
		cfg := config()
		cfg.TaskSubject = subject
		c, err := New(nc, cfg)
		if err != nil {
			t.Fatal(err)
		}
		task := Task{TaskID: "same", Text: "work"}
		first, err := c.Submit(ctx, task)
		if err != nil || first.Duplicate || first.Stream != "INPUT" {
			t.Fatalf("%s first %+v: %v", subject, first, err)
		}
		if task.Version != 0 {
			t.Fatal("Submit changed the caller's task")
		}
		for _, version := range []int{0, 1} {
			task.Version = version
			retry, err := c.Submit(ctx, task)
			if err != nil || !retry.Duplicate || retry.Sequence != first.Sequence {
				t.Fatalf("%s version %d retry %+v: %v", subject, version, retry, err)
			}
		}
		msg, err := js.GetMsg("INPUT", first.Sequence)
		if err != nil {
			t.Fatal(err)
		}
		var stored Task
		if err := json.Unmarshal(msg.Data, &stored); err != nil {
			t.Fatal(err)
		}
		if msg.Subject != subject || stored.Version != 1 || stored.TaskID != task.TaskID || stored.Text != task.Text {
			t.Fatalf("stored %s %+v", msg.Subject, stored)
		}
		ids = append(ids, msg.Header.Get(gonats.MsgIdHdr))
	}
	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("task identities %v", ids)
	}
	si, err := js.StreamInfo("INPUT")
	if err != nil || si.State.Msgs != 2 {
		t.Fatalf("input stream %+v: %v", si, err)
	}
}

// Answers with the same run and tool IDs must remain distinct across routes.
// Each route includes both the configured base and the worker token.
func TestAnswerSharedStream(t *testing.T) {
	t.Parallel()
	nc, js := broker(t)
	if _, err := js.AddStream(&gonats.StreamConfig{Name: "INPUT", Subjects: []string{"answers.*", "other.*"}}); err != nil {
		t.Fatal(err)
	}
	ctx := deadline(t)
	ids := make(map[string]bool)
	for _, route := range []struct{ base, worker string }{{"answers", "one"}, {"answers", "two"}, {"other", "one"}} {
		cfg := config()
		cfg.AnswerSubject = route.base
		c, err := New(nc, cfg)
		if err != nil {
			t.Fatal(err)
		}
		waiting := Outcome{Version: 1, TaskID: "same", RunID: "run", WorkerID: route.worker, AnswerSubject: route.base + "." + route.worker, State: runtime.RunWaiting, Suspend: &runtime.SuspendRequest{ToolCallID: "call"}}
		first, err := c.Answer(ctx, waiting, []runtime.InputResponse{{Text: "yes"}})
		if err != nil || first.Duplicate || first.Stream != "INPUT" {
			t.Fatalf("%s first %+v: %v", waiting.AnswerSubject, first, err)
		}
		retry, err := c.Answer(ctx, waiting, []runtime.InputResponse{{Text: "changed"}})
		if err != nil || !retry.Duplicate || retry.Sequence != first.Sequence {
			t.Fatalf("%s retry %+v: %v", waiting.AnswerSubject, retry, err)
		}
		msg, err := js.GetMsg("INPUT", first.Sequence)
		if err != nil {
			t.Fatal(err)
		}
		var stored Answer
		if err := json.Unmarshal(msg.Data, &stored); err != nil {
			t.Fatal(err)
		}
		id := msg.Header.Get(gonats.MsgIdHdr)
		if id == "" || ids[id] || stored.MessageID != id || msg.Subject != waiting.AnswerSubject || stored.WorkerID != route.worker || stored.RunID != waiting.RunID || stored.ToolCallID != waiting.Suspend.ToolCallID || len(stored.Responses) != 1 || stored.Responses[0].Text != "yes" {
			t.Fatalf("stored %s %+v, identity %q", msg.Subject, stored, id)
		}
		ids[id] = true
	}
	si, err := js.StreamInfo("INPUT")
	if err != nil || si.State.Msgs != 3 {
		t.Fatalf("input stream %+v: %v", si, err)
	}
}
