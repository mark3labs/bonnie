package bonnie

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats-server/v2/server"
	gonats "github.com/nats-io/nats.go"

	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
)

// Issue 18: the managed completion wrapper must preserve saved-conversation
// continuation. A new host shares only the SQLite journal and broker with the
// stopped host. Redelivery must keep the admission identity and must not add
// an empty or repeated user message to the journal or the model request.
func TestCompletionHostTargetedRedelivery(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Shutdown(); srv.WaitForShutdown() })
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("broker did not start")
	}
	nc, err := gonats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddStream(&gonats.StreamConfig{Name: "OUTPUT", Subjects: []string{"results"}}); err != nil {
		t.Fatal(err)
	}
	sub, err := nc.SubscribeSync("results")
	if err != nil {
		t.Fatal(err)
	}
	cfg := natschannel.Config{
		Conn: nc, Subject: "tasks", AnswerSubject: "answers", ResultSubject: "results",
		Stream: "INPUT", Consumer: "agents", AgentID: "one", CreateStream: true,
		TargetedTasks: true, Concurrency: 1,
	}
	const prompt = "recover this exact task prompt"
	journalDir, sandboxDir := t.TempDir(), t.TempDir()
	entered := make(chan string, 1)
	var completed atomic.Int32

	startHost := func(model *fakemodel.Model, block bool) func() {
		t.Helper()
		hostCtx, stop := context.WithCancel(ctx)
		a := New(
			WithAddr("127.0.0.1:0"), WithJournal(journalDir), Quiet(),
			WithInstructions(""), WithContextFiles(""), WithSkills(""), WithoutHumanInput(),
			WithShutdownTimeout(3*time.Second), WithNATS(cfg),
			WithSandbox(sandbox.Local(sandbox.WithLocalRoot(sandboxDir))),
			WithKit(completionKitOptions(model)...),
			WithKitSetup(func(turnCtx context.Context, k *kit.Kit, scope RunScope) error {
				if block {
					// Kit saves the user message before it prepares the first step.
					k.OnPrepareStep(kit.HookPriorityNormal, func(kit.PrepareStepHook) *kit.PrepareStepResult {
						entered <- scope.RunID
						<-turnCtx.Done()
						return nil
					})
				}
				return nil
			}),
			WithCompletionHook(CompletionPolicy{NewHook: func(context.Context, RunScope) (CompletionHook, error) {
				return func(context.Context, CompletionCandidate) (CompletionFeedback, error) {
					completed.Add(1)
					return CompletionFeedback{}, nil
				}, nil
			}}),
		)
		done := make(chan error, 1)
		go func() { done <- a.Run(hostCtx) }()
		var stopped atomic.Bool
		shutdown := func() {
			t.Helper()
			if stopped.Swap(true) {
				return
			}
			stop()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("host shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("host did not stop")
			}
		}
		t.Cleanup(shutdown)
		return shutdown
	}

	firstModel := fakemodel.New()
	stopFirst := startHost(firstModel, true)
	// Wait for stream creation, then publish exactly one targeted message.
	for {
		if _, err := js.StreamInfo("INPUT"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("input stream did not become ready")
		case <-time.After(5 * time.Millisecond):
		}
	}
	task := natschannel.Task{Version: 1, TaskID: "issue-18", Text: prompt}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish("tasks.agent.one", data); err != nil {
		t.Fatal(err)
	}
	var runID string
	select {
	case runID = <-entered:
	case <-ctx.Done():
		t.Fatal("targeted task did not enter the active turn")
	}
	stopFirst()

	// Open a separate journal handle after Agent.Run has closed the first one.
	journal, err := runtime.OpenSQLiteJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Errorf("close inspection journal: %v", err)
		}
	})
	if state, err := journal.State(ctx, runID); err != nil || state != runtime.RunInterrupted {
		t.Fatalf("stopped run state = %s, error = %v", state, err)
	}
	ids, err := journal.Runs(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var admitted struct {
		RunID  string             `json:"run_id"`
		Result natschannel.Result `json:"result"`
	}
	admissions := 0
	for _, id := range ids {
		records, err := journal.Replay(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range records {
			if rec.ExtType == "nats_task_attempt_admitted" {
				admissions++
				if err := json.Unmarshal(rec.Payload, &admitted); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if admissions != 1 || admitted.RunID != runID || admitted.Result.AttemptID == "" {
		t.Fatalf("admissions = %d, saved admission = %+v, run = %s", admissions, admitted, runID)
	}
	if completed.Load() != 0 || len(firstModel.Requests()) != 0 {
		t.Fatal("interrupted turn reached the model or completion hook")
	}

	// Keep the same durable consumer. Shorten only its redelivery delay.
	for info := range js.ConsumersInfo("INPUT", gonats.Context(ctx)) {
		if info.Config.FilterSubject == "tasks.agent.one" {
			consumer := info.Config
			consumer.AckWait = 100 * time.Millisecond
			if _, err := js.UpdateConsumer("INPUT", &consumer); err != nil {
				t.Fatal(err)
			}
		}
	}
	secondModel := fakemodel.New(fakemodel.Say("recovered response"))
	stopSecond := startHost(secondModel, false)
	msg, err := sub.NextMsgWithContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result natschannel.Result
	if err := json.Unmarshal(msg.Data, &result); err != nil {
		t.Fatal(err)
	}
	stopSecond()
	if result.Error != "" || result.State != runtime.RunCompleted || result.Response != "recovered response" ||
		result.RunID != runID || result.AttemptID != admitted.Result.AttemptID ||
		result.TaskID != task.TaskID || result.AgentID != cfg.AgentID || result.AnswerSubject != "answers.one" {
		t.Fatalf("redelivered outcome = %+v, saved admission = %+v", result, admitted)
	}
	if completed.Load() != 1 {
		t.Errorf("completion hook calls = %d, want 1", completed.Load())
	}
	requests := secondModel.Requests()
	if len(requests) != 1 {
		t.Fatalf("recovered model requests = %d, want 1", len(requests))
	}
	var users []string
	for _, message := range requests[0].Messages {
		if message.Role == "user" {
			var text strings.Builder
			for _, part := range message.Content {
				if part, ok := part.(kit.LLMTextPart); ok {
					text.WriteString(part.Text)
				}
			}
			users = append(users, text.String())
		}
	}
	// Channel context is injected for the model, but is not saved as user input.
	wantModelUsers := []string{"[context] This conversation is on channel nats (task).", prompt}
	if !reflect.DeepEqual(users, wantModelUsers) {
		t.Errorf("model user prompts = %q, want %q", users, wantModelUsers)
	}
	records, err := journal.Replay(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	users = nil
	for _, rec := range records {
		if rec.Kind == runtime.RecordMessage && rec.Role == "user" {
			users = append(users, rec.Text)
		}
	}
	if !reflect.DeepEqual(users, []string{prompt}) {
		t.Errorf("durable user prompts = %q, want one original prompt", users)
	}
	if state, err := journal.State(ctx, runID); err != nil || state != runtime.RunCompleted {
		t.Errorf("recovered state = %s, error = %v", state, err)
	}
}
