package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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
	ensured     int
	ensureRun   string
	ensureAt    int
	ensureErr   error
}

func (f *fakeClient) Lookup(_ context.Context, _ string) (string, int, error) {
	return f.lookupRun, f.lookupAt, f.lookupErr
}

func (f *fakeClient) Ensure(_ context.Context, _ string) (string, int, error) {
	f.ensured++
	if f.ensureErr != nil {
		return "", 0, f.ensureErr
	}
	if f.ensureRun == "" {
		f.ensureRun = "run-1"
	}
	return f.ensureRun, f.ensureAt, nil
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

// firstSend drives the first message through the handshake the TUI now
// performs: the run is resolved and the stream is opened before the turn is
// dispatched. It returns the model and the command that runs the turn.
func firstSend(t *testing.T, m Model, text string) (Model, tea.Cmd) {
	t.Helper()
	m, _ = submit(t, m, text)
	if m.pending == "" {
		t.Fatal("the first message was not held until the stream opened")
	}
	next, cmd := m.Update(ensureMsg{runID: "run-1"})
	m = next.(Model)
	if cmd != nil {
		_ = cmd() // opens the stream on the fake client
	}
	next, turn := m.Update(streamReadyMsg{ch: make(chan runtime.Event)})
	if turn == nil {
		t.Fatal("no turn was dispatched once the stream was open")
	}
	return next.(Model), turn
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

// runCmd executes a command and gives it a moment to produce a message. A
// command that parks — the live stream read waits for an event the harness
// never sends — reports nil, which is drive's signal that the loop has
// settled.
func runCmd(cmd tea.Cmd) tea.Msg {
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}

// drive runs a command, feeds its message to the model, and keeps going until
// the model produces no further command. This is how a synchronous event loop
// would settle.
func drive(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for cmd != nil {
		msg := runCmd(cmd)
		if msg == nil {
			return m
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, batched := range batch {
				m = drive(t, m, batched)
			}
			return m
		}
		next, ncmd := m.Update(msg)
		m = next.(Model)
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

// TestViewCursorStaysOnTheInputRowWhenTheFrameExceedsTheScreen pins the
// inline-mode cursor contract: bubbletea moves the terminal cursor to the
// screen row the view reports, and a transcript taller than the terminal has
// its top rows in scrollback. The view must subtract the scrolled rows, or
// the terminal clamps the move to its bottom row and the cursor lands below
// the footer (verified in tmux before the fix: cursor at the pane's last row
// while the input sat three rows up).
func TestViewCursorStaysOnTheInputRowWhenTheFrameExceedsTheScreen(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeClient{}) // 100x30 window
	for i := range 40 {
		m.commit(kindAssistant, fmt.Sprintf("answer line %d", i))
	}

	v := m.View()
	if v.Cursor == nil {
		t.Fatal("view has no cursor")
	}
	// 40 entries + header + status + input + footer text + trailing row is a
	// frame taller than the 30-row window; the test is only valid while it is.
	if rows := strings.Count(v.Content, "\n") + 1; rows <= m.height {
		t.Fatalf("frame has %d rows, want more than the %d-row screen", rows, m.height)
	}
	// The frame's bottom row is the screen's bottom row, and the input sits
	// two rows above it (footer text, then the row the trailing newline
	// opens) — so the input's screen row is height-3. Verified in tmux at
	// three window heights, typed and empty input.
	if v.Cursor.Y != m.height-3 {
		t.Fatalf("cursor row = %d, want %d (the input row, three above the screen bottom)",
			v.Cursor.Y, m.height-3)
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

	m, cmd := firstSend(t, m, "deploy the app")
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

	m, cmd := firstSend(t, m, "deploy the app")
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

	m, cmd := firstSend(t, m, "hi")
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

	m, cmd := firstSend(t, m, "boom")
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

// TestFirstTurnStreamsToolCalls is the mirror of
// TestStartupLookupResumesToolStream for a session whose address is not bound
// yet: the very first turn must also stream its tool and reasoning events.
//
// The regression it guards: the TUI used to learn its run ID from the reply
// to the first message, so the stream opened after that turn had finished.
// Reasoning deltas and tool events are live-only, so the first turn rendered
// as a bare answer and every later turn rendered in full — quit and reopen
// and the same agent suddenly "worked".
func TestFirstTurnStreamsToolCalls(t *testing.T) {
	t.Parallel()
	f := &fakeClient{lookupErr: ErrNotFound, ensureRun: "run-1"}
	m := newTestModel(f)

	// Startup lookup finds nothing; there is no run to stream yet.
	next, _ := m.Update(lookupMsg{err: ErrNotFound})
	m = next.(Model)

	// The user sends the first message. It is held, not dispatched.
	sent, cmd := m.send("ls")
	m = sent.(Model)
	if cmd == nil {
		t.Fatal("send produced no command")
	}
	if m.pending != "ls" {
		t.Fatalf("pending = %q, want the message held until the stream opens", m.pending)
	}
	if f.started || f.turns != 0 {
		t.Fatal("the turn was dispatched before the stream was open")
	}

	// Resolving the address yields the run and opens the stream.
	next, cmd = m.Update(ensureMsg{runID: "run-1", cursor: 0})
	m = next.(Model)
	if m.runID != "run-1" {
		t.Fatalf("runID = %q, want run-1 before the turn", m.runID)
	}
	if cmd == nil {
		t.Fatal("ensure produced no stream command")
	}
	_ = cmd()
	if f.streamed != 1 {
		t.Fatalf("stream opened %d times, want 1 before the turn", f.streamed)
	}

	// Only once the stream is live is the held turn dispatched.
	next, _ = m.Update(streamReadyMsg{ch: make(chan runtime.Event)})
	m = next.(Model)
	if m.pending != "" {
		t.Fatalf("pending = %q, want it dispatched once the stream was open", m.pending)
	}

	// Events of that first turn now reach the transcript.
	tc := kitEventToolCall("call-1", "shell", `{"command":"ls -la"}`)
	m.apply(runtime.Event{RunID: "run-1", Seq: 1, Type: tc[0], Data: []byte(tc[1])})
	if got := m.transcript(); !strings.Contains(got, styles.toolName.Render("shell")) {
		t.Fatalf("the first turn misses its tool call:\n%s", got)
	}
}

// A stream that will not open must not swallow the first message.
func TestFirstTurnSendsWhenTheStreamFails(t *testing.T) {
	t.Parallel()
	f := &fakeClient{lookupErr: ErrNotFound, ensureErr: errTest{}}
	m := newTestModel(f)

	sent, _ := m.send("ls")
	m = sent.(Model)

	next, cmd := m.Update(ensureMsg{err: errTest{}})
	m = next.(Model)
	if m.pending != "" {
		t.Fatalf("pending = %q, want the message sent anyway", m.pending)
	}
	if cmd == nil {
		t.Fatal("the held message was dropped when the run would not resolve")
	}
	if _ = cmd(); !f.started {
		t.Fatal("the fallback did not start the run by address")
	}
}
