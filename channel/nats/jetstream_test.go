package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats-server/v2/server"
	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

func jsServer(t *testing.T) (*gonats.Conn, gonats.JetStreamContext) {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, JetStream: true, StoreDir: t.TempDir()})
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
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	return nc, js
}

func jsConfig(nc *gonats.Conn, worker string) Config {
	return Config{Conn: nc, Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results", Stream: "INPUT", Consumer: "workers", WorkerID: worker, CreateStream: true, Concurrency: 1}
}

func resultStream(t *testing.T, js gonats.JetStreamContext) {
	t.Helper()
	if _, err := js.AddStream(&gonats.StreamConfig{Name: "OUTPUT", Subjects: []string{"results"}}); err != nil {
		t.Fatal(err)
	}
}

func inputStream(t *testing.T, js gonats.JetStreamContext, wait time.Duration) {
	t.Helper()
	if _, err := js.AddStream(&gonats.StreamConfig{Name: "INPUT", Subjects: []string{"tasks", "answers.*"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddConsumer("INPUT", &gonats.ConsumerConfig{Durable: "workers", FilterSubject: "tasks", AckPolicy: gonats.AckExplicitPolicy, AckWait: wait, MaxAckPending: 2}); err != nil {
		t.Fatal(err)
	}
}

func jsSend(t *testing.T, js gonats.JetStreamContext, subject string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(subject, data); err != nil {
		t.Fatal(err)
	}
}

func jsChannel(t *testing.T, r *runtime.Runner, cfg Config) *Channel {
	t.Helper()
	c, err := New(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// Separate journals can share one task consumer. Each result identifies its
// worker, run, and independent attempt; no task is broadcast to both workers.
func TestJetStreamDistribution(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	resultStream(t, js)
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	replies := make([]fakemodel.Reply, 12)
	for i := range replies {
		replies[i] = fakemodel.Say("done")
	}
	for _, worker := range []string{"one", "two"} {
		cfg := jsConfig(nc, worker)
		cfg.Consumer = "" // Both workers must resolve the same durable default.
		ch := jsChannel(t, testRunner(fakemodel.New(replies...)), cfg)
		if ch.cfg.Consumer != DefaultConsumerName(cfg.Subject) {
			t.Fatalf("consumer = %q", ch.cfg.Consumer)
		}
	}
	for i := range 12 {
		jsSend(t, js, "tasks", Task{Version: 1, TaskID: fmt.Sprint(i), Text: "work"})
	}
	seen := map[string]bool{}
	workers := map[string]bool{}
	for range 12 {
		result := receive(t, sub)
		if result.Error != "" || result.Version != 1 || result.AttemptID == "" || result.AnswerSubject != "answers."+result.WorkerID || seen[result.TaskID] {
			t.Fatalf("result: %+v", result)
		}
		seen[result.TaskID] = true
		workers[result.WorkerID] = true
	}
	if len(workers) != 2 {
		t.Fatalf("workers: %v", workers)
	}
}

// A waiting input is acknowledged. Answers use its worker route and exact
// run/tool identity. Repeated answer identities do not resume a second time.
func TestJetStreamAnswerRouting(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	resultStream(t, js)
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	model := fakemodel.New(fakemodel.Call("ask_human", `{"question":"Where?"}`), fakemodel.Say("done"))
	jsChannel(t, testRunner(model), jsConfig(nc, "one"))
	jsSend(t, js, "tasks", Task{Version: 1, TaskID: "a", Text: "work"})
	waiting := receive(t, sub)
	if waiting.Suspend == nil || waiting.State != runtime.RunWaiting {
		t.Fatalf("waiting: %+v", waiting)
	}
	answer := Answer{Version: 1, MessageID: "first", TaskID: "a", RunID: waiting.RunID, WorkerID: "one", ToolCallID: waiting.Suspend.ToolCallID, Responses: []runtime.InputResponse{{Text: "here"}}}
	wrong := answer
	wrong.MessageID = "wrong"
	wrong.ToolCallID = "old"
	jsSend(t, js, waiting.AnswerSubject, wrong)
	if got := receive(t, sub); got.Error != "stale answer" {
		t.Fatalf("stale: %+v", got)
	}
	jsSend(t, js, waiting.AnswerSubject, answer)
	completed := receive(t, sub)
	if completed.State != runtime.RunCompleted || completed.Error != "" {
		t.Fatalf("completed: %+v", completed)
	}
	jsSend(t, js, waiting.AnswerSubject, answer)
	if got := receive(t, sub); got.AttemptID != completed.AttemptID || got.Error != "" {
		t.Fatalf("duplicate: %+v", got)
	}
}

type slowAgent struct {
	calls *atomic.Int32
	delay time.Duration
}

func (a *slowAgent) PromptResult(ctx context.Context, _ string) (*kit.TurnResult, error) {
	a.calls.Add(1)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(a.delay):
		return &kit.TurnResult{}, nil
	}
}
func (*slowAgent) InjectSteer(string) {}
func (*slowAgent) Close() error       { return nil }
func slowRunner(j runtime.Journal, calls *atomic.Int32, delay time.Duration) *runtime.Runner {
	return runtime.NewRunner(j, func(context.Context, *runtime.Session) (runtime.Agent, error) {
		return &slowAgent{calls: calls, delay: delay}, nil
	})
}

// Processing longer than AckWait does not allow another worker to execute
// the same input while the first worker sends progress acknowledgements.
func TestJetStreamProgress(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	resultStream(t, js)
	inputStream(t, js, 90*time.Millisecond)
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	jsChannel(t, slowRunner(runtime.NewMemoryJournal(), &calls, 700*time.Millisecond), jsConfig(nc, "one"))
	jsChannel(t, slowRunner(runtime.NewMemoryJournal(), &calls, 700*time.Millisecond), jsConfig(nc, "two"))
	jsSend(t, js, "tasks", Task{Version: 1, TaskID: "a", Text: "work"})
	if got := receive(t, sub); got.Error != "" {
		t.Fatalf("result: %+v", got)
	}
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("executions: %d", calls.Load())
	}
}

// A second Runner shares only the durable journal. Failed result publication
// leaves input unacknowledged; replay publishes the saved result, not a new run.
func TestJetStreamSavedResultRecovery(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	inputStream(t, js, 100*time.Millisecond)
	journal, err := runtime.OpenSQLiteJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	var calls atomic.Int32
	c := jsChannel(t, slowRunner(journal, &calls, 0), jsConfig(nc, "one"))
	jsSend(t, js, "tasks", Task{Version: 1, TaskID: "a", Text: "work"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		failure := c.failure
		c.mu.Unlock()
		if failure != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("publication did not fail")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Shutdown(context.Background()); err == nil {
		t.Fatal("missing publication error")
	}
	resultStream(t, js)
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	jsChannel(t, slowRunner(journal, &calls, 0), jsConfig(nc, "one"))
	if got := receive(t, sub); got.Error != "" || got.RunID == "" {
		t.Fatalf("replayed result: %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("reran saved input: %d", calls.Load())
	}
}

func TestJetStreamInvalidConfig(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	for _, change := range []func(*Config){func(c *Config) { c.WorkerID = "a.b" }, func(c *Config) { c.Consumer = "a_b" }, func(c *Config) { c.Stream = "" }, func(c *Config) { c.ResultSubject = "answers.one" }} {
		cfg := jsConfig(nc, "one")
		change(&cfg)
		if _, err := New(testRunner(fakemodel.New()), cfg); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	inputStream(t, js, time.Second)
	if err := js.DeleteConsumer("INPUT", "workers"); err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddConsumer("INPUT", &gonats.ConsumerConfig{Durable: "workers", FilterSubject: "tasks", AckPolicy: gonats.AckNonePolicy}); err != nil {
		t.Fatal(err)
	}
	c, err := New(testRunner(fakemodel.New()), jsConfig(nc, "one"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err == nil {
		t.Fatal("accepted incompatible consumer")
	}
}
