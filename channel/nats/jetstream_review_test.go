package nats

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
	gonats "github.com/nats-io/nats.go"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
)

type outcomeFailureJournal struct {
	runtime.Journal
	fail atomic.Bool
	hit  chan struct{}
}

func (j *outcomeFailureJournal) Append(ctx context.Context, record runtime.Record) (int, error) {
	if record.ExtType == "nats_result" && j.fail.Load() {
		select {
		case j.hit <- struct{}{}:
		default:
		}
		return 0, errors.New("outcome save failed")
	}
	return j.Journal.Append(ctx, record)
}

func reviewRunner(j runtime.Journal, model *fakemodel.Model) *runtime.Runner {
	return runtime.NewRunner(j, runtime.KitAgent(model.Option(), func(o *kit.Options) {
		o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents = true, true, true, true, true
		o.DisableCoreTools, o.Quiet = true, true
	}, kit.WithTools(runtime.AskTool())))
}

// Resume changes the durable run, then saving the transport outcome fails.
// A second Runner shares only the journal and must recover without a model call.
func TestJetStreamAdmittedAnswerRecovery(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"completed", "waiting", "failed"} {
		waitingAgain := outcome == "waiting"
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			nc, js := jsServer(t)
			resultStream(t, js)
			inputStream(t, js, 100*time.Millisecond)
			if _, err := js.AddConsumer("INPUT", &gonats.ConsumerConfig{Durable: "agents_one", FilterSubject: "answers.one", AckPolicy: gonats.AckExplicitPolicy, AckWait: 100 * time.Millisecond, MaxAckPending: 2}); err != nil {
				t.Fatal(err)
			}
			sub, err := nc.SubscribeSync("results")
			if err != nil {
				t.Fatal(err)
			}
			next := fakemodel.Say("done")
			if waitingAgain {
				next = fakemodel.Call("ask_human", `{"question":"Next?"}`)
			}
			j := &outcomeFailureJournal{Journal: runtime.NewMemoryJournal(), hit: make(chan struct{}, 1)}
			replies := []fakemodel.Reply{fakemodel.Call("ask_human", `{"question":"Where?"}`)}
			if outcome != "failed" {
				replies = append(replies, next)
			}
			c := jsChannel(t, reviewRunner(j, fakemodel.New(replies...)), jsConfig(nc, "one"))
			jsSend(t, js, "tasks", Task{Version: 1, TaskID: "a", Text: "work"})
			first := receive(t, sub)
			if first.Suspend == nil {
				t.Fatalf("first: %+v", first)
			}
			answer := Answer{Version: 1, MessageID: "answer", TaskID: "a", RunID: first.RunID, AgentID: "one", ToolCallID: first.Suspend.ToolCallID, Responses: []runtime.InputResponse{{Text: "here"}}}
			j.fail.Store(true)
			jsSend(t, js, first.AnswerSubject, answer)
			select {
			case <-j.hit:
			case <-time.After(5 * time.Second):
				t.Fatal("save did not fail")
			}
			_ = c.Shutdown(context.Background())
			admission, err := c.loadAdmission(t.Context(), c.cacheKey("answer", answer.RunID, answer.MessageID))
			if err != nil || admission == nil || admission.Answer.Responses[0].Text != "here" || admission.Suspend.ToolCallID != first.Suspend.ToolCallID {
				t.Fatalf("admission: %+v %v", admission, err)
			}
			j.fail.Store(false)
			jsChannel(t, reviewRunner(j.Journal, fakemodel.New()), jsConfig(nc, "one"))
			got := receive(t, sub)
			if outcome == "failed" {
				if got.State != runtime.RunFailed || got.Error != "admitted answer resume interrupted; cannot auto continue safely" {
					t.Fatalf("failure: %+v", got)
				}
				return
			}
			if got.Error != "" || got.RunID != first.RunID || got.AttemptID != first.AttemptID {
				t.Fatalf("recovered: %+v", got)
			}
			if waitingAgain && (got.State != runtime.RunWaiting || got.Suspend == nil || got.Suspend.ToolCallID == first.Suspend.ToolCallID) {
				t.Fatalf("new suspension: %+v", got)
			}
			if !waitingAgain && (got.State != runtime.RunCompleted || got.Response != "done") {
				t.Fatalf("completed: %+v", got)
			}
		})
	}
}

