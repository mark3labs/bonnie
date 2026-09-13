package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// fakeClient is a scripted [Client]; no network is involved.
type fakeClient struct {
	started     bool
	turns       int
	startRes    *runtime.Run
	sendRes     *runtime.Run
	respond     *runtime.Run
	errs        []error
	events      chan runtime.Event
	streamed    int
	streamAfter []int
	cancelled   int
}

func (f *fakeClient) Start(_ context.Context, _ string, _ string) (*runtime.Run, error) {
	if f.startRes == nil {
		f.startRes = &runtime.Run{ID: "run-1", State: runtime.RunWaiting,
			Suspend: &runtime.SuspendRequest{Prompt: "which region?"}}
	}
	f.started = true
	if len(f.errs) > 0 {
		return nil, f.errs[0]
	}
	return f.startRes, nil
}

func (f *fakeClient) Send(_ context.Context, runID, _ string) (*runtime.Run, error) {
	f.turns++
	if len(f.errs) > 0 {
		return nil, f.errs[0]
	}
	if f.sendRes != nil {
		return f.sendRes, nil
	}
	return &runtime.Run{ID: runID, State: runtime.RunCompleted, Response: "answer"}, nil
}

func (f *fakeClient) Respond(_ context.Context, runID, _ string) (*runtime.Run, error) {
	f.turns++
	if len(f.errs) > 0 {
		return nil, f.errs[0]
	}
	if f.respond != nil {
		return f.respond, nil
	}
	return &runtime.Run{ID: runID, State: runtime.RunCompleted, Response: "done"}, nil
}

func (f *fakeClient) Cancel(_ context.Context, _ string) error {
	f.cancelled++
	return nil
}

func (f *fakeClient) Stream(_ context.Context, _ string, after int) (<-chan runtime.Event, func(), error) {
	f.streamed++
	f.streamAfter = append(f.streamAfter, after)
	ch := make(chan runtime.Event)
	if f.events != nil {
		go func() {
			defer close(ch)
			for ev := range f.events {
				ch <- ev
			}
		}()
	}
	return ch, func() {}, nil
}

// keyEnter is the enter key as bubbletea v2 delivers it.
func keyEnter() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }

func newTestModel(c Client) Model {
	m := New(c, context.Background(), "tui-test")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return next.(Model)
}

func keyText(text string) tea.KeyPressMsg {
	runes := []rune(text)
	return tea.KeyPressMsg{Code: runes[0], Text: text}
}

func keyCtrl(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl}
}

// submit types text into the input and presses enter, returning the command
// the model produced for the send.
func submit(t *testing.T, m Model, text string) (Model, tea.Cmd) {
	t.Helper()
	m.input.SetValue(text)
	next, cmd := m.Update(keyEnter())
	if cmd == nil {
		t.Fatalf("enter produced no command for %q", text)
	}
	return next.(Model), cmd
}

// drive runs a command, feeds its message to the model, and keeps going until
// the model produces no further command. This is how a synchronous event loop
// would settle.
func drive(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for cmd != nil {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, batched := range batch {
				m = drive(t, m, batched)
			}
			return m
		}
		next, ncmd := m.Update(msg)
		m = next.(Model)
		if m.streamCh != nil {
			return m
		}
		cmd = ncmd
	}
	return m
}

func TestModelAcceptsTypedKeys(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	if !m.input.Focused() {
		t.Fatal("textarea is not focused")
	}

	next, _ := m.Update(keyText("h"))
	m = next.(Model)
	next, _ = m.Update(keyText("i"))
	m = next.(Model)
	if got := m.input.Value(); got != "hi" {
		t.Fatalf("input = %q, want hi", got)
	}
}

func TestModelDropsLeakedTerminalColorReply(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	next, _ := m.Update(keyText("]10;rgb:ffff/ffff/ffff"))
	m = next.(Model)
	if got := m.input.Value(); got != "" {
		t.Fatalf("input = %q, want leaked terminal reply to be dropped", got)
	}
}

func TestFinalResponseReplacesStreamedDraft(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.appendAssistant("draft")
	m.apply(runtime.Event{Seq: 2, Type: runtime.EventResponse, Text: "final"})
	m.apply(runtime.Event{Seq: 2, Type: runtime.EventResponse, Text: "final"})
	if len(m.entries) != 1 {
		t.Fatalf("entries = %+v, want one final assistant entry", m.entries)
	}
	if m.entries[0].text != "final" || m.entries[0].streamed {
		t.Fatalf("entry = %+v, want closed final response", m.entries[0])
	}
}

// TestModelFirstTurnParks: a fresh conversation starts a run that parks on a
// question; the transcript carries the user message and the question.
func TestSpinnerAdvancesDuringTurn(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	m.spin = true
	next, cmd := m.Update(spinnerMsg{})
	m = next.(Model)
	if m.frame != 1 || cmd == nil {
		t.Fatalf("spinner frame = %d, cmd nil = %t", m.frame, cmd == nil)
	}
}

