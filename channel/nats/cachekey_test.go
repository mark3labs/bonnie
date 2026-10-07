package nats

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

// Every transport key must fit SQLite, even with maximum-length names and
// answer identities. Short keys retain their old spelling for stored results.
func TestCacheKeysFitSQLite(t *testing.T) {
	t.Parallel()
	j, err := runtime.OpenSQLiteJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	seen := map[string]bool{}
	for _, size := range []int{1, 128} {
		c := &Channel{cfg: Config{Stream: strings.Repeat("s", size), Consumer: strings.Repeat("c", size), WorkerID: strings.Repeat("w", size)}}
		for _, kind := range []string{"input", "run", "status", "answer"} {
			ids := []string{strings.Repeat("r", 256)}
			if kind == "answer" {
				ids = append(ids, strings.Repeat("m", 256))
			}
			key := c.cacheKey(kind, ids...)
			if len(key) > 200 || !strings.HasPrefix(key, runtime.ReservedRunPrefix) || key != c.cacheKey(kind, ids...) || seen[key] {
				t.Fatalf("invalid or duplicate key: %q", key)
			}
			seen[key] = true
			if _, err := j.Append(t.Context(), runtime.Record{RunID: key, Kind: runtime.RecordExtensionData}); err != nil {
				t.Fatalf("append %s: %v", kind, err)
			}
			if _, err := j.Replay(t.Context(), key); err != nil {
				t.Fatalf("replay %s: %v", kind, err)
			}
		}
	}
	c := &Channel{cfg: Config{Stream: "INPUT", Consumer: "workers", WorkerID: "one"}}
	want := runtime.ReservedRunPrefix + "nats.INPUT.workers.one.answer." + hex.EncodeToString([]byte("run")) + "." + hex.EncodeToString([]byte("message"))
	if got := c.cacheKey("answer", "run", "message"); got != want {
		t.Fatalf("legacy key = %q, want %q", got, want)
	}
	c.cfg.Stream = strings.Repeat("s", 128)
	if c.cacheKey("answer", "ab", "c") == c.cacheKey("answer", "a", "bc") {
		t.Fatal("answer identity boundaries were lost")
	}
}

// Long names previously let a run finish but blocked its result route write.
// SQLite must accept input, run, status, and answer records. A second Runner
// with only the shared journal must return a duplicate answer without a model.
func TestJetStreamLongNamesSQLite(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	resultStream(t, js)
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	j, err := runtime.OpenSQLiteJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	cfg := jsConfig(nc, strings.Repeat("w", 32))
	cfg.Stream, cfg.Consumer = strings.Repeat("s", 128), strings.Repeat("c", 64)
	if _, err := js.AddStream(&gonats.StreamConfig{Name: cfg.Stream, Subjects: []string{"tasks", "answers.*"}}); err != nil {
		t.Fatal(err)
	}
	for name, subject := range map[string]string{cfg.Consumer: "tasks", cfg.Consumer + "_" + cfg.WorkerID: "answers." + cfg.WorkerID} {
		if _, err := js.AddConsumer(cfg.Stream, &gonats.ConsumerConfig{Durable: name, FilterSubject: subject, AckPolicy: gonats.AckExplicitPolicy, AckWait: 100 * time.Millisecond, MaxAckPending: 2}); err != nil {
			t.Fatal(err)
		}
	}
	model := fakemodel.New(fakemodel.Call("ask_human", `{"question":"Where?"}`), fakemodel.Say("done"))
	c := jsChannel(t, reviewRunner(j, model), cfg)
	jsSend(t, js, "tasks", Task{Version: 1, TaskID: "task", Text: "work"})
	waiting := receive(t, sub)
	if waiting.State != runtime.RunWaiting || waiting.Suspend == nil || waiting.Error != "" {
		t.Fatalf("waiting: %+v", waiting)
	}
	answer := Answer{Version: 1, MessageID: strings.Repeat("m", 256), TaskID: "task", RunID: waiting.RunID, WorkerID: cfg.WorkerID, ToolCallID: waiting.Suspend.ToolCallID, Responses: []runtime.InputResponse{{Text: "here"}}}
	jsSend(t, js, waiting.AnswerSubject, answer)
	completed := receive(t, sub)
	if completed.State != runtime.RunCompleted || completed.Error != "" || completed.Response != "done" {
		t.Fatalf("completed: %+v", completed)
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	jsChannel(t, reviewRunner(j, fakemodel.New()), cfg)
	jsSend(t, js, waiting.AnswerSubject, answer)
	if got := receive(t, sub); got.AttemptID != completed.AttemptID || got.Error != "" || got.Response != "done" {
		t.Fatalf("duplicate: %+v", got)
	}
}