// Encoded output, including escaping and suspension data, must fit before
// any outcome cache write. An oversized outcome is an explicit small failure.
func TestJetStreamOversizedOutput(t *testing.T) {
	t.Parallel()
	for _, question := range []bool{false, true} {
		t.Run(map[bool]string{false: "response", true: "suspension"}[question], func(t *testing.T) {
			t.Parallel()
			nc, js := jsServer(t)
			if _, err := js.AddStream(&gonats.StreamConfig{Name: "OUTPUT", Subjects: []string{"results"}, MaxMsgSize: 4096}); err != nil {
				t.Fatal(err)
			}
			sub, err := nc.SubscribeSync("results")
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Repeat("\"", 3000)
			reply := fakemodel.Say(text)
			if question {
				data, _ := json.Marshal(map[string]string{"question": text})
				reply = fakemodel.Call("ask_human", string(data))
			}
			c := jsChannel(t, testRunner(fakemodel.New(reply)), jsConfig(nc, "one"))
			jsSend(t, js, "tasks", Task{Version: 1, TaskID: "a", Text: "work"})
			got := receive(t, sub)
			if got.Error != "result too large" || got.State != runtime.RunFailed || got.RunID == "" || got.AttemptID == "" || got.Suspend != nil || got.Response != "" {
				t.Fatalf("output: %+v", got)
			}
			cached, err := c.loadResult(t.Context(), c.cacheKey("run", got.RunID))
			if err != nil || cached == nil || cached.Error != got.Error {
				t.Fatalf("cache: %+v %v", cached, err)
			}
			run, err := c.core.Runner().Snapshot(t.Context(), got.RunID)
			if err != nil || run.State == runtime.RunFailed {
				t.Fatalf("transport changed run: %+v %v", run, err)
			}
		})
	}
}

func TestJetStreamRejectsSmallOutputLimit(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	if _, err := js.AddStream(&gonats.StreamConfig{Name: "OUTPUT", Subjects: []string{"results"}, MaxMsgSize: 128}); err != nil {
		t.Fatal(err)
	}
	c, err := New(testRunner(fakemodel.New()), jsConfig(nc, "one"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "too small for metadata") {
		t.Fatalf("start: %v", err)
	}
}

// A body that fits the stream limit alone must still leave room for headers.
func TestJetStreamOutputHeaderLimit(t *testing.T) {
	t.Parallel()
	nc, js := jsServer(t)
	if _, err := js.AddStream(&gonats.StreamConfig{Name: "OUTPUT", Subjects: []string{"results"}, MaxMsgSize: 8192}); err != nil {
		t.Fatal(err)
	}
	c := jsChannel(t, testRunner(fakemodel.New()), jsConfig(nc, "one"))
	result := Result{Version: 1, TaskID: "task", AgentID: "one", Response: strings.Repeat("x", 8080)}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= 8192 {
		t.Fatalf("body does not fit alone: %d", len(data))
	}
	bounded, err := c.boundResult(t.Context(), result)
	if err != nil {
		t.Fatal(err)
	}
	if bounded.Error != "result too large" {
		t.Fatalf("headers not reserved: %+v", bounded)
	}
	msg := gonats.NewMsg("results")
	msg.Data, err = json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	msg.Header.Set(gonats.MsgIdHdr, strings.Repeat("k", 450))
	if _, err := js.PublishMsg(msg); err != nil {
		t.Fatalf("bounded outcome cannot publish: %v", err)
	}
}
