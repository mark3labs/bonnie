package nats

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats-server/v2/server"
	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

var _ channel.Lifecycle = (*Channel)(nil)

func testServer(t *testing.T) *gonats.Conn {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server did not start")
	}
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	nc, err := gonats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func testRunner(model *fakemodel.Model) *runtime.Runner {
	return runtime.NewRunner(runtime.NewMemoryJournal(), runtime.KitAgent(model.Option(), func(o *kit.Options) {
		o.SkipConfig = true
		o.NoContextFiles = true
		o.NoSkills = true
		o.NoExtensions = true
		o.NoAgents = true
		o.DisableCoreTools = true
		o.Quiet = true
	}, kit.WithTools(runtime.AskTool())))
}

func send(t *testing.T, nc *gonats.Conn, subject string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Publish(subject, data); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, sub *gonats.Subscription) Result {
	t.Helper()
	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var result Result
	if err := json.Unmarshal(msg.Data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// A task duplicate must not answer a question. Only a matching explicit
// answer resumes it. The wire preserves the suspension and run ID.
func TestTaskSuspendResume(t *testing.T) {
	t.Parallel()
	nc := testServer(t)
	model := fakemodel.New(fakemodel.Call("ask_human", `{"question":"Which region?"}`), fakemodel.Say("done"), fakemodel.Say("other"))
	c, err := New(testRunner(model), Config{Conn: nc, Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	send(t, nc, "tasks", Task{TaskID: "one", Text: "deploy", Context: []string{"region context"}})
	waiting := receive(t, sub)
	if waiting.Error != "" || waiting.State != runtime.RunWaiting || waiting.Suspend == nil || waiting.Suspend.ToolCallID == "" {
		t.Fatalf("waiting: %+v", waiting)
	}
	send(t, nc, "tasks", Task{TaskID: "one", Text: "this is NOT an answer"})
	if got := receive(t, sub); got.Error != "duplicate task" {
		t.Fatalf("duplicate: %+v", got)
	}
	send(t, nc, "answers", Answer{TaskID: "one", ToolCallID: "old", Responses: []runtime.InputResponse{{Text: "wrong"}}})
	if got := receive(t, sub); got.Error != "stale answer" {
		t.Fatalf("stale: %+v", got)
	}
	send(t, nc, "answers", Answer{TaskID: "one", ToolCallID: waiting.Suspend.ToolCallID, Responses: []runtime.InputResponse{{Text: "eu-west-1"}}})
	completed := receive(t, sub)
	if completed.Error != "" || completed.RunID != waiting.RunID || completed.State != runtime.RunCompleted || completed.Response != "done" {
		t.Fatalf("completed: %+v", completed)
	}
	send(t, nc, "answers", Answer{TaskID: "one", ToolCallID: waiting.Suspend.ToolCallID, Responses: []runtime.InputResponse{{Text: "late"}}})
	if got := receive(t, sub); got.Error != "stale answer" {
		t.Fatalf("late: %+v", got)
	}
	send(t, nc, "tasks", Task{TaskID: "two", Text: "other"})
	if got := receive(t, sub); got.RunID == waiting.RunID || got.Response != "other" {
		t.Fatalf("independent: %+v", got)
	}
}

// Invalid input gets a visible error, never a run or a caller-selected reply.
func TestMalformedMessages(t *testing.T) {
	t.Parallel()
	nc := testServer(t)
	c, err := New(testRunner(fakemodel.New()), Config{Conn: nc, Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	for _, data := range []string{"{", "null", `{"task_id":"x","text":""}`, `{"task_id":"x","text":"work","result_subject":"other"}`, `{"task_id":"x","text":"work"} {}`, strings.Repeat("x", maxMessageBytes+1)} {
		// Oversized messages are checked directly: the server's default payload
		// limit can reject them before the adapter sees them.
		if len(data) > maxMessageBytes {
			if err := decode([]byte(data), new(Task)); err == nil {
				t.Fatal("accepted oversized input")
			}
			continue
		}
		if err := nc.PublishRequest("tasks", "other", []byte(data)); err != nil {
			t.Fatal(err)
		}
		got := receive(t, sub)
		if got.Error == "" || got.RunID != "" {
			t.Fatalf("malformed result: %+v", got)
		}
	}
	send(t, nc, "answers", Answer{TaskID: "unknown", ToolCallID: "call", Responses: []runtime.InputResponse{{Text: "yes"}}})
	if got := receive(t, sub); got.Error != "unknown task" {
		t.Fatalf("unknown: %+v", got)
	}
}

// Lifecycle calls are safe when concurrent. The caller's connection survives
// shutdown. Duplicate deliveries never cause a second model request.
func TestLifecycleAndDuplicates(t *testing.T) {
	t.Parallel()
	nc := testServer(t)
	c, err := New(testRunner(fakemodel.New(fakemodel.Say("done"))), Config{Conn: nc, Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results", Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("second Start succeeded")
	}
	for range 12 {
		send(t, nc, "tasks", Task{TaskID: "same", Text: "work"})
	}
	completed := 0
	for range 12 {
		got := receive(t, sub)
		if got.Error == "" {
			completed++
		} else if got.Error != "duplicate task" {
			t.Fatalf("result: %+v", got)
		}
	}
	if completed != 1 {
		t.Fatalf("completed = %d", completed)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := c.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if nc.IsClosed() {
		t.Fatal("closed caller connection")
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("restarted stopped channel")
	}
}

func TestConfigAndDedupe(t *testing.T) {
	t.Parallel()
	r := testRunner(fakemodel.New())
	valid := Config{URL: "nats://localhost:4222", Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results"}
	cases := []Config{valid, valid, valid, valid, valid, valid}
	cases[0].Subject = "tasks.*"
	cases[1].ResultSubject = "results..out"
	cases[2].AnswerSubject = "tasks"
	cases[3].Concurrency = -1
	cases[4].Buffer = -1
	cases[5].URL = "nats://secret@%invalid"
	for _, cfg := range cases {
		if _, err := New(r, cfg); err == nil {
			t.Fatalf("accepted invalid config: %+v", cfg)
		}
	}
	if _, err := New(nil, valid); err == nil {
		t.Fatal("accepted nil runner")
	}
	c, err := New(r, valid)
	if err != nil {
		t.Fatal(err)
	}
	for i := range dedupeLimit + 1 {
		if !c.claim(string(rune(i + 1))) {
			t.Fatal("unexpected duplicate")
		}
	}
	if len(c.seen) != dedupeLimit {
		t.Fatal("unbounded dedupe")
	}
	if !c.claim(string(rune(1))) {
		t.Fatal("old entry not evicted")
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("started after shutdown")
	}
}
