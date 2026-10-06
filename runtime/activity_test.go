package runtime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

type recordingActivityLogger struct {
	mu     sync.Mutex
	events []Event
	call   func(Event)
}

func (l *recordingActivityLogger) LogActivity(ev Event) {
	if l.call != nil {
		l.call(ev)
	}
	l.mu.Lock()
	l.events = append(l.events, ev)
	l.mu.Unlock()
}

func (l *recordingActivityLogger) snapshot() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.events...)
}

func TestActivityLoggerAndEventBufferOptionOrder(t *testing.T) {
	t.Parallel()
	for _, loggerFirst := range []bool{true, false} {
		logger := &recordingActivityLogger{}
		var opts []RunnerOption
		if loggerFirst {
			opts = []RunnerOption{WithActivityLogger(logger), WithEventBuffer(2)}
		} else {
			opts = []RunnerOption{WithEventBuffer(2), WithActivityLogger(logger)}
		}
		r := NewRunner(NewMemoryJournal(), nil, opts...)
		r.Events().Publish(Event{RunID: "order", Type: "probe"})
		got := logger.snapshot()
		if len(got) != 1 || got[0].Type != "probe" {
			t.Fatalf("loggerFirst=%v: logged events = %+v, want probe", loggerFirst, got)
		}
	}
}

func TestNilActivityLoggerIsNoOp(t *testing.T) {
	t.Parallel()
	r := NewRunner(NewMemoryJournal(), nil, WithActivityLogger(nil), WithEventBuffer(1))
	r.Events().Publish(Event{RunID: "nil", Type: "probe"})
}

func TestActivityLoggerRunsAfterStampAndOutsideBusLock(t *testing.T) {
	t.Parallel()
	logger := &recordingActivityLogger{}
	var bus *EventBus
	logger.call = func(ev Event) {
		if ev.Time.IsZero() {
			t.Error("logger received event before timestamp was stamped")
		}
		_ = bus.Oldest(ev.RunID) // deadlocks if LogActivity is called under the bus lock.
	}
	r := NewRunner(NewMemoryJournal(), nil, WithActivityLogger(logger))
	bus = r.Events()
	got := bus.Publish(Event{RunID: "callback", Type: "probe"})
	if got.Time.IsZero() {
		t.Fatal("Publish returned an unstamped event")
	}
	logged := logger.snapshot()
	if len(logged) != 1 || !logged[0].Time.Equal(got.Time) {
		t.Fatalf("logged events = %+v, want the stamped published event", logged)
	}
}

func TestActivityLoggerReceivesRunStateAndResponse(t *testing.T) {
	t.Parallel()
	logger := &recordingActivityLogger{}
	factory, _ := fakeFactory(&kit.TurnResult{Response: "hello"})
	r := NewRunner(NewMemoryJournal(), factory, WithActivityLogger(logger))
	run, err := r.Start(context.Background(), "activity-run", Input{Text: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.State != RunCompleted || run.Response != "hello" {
		t.Fatalf("run = %+v, want completed response hello", run)
	}
	var running, completed, response bool
	for _, ev := range logger.snapshot() {
		switch {
		case ev.Type == EventState && ev.State == RunRunning:
			running = true
		case ev.Type == EventState && ev.State == RunCompleted:
			completed = true
		case ev.Type == EventResponse && ev.Text == "hello":
			response = true
		}
	}
	if !running || !completed || !response {
		t.Fatalf("activity events = %+v, want running state, response, and completed state", logger.snapshot())
	}
}

// Agent lifecycle events use the same logger as runtime state events.
func TestActivityLoggerReceivesAgentEvents(t *testing.T) {
	t.Parallel()
	logger := &recordingActivityLogger{}
	r := NewRunner(nil, nil, WithActivityLogger(logger))
	agent := &activityEventAgent{fakeAgent: &fakeAgent{}}
	stop := forwardAgentEvents(agent, r.Events(), "tools")
	defer stop()
	agent.listener(kit.ToolCallEvent{ToolCallID: "call-1", ToolName: "read"})
	events := logger.snapshot()
	if len(events) != 1 || events[0].RunID != "tools" || events[0].Type != string(kit.EventToolCall) {
		t.Fatalf("logged events = %+v", events)
	}
	var call kit.ToolCallEvent
	if err := json.Unmarshal(events[0].Data, &call); err != nil {
		t.Fatal(err)
	}
	if call.ToolName != "read" || call.ToolCallID != "call-1" {
		t.Fatalf("logged tool call = %+v", call)
	}
}

type activityEventAgent struct {
	*fakeAgent
	listener kit.EventListener
}

func (a *activityEventAgent) Subscribe(listener kit.EventListener) func() {
	a.listener = listener
	return func() { a.listener = nil }
}

// Reading a finished run through a second Runner must not repeat logs.
func TestActivityLoggerDoesNotLogReplay(t *testing.T) {
	t.Parallel()
	journal := NewMemoryJournal()
	factory, _ := fakeFactory(&kit.TurnResult{Response: "hello"})
	first := NewRunner(journal, factory)
	if _, err := first.Start(context.Background(), "replay", Input{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	logger := &recordingActivityLogger{}
	second := NewRunner(journal, nil, WithActivityLogger(logger))
	events, stop := second.StreamEvents("replay", 0)
	defer stop()
	for ev := range events {
		if ev.Type == EventState && ev.State == RunCompleted {
			break
		}
	}
	if got := logger.snapshot(); len(got) != 0 {
		t.Fatalf("replay logged events: %+v", got)
	}
}

func TestConcurrentActivityLoggerPublishes(t *testing.T) {
	t.Parallel()
	logger := &recordingActivityLogger{}
	// Configure through Runner so the test exercises the public option.
	r := NewRunner(NewMemoryJournal(), nil, WithActivityLogger(logger), WithEventBuffer(32))
	bus := r.Events()
	const publishers = 8
	const each = 25
	var wg sync.WaitGroup
	for p := range publishers {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := range each {
				bus.Publish(Event{RunID: "concurrent", Type: "probe", Text: string(rune('a' + (p+i)%26))})
			}
		}(p)
	}
	wg.Wait()
	if got := len(logger.snapshot()); got != publishers*each {
		t.Fatalf("logged %d events, want %d", got, publishers*each)
	}
}
