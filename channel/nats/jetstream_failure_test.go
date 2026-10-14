package nats

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

// Fail one transport journal operation, not the Runner's own journal work.
// Tests call the handler directly so there is no concurrent redelivery.
type jsFailureJournal struct {
	runtime.Journal
	key, ext string
	readAt   int
	reads    int
	hit      bool
}

func (j *jsFailureJournal) Replay(ctx context.Context, key string) ([]runtime.Record, error) {
	if key == j.key && j.readAt > 0 {
		j.reads++
		if j.reads == j.readAt {
			j.hit = true
			return nil, errors.New("injected read failure")
		}
	}
	return j.Journal.Replay(ctx, key)
}

func (j *jsFailureJournal) Append(ctx context.Context, rec runtime.Record) (int, error) {
	if rec.RunID == j.key && rec.ExtType == j.ext && j.ext != "" {
		j.hit = true
		return 0, errors.New("injected write failure")
	}
	return j.Journal.Append(ctx, rec)
}

// Each reported journal failure leaves the delivery pending. Protocol errors
// from the run lookup and Snapshot are intentionally not infrastructure exits.
func TestJetStreamJournalFailuresLeaveUnacked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind, ext, report string
		readAt                  int
		answer                  bool
	}{
		{name: "input read", kind: "input", readAt: 1, report: "result cache read failed"},
		{name: "task admission read", kind: "input", readAt: 2, report: "task admission read failed"},
		{name: "task admission save", kind: "input", ext: "nats_task_attempt_admitted", report: "task admission save failed"},
		{name: "task index save", kind: "status", ext: "nats_task_admitted", report: "task admission index save failed"},
		{name: "run route save", kind: "run", ext: "nats_result", report: "run route save failed"},
		{name: "input save", kind: "input", ext: "nats_result", report: "result cache save failed"},
		{name: "answer cache read", kind: "answer", readAt: 1, answer: true, report: "answer cache read failed"},
		{name: "answer admission read", kind: "answer", readAt: 2, answer: true, report: "answer admission read failed"},
		{name: "answer admission save", kind: "answer", ext: "nats_answer_admitted", answer: true, report: "answer admission save failed"},
		{name: "answer run route save", kind: "run", ext: "nats_result", answer: true, report: "run route save failed"},
		{name: "answer result save", kind: "answer", ext: "nats_result", answer: true, report: "answer result save failed"},
		{name: "answer input save", kind: "input", ext: "nats_result", answer: true, report: "result cache save failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nc, js := jsServer(t)
			resultStream(t, js)
			inputStream(t, js, 10*time.Second)
			if _, err := js.AddConsumer("INPUT", &gonats.ConsumerConfig{Durable: "agents_one", FilterSubject: "answers.one", AckPolicy: gonats.AckExplicitPolicy, AckWait: 10 * time.Second}); err != nil {
				t.Fatal(err)
			}
			j := &jsFailureJournal{Journal: runtime.NewMemoryJournal()}
			cfg := jsConfig(nc, "one")
			cfg.QuerySubject = "queries"
			c, err := New(reviewRunner(j, fakemodel.New(fakemodel.Call("ask_human", `{"question":"Where?"}`), fakemodel.Say("done"))), cfg)
			if err != nil {
				t.Fatal(err)
			}
			c.js, c.conn, c.ackWait = js, nc, 10*time.Second
			fetch := func(subject, consumer string) *gonats.Msg {
				t.Helper()
				sub, err := js.PullSubscribe(subject, consumer, gonats.Bind("INPUT", consumer))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = sub.Unsubscribe() })
				msgs, err := sub.Fetch(1, gonats.MaxWait(2*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				return msgs[0]
			}
			jsSend(t, js, "tasks", Task{Version: 1, TaskID: "a", Text: "work"})
			msg := fetch("tasks", "agents")
			consumer, seq := "agents", "1"
			var first *Result
			if tc.answer {
				c.handleJetStream(t.Context(), msg)
				first, err = c.loadResult(t.Context(), c.cacheKey("input", "1"))
				if err != nil || first == nil || first.Suspend == nil {
					t.Fatalf("waiting result: %+v, %v", first, err)
				}
				jsSend(t, js, "answers.one", Answer{Version: 1, MessageID: "answer", TaskID: "a", AgentID: "one", RunID: first.RunID, ToolCallID: first.Suspend.ToolCallID, Responses: []runtime.InputResponse{{Text: "here"}}})
				msg = fetch("answers.one", "agents_one")
				consumer, seq = "agents_one", "2"
			}
			switch tc.kind {
			case "input":
				j.key = c.cacheKey("input", seq)
			case "answer":
				j.key = c.cacheKey("answer", first.RunID, "answer")
			case "status":
				j.key = c.statusKey()
			case "run":
				if first != nil {
					j.key = c.cacheKey("run", first.RunID)
				} else {
					// Seed the admission so the run-route key is known in advance.
					result := Result{Version: 1, TaskID: "a", AgentID: "one", AttemptID: "fixed", AnswerSubject: c.answerRoute()}
					ref := c.core.From("js/fixed")
					runID, err := ref.RunID(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					data, err := json.Marshal(admittedTask{Result: result, RunID: runID})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := j.Append(t.Context(), runtime.Record{RunID: c.cacheKey("input", seq), Kind: runtime.RecordExtensionData, ExtType: "nats_task_attempt_admitted", Payload: data}); err != nil {
						t.Fatal(err)
					}
					j.key = c.cacheKey("run", runID)
				}
			}
			j.ext, j.readAt = tc.ext, tc.readAt
			c.handleJetStream(t.Context(), msg)
			if !j.hit {
				t.Fatal("failure was not injected")
			}
			c.mu.Lock()
			failure := c.failure
			c.mu.Unlock()
			if failure == nil || !strings.HasSuffix(failure.Error(), ": "+tc.report) {
				t.Fatalf("failure = %v, want %q", failure, tc.report)
			}
			info, err := js.ConsumerInfo("INPUT", consumer)
			if err != nil {
				t.Fatal(err)
			}
			if info.NumAckPending != 1 || info.AckFloor.Consumer != 0 {
				t.Fatalf("delivery was acknowledged: %+v", info)
			}
		})
	}
}
