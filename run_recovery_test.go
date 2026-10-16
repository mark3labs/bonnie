package bonnie

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// durableAgent saves its input before it works, as Kit does. A blocking one
// works until its turn is stopped; every one can continue a saved
// conversation.
type durableAgent struct {
	session   *runtime.Session
	block     bool
	started   chan struct{}
	once      *sync.Once
	continued *atomic.Int32
}

func (a *durableAgent) PromptResult(ctx context.Context, msg string) (*kit.TurnResult, error) {
	if _, err := a.session.AppendMessage(kit.NewLLMUserMessage(msg)); err != nil {
		return nil, err
	}
	if a.block {
		a.once.Do(func() { close(a.started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return a.answer("answered")
}

func (a *durableAgent) ContinueResult(context.Context) (*kit.TurnResult, error) {
	a.continued.Add(1)
	return a.answer("continued")
}

func (a *durableAgent) answer(text string) (*kit.TurnResult, error) {
	if _, err := a.session.AppendMessage(kit.LLMMessage{Role: kit.LLMMessageRole("assistant"), Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: text}}}); err != nil {
		return nil, err
	}
	return &kit.TurnResult{Response: text}, nil
}

func (*durableAgent) InjectSteer(string) {}
func (*durableAgent) Close() error       { return nil }

func durableFactory(block bool) (runtime.AgentFactory, chan struct{}, *atomic.Int32) {
	started, once, continued := make(chan struct{}), &sync.Once{}, &atomic.Int32{}
	return func(_ context.Context, s *runtime.Session) (runtime.Agent, error) {
		return &durableAgent{session: s, block: block, started: started, once: once, continued: continued}, nil
	}, started, continued
}

// recoveryChannel is a mounted channel with no routes. It receives the result
// of a run the host continued, or claims its runs for itself.
type recoveryChannel struct {
	name      string
	owns      bool
	delivered chan *runtime.Run
}

func (c *recoveryChannel) Name() string                   { return c.name }
func (*recoveryChannel) Routes() []channel.Route          { return nil }
func (*recoveryChannel) From(string) channel.SessionRef   { return nil }
func (*recoveryChannel) Attach(string) channel.SessionRef { return nil }
func (c *recoveryChannel) OwnsRecovery() bool             { return c.owns }
func (c *recoveryChannel) DeliverRecovered(_ context.Context, _ string, run *runtime.Run, _ error) error {
	c.delivered <- run
	return nil
}

// seedUnfinished writes what a process that died mid-turn leaves: a turn
// that is running, its saved input, and the channel it came from.
func seedUnfinished(t *testing.T, j runtime.Journal, runID, origin string) {
	t.Helper()
	ctx := context.Background()
	s := runtime.NewSession(runID, j)
	if _, err := s.AppendExtensionData(runtime.ExtOrigin, `{"channel":"`+origin+`"}`); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []runtime.Record{{RunID: runID, Kind: runtime.RecordTurn, Text: "t"}, {RunID: runID, Kind: runtime.RecordState, State: runtime.RunRunning}} {
		if _, err := j.Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AppendMessage(kit.NewLLMUserMessage("do it")); err != nil {
		t.Fatal(err)
	}
}

func durableAgentOptions(dir string, factory runtime.AgentFactory, opts ...Option) []Option {
	return append([]Option{
		WithAgentFactory(factory), WithJournal(dir), WithInstructions(""),
		WithContextFiles(""), Quiet(), WithShutdownTimeout(5 * time.Second),
	}, opts...)
}

// A served agent continues each run a crash left without a stop reason, with
// no new message, and answers on the channel the run came from. A channel
// that recovers its own runs keeps them.
func TestRunContinuesUnfinishedRunsAtStartup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedUnfinished(t, j, "chat-run", "chat")
	seedUnfinished(t, j, "broker-run", "broker")
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	chat := &recoveryChannel{name: "chat", delivered: make(chan *runtime.Run, 1)}
	broker := &recoveryChannel{name: "broker", owns: true, delivered: make(chan *runtime.Run, 1)}
	factory, _, continued := durableFactory(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- New(durableAgentOptions(dir, factory, WithAddr("127.0.0.1:0"),
			WithChannel(func(*runtime.Runner) (Channel, error) { return chat, nil }),
			WithChannel(func(*runtime.Runner) (Channel, error) { return broker, nil }),
		)...).Run(ctx)
	}()
	select {
	case run := <-chat.delivered:
		if run == nil || run.ID != "chat-run" || run.State != runtime.RunCompleted || run.Response != "continued" {
			t.Fatalf("delivered = %+v", run)
		}
	case err := <-done:
		t.Fatalf("Run returned before recovery: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the unfinished run was not continued")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := continued.Load(); n != 1 {
		t.Fatalf("continued = %d, want 1", n)
	}
	select {
	case run := <-broker.delivered:
		t.Fatalf("the host continued a run its channel owns: %+v", run)
	default:
	}

	j, err = runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	for id, want := range map[string]runtime.RunState{"chat-run": runtime.RunCompleted, "broker-run": runtime.RunRunning} {
		if state, err := j.State(context.Background(), id); err != nil || state != want {
			t.Fatalf("%s state = %s, %v; want %s", id, state, err, want)
		}
	}
}

// A graceful stop interrupts a turn in flight instead of waiting for it, and
// the next start of the agent continues it. Before, the stop either waited
// out the turn or cut it off with no state, and nothing continued it.
func TestRunShutdownInterruptsAndNextStartContinues(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	factory, started, _ := durableFactory(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- New(durableAgentOptions(dir, factory, WithListener(ln))...).Run(ctx) }()

	type reply struct {
		run bonniehttp.RunResponse
		err error
	}
	replies := make(chan reply, 1)
	go func() {
		body := bytes.NewBufferString(`{"text":"long task"}`)
		var out reply
		resp, err := http.Post("http://"+ln.Addr().String()+channel.APIPrefix+"/runs", "application/json", body)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			err = json.NewDecoder(resp.Body).Decode(&out.run)
		}
		out.err = err
		replies <- out
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn did not start")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := <-replies
	if got.err != nil || got.run.State != runtime.RunInterrupted || got.run.RunID == "" {
		t.Fatalf("reply = %+v, %v", got.run, got.err)
	}
	runID := got.run.RunID

	// The next start continues the run without a new message.
	factory, _, continued := durableFactory(false)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	done = make(chan error, 1)
	go func() {
		done <- New(durableAgentOptions(dir, factory, WithAddr("127.0.0.1:0"))...).Run(ctx)
	}()
	probe, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		state, err := probe.State(context.Background(), runID)
		if err != nil && !errors.Is(err, runtime.ErrRunNotFound) {
			t.Fatal(err)
		}
		if state == runtime.RunCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %s, want completed", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := continued.Load(); n != 1 {
		t.Fatalf("continued = %d, want 1", n)
	}
}
