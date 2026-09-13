package tui

import (
	"context"
	"encoding/json"
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
	lookupRun   string
	lookupAt    int
	lookupErr   error
}

func (f *fakeClient) Lookup(_ context.Context, _ string) (string, int, error) {
	return f.lookupRun, f.lookupAt, f.lookupErr
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

func TestModelRendersOneInputPromptAndRealCursor(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	view := m.View()
	if got := strings.Count(view.Content, "❯"); got != 1 {
		t.Fatalf("prompt count = %d, want 1:\n%s", got, view.Content)
	}
	if view.Cursor == nil {
		t.Fatal("view has no real textarea cursor")
	}
	wantY := strings.Count(m.renderPrefix(), "\n")
	if view.Cursor.Y != wantY {
		t.Fatalf("cursor row = %d, want input row %d", view.Cursor.Y, wantY)
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

	start := kitEventToolCallStart("call-1", "shell")
	m.apply(runtime.Event{RunID: "run-1", Seq: 1, Type: start[0], Data: []byte(start[1])})
	if got := m.transcript(); !strings.Contains(got, styles.toolMarker.Render(spinnerFrames[0])) ||
		!strings.Contains(got, styles.toolName.Render("shell")) {
		t.Fatalf("working tool has no colored spinner and name:\n%s", got)
	}

	tc := kitEventToolCall("call-1", "shell", `{"cmd":"ls"}`)
	m.apply(runtime.Event{RunID: "run-1", Seq: 1, Type: tc[0], Data: []byte(tc[1])})
	longResult := strings.Repeat("file.txt\n", 20)
	tr := kitEventToolResult("call-1", "shell", longResult)
	m.apply(runtime.Event{RunID: "run-1", Seq: 1, Type: tr[0], Data: []byte(tr[1])})

	rendered := m.transcript()
	for _, want := range []string{
		styles.toolMarker.Render("✓"), styles.toolName.Render("shell"),
		`{"cmd":"ls"}`, "file.txt file.txt", "…",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("transcript misses %q:\n%s", want, rendered)
		}
	}
	if got := strings.Count(rendered, "shell"); got != 1 {
		t.Errorf("tool rendered %d times, want one lifecycle line:\n%s", got, rendered)
	}
}

func TestToolResultWithoutStartStillRenders(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{})
	tr := kitEventToolResult("call-1", "read", "contents")
	m.apply(runtime.Event{RunID: "run-1", Seq: 1, Type: tr[0], Data: []byte(tr[1])})

	got := m.transcript()
	for _, want := range []string{
		styles.toolMarker.Render("✓"), styles.toolName.Render("read"),
		styles.toolResultMark.Render("→"), styles.toolResult.Render("contents"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("standalone result misses %q:\n%s", want, got)
		}
	}
}

func TestToolRenderingUsesDistinctColors(t *testing.T) {
	t.Parallel()
	got := renderTool(entry{
		kind: kindTool, text: `shell({"command":"ls"})`,
		toolDone: true, toolResult: "agent.yaml",
	}, 0)

	for _, want := range []string{
		styles.toolMarker.Render("✓"),
		styles.toolName.Render("shell"),
		styles.toolArgs.Render(`({"command":"ls"})`),
		styles.toolResultMark.Render("→"),
		styles.toolResult.Render("agent.yaml"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("colored tool entry misses %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, styles.reasoning.Render("shell")) {
		t.Fatalf("tool entry uses the reasoning style:\n%s", got)
	}
}

func TestToolResultTruncationIsUnicodeSafe(t *testing.T) {
	t.Parallel()
	got := resultSnippet(strings.Repeat("界", 100))
	if !strings.HasSuffix(got, "…") || len([]rune(got)) != 80 {
		t.Fatalf("resultSnippet = %q (%d runes), want 80 runes ending in ellipsis", got, len([]rune(got)))
	}
}

// TestStartupLookupResumesToolStream: a TUI that reopens an existing address
// resolves its run on init and opens the durable stream, so tool and reasoning
// events of the next turn are not missed.
func TestStartupLookupResumesToolStream(t *testing.T) {
	t.Parallel()
	f := &fakeClient{lookupRun: "run-9", lookupAt: 4}
	m := newTestModel(f)

	if f.streamed != 0 {
		t.Fatalf("stream opened %d times before the lookup finished", f.streamed)
	}
	next, cmd := m.Update(lookupMsg{runID: "run-9", cursor: 4})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("lookup produced no stream command")
	}
	_ = cmd()
	if m.runID != "run-9" || m.cursor != 4 {
		t.Fatalf("run = %q cursor = %d, want run-9 at 4", m.runID, m.cursor)
	}
	if f.streamed != 1 || len(f.streamAfter) != 1 || f.streamAfter[0] != 4 {
		t.Fatalf("stream calls = %d after %v, want one call after 4", f.streamed, f.streamAfter)
	}

	tc := kitEventToolCall("call-1", "shell", `{"command":"sleep 2"}`)
	m.apply(runtime.Event{RunID: "run-9", Seq: 5, Type: tc[0], Data: []byte(tc[1])})
	tr := kitEventToolResult("call-1", "shell", "")
	m.apply(runtime.Event{RunID: "run-9", Seq: 5, Type: tr[0], Data: []byte(tr[1])})
	if got := m.transcript(); !strings.Contains(got, styles.toolName.Render("shell")) {
		t.Fatalf("resumed session misses the tool call:\n%s", got)
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

func kitEventToolCallStart(callID, name string) [2]string {
	e := kit.ToolCallStartEvent{ToolCallID: callID, ToolName: name}
	return encodedKitEvent(kit.EventToolCallStart, e)
}

func kitEventToolCall(callID, name, args string) [2]string {
	e := kit.ToolCallEvent{ToolCallID: callID, ToolName: name, ToolArgs: args}
	return encodedKitEvent(kit.EventToolCall, e)
}

func kitEventToolResult(callID, name, result string) [2]string {
	e := kit.ToolResultEvent{ToolCallID: callID, ToolName: name, Result: result}
	return encodedKitEvent(kit.EventToolResult, e)
}

func encodedKitEvent(typ kit.EventType, value any) [2]string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return [2]string{string(typ), string(data)}
}