func TestModelFirstTurnParks(t *testing.T) {
	t.Parallel()
	f := &fakeClient{}
	m := newTestModel(f)

	m, cmd := submit(t, m, "deploy the app")
	m = drive(t, m, cmd)

	if !f.started {
		t.Fatal("Start was not called on the first message")
	}
	if m.state != statusWaiting {
		t.Fatalf("state = %v, want waiting", m.state)
	}
	rendered := m.render()
	if !strings.Contains(rendered, "deploy the app") {
		t.Errorf("transcript misses the user message:\n%s", rendered)
	}
	if !strings.Contains(rendered, "which region?") {
		t.Errorf("transcript misses the question:\n%s", rendered)
	}
}

// TestModelResumeAnswersAQuestion: a parked run is answered by the next send,
// which routes to Respond, and the transcript reaches the final answer.
func TestModelResumeAnswersAQuestion(t *testing.T) {
	t.Parallel()
	f := &fakeClient{}
	m := newTestModel(f)

	m, cmd := submit(t, m, "deploy the app")
	m = drive(t, m, cmd)
	if m.state != statusWaiting {
		t.Fatalf("state = %v, want waiting before respond", m.state)
	}

	m, cmd = submit(t, m, "eu-west-1")
	m = drive(t, m, cmd)

	if f.turns != 1 {
		t.Fatalf("respond called %d times, want 1", f.turns)
	}
	if m.state != statusDone {
		t.Fatalf("state = %v, want done", m.state)
	}
	rendered := m.render()
	if !strings.Contains(rendered, "done") {
		t.Errorf("transcript misses the final answer:\n%s", rendered)
	}
}

// TestModelRendersAssistantAnswer on a completed turn.
func TestModelRendersAssistantAnswer(t *testing.T) {
	t.Parallel()
	f := &fakeClient{startRes: &runtime.Run{ID: "run-1", State: runtime.RunCompleted, Response: "hello there"}}
	m := newTestModel(f)

	m, cmd := submit(t, m, "hi")
	m = drive(t, m, cmd)

	rendered := m.render()
	if !strings.Contains(rendered, "hello there") {
		t.Errorf("transcript misses the assistant answer:\n%s", rendered)
	}
}

// TestModelStreamsToolCalls: live tool events are folded into the transcript.
func TestModelStreamsToolCalls(t *testing.T) {
	t.Parallel()
	f := &fakeClient{}
	m := newTestModel(f)
	m.runID = "run-1"

	tc := kitEventToolCall("shell", `{"cmd":"ls"}`)
	m.apply(runtime.Event{RunID: "run-1", Seq: 1, Type: tc[0], Data: []byte(tc[1])})
	tr := kitEventToolResult("shell", "file.txt")
	m.apply(runtime.Event{RunID: "run-1", Seq: 2, Type: tr[0], Data: []byte(tr[1])})

	rendered := m.render()
	if !strings.Contains(rendered, "shell") {
		t.Errorf("transcript misses the tool call:\n%s", rendered)
	}
}

func TestModelReconnectsFromLastCursor(t *testing.T) {
	t.Parallel()
	f := &fakeClient{}
	m := newTestModel(f)
	m.runID = "run-1"
	m.streamCh = make(chan runtime.Event)
	m.apply(runtime.Event{RunID: "run-1", Seq: 7, Type: runtime.EventState, State: runtime.RunRunning})

	next, cmd := m.Update(streamEndMsg{})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("closed stream produced no reconnect command")
	}
	next, cmd = m.Update(reconnectMsg{})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("reconnect produced no stream command")
	}
	msg := cmd()
	next, _ = m.Update(msg)
	m = next.(Model)
	if f.streamed != 1 || len(f.streamAfter) != 1 || f.streamAfter[0] != 7 {
		t.Fatalf("stream calls = %d after %v, want one call after 7", f.streamed, f.streamAfter)
	}
}

func TestModelCancelCallsChannel(t *testing.T) {
	t.Parallel()
	f := &fakeClient{}
	m := newTestModel(f)
	m.runID = "run-1"
	m.spin = true
	m.state = statusRunning

	next, cmd := m.Update(keyCtrl('w'))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("ctrl+w produced no cancel command")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if f.cancelled != 1 {
		t.Fatalf("cancel calls = %d, want 1", f.cancelled)
	}
	if m.label != "cancelling" {
		t.Fatalf("label = %q, want cancelling", m.label)
	}
}

// TestErrorShowsOnFailure: a failed entry point sets the error state and
// records the message.
func TestErrorShowsOnFailure(t *testing.T) {
	t.Parallel()
	f := &fakeClient{errs: []error{errTest{}}}
	m := newTestModel(f)

	m, cmd := submit(t, m, "boom")
	m = drive(t, m, cmd)
	if m.state != statusError {
		t.Fatalf("state = %v, want error", m.state)
	}
	rendered := m.render()
	if !strings.Contains(rendered, "boom thing") {
		t.Errorf("transcript misses the error:\n%s", rendered)
	}
}

type errTest struct{}

func (errTest) Error() string { return "boom thing" }

func kitEventToolCall(name, args string) [2]string {
	return [2]string{string(kit.EventToolCall), `{"ToolName":"` + name + `","ToolArgs":"` + args + `"}`}
}

func kitEventToolResult(name, result string) [2]string {
	return [2]string{string(kit.EventToolResult), `{"ToolName":"` + name + `","Result":"` + result + `"}`}
}
