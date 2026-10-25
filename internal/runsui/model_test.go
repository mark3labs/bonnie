package runsui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/wire"
)

// fakeJournal serves fixed runs and counts replays, so a test can prove a
// refresh reads only what changed.
type fakeJournal struct {
	mu      sync.Mutex
	recs    map[string][]runtime.Record
	states  map[string]runtime.RunState
	replays map[string]int
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{recs: map[string][]runtime.Record{}, states: map[string]runtime.RunState{}, replays: map[string]int{}}
}

func (j *fakeJournal) put(id string, st runtime.RunState, recs []runtime.Record) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recs[id], j.states[id] = recs, st
}

func (j *fakeJournal) Runs(context.Context, runtime.RunState) ([]string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var ids []string
	for id := range j.recs {
		ids = append(ids, id)
	}
	return ids, nil
}

func (j *fakeJournal) Replay(_ context.Context, id string) ([]runtime.Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.replays[id]++
	recs, ok := j.recs[id]
	if !ok {
		return nil, runtime.ErrRunNotFound
	}
	return recs, nil
}

func (j *fakeJournal) State(_ context.Context, id string) (runtime.RunState, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.states[id], nil
}

func (j *fakeJournal) Position(_ context.Context, id string) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.recs[id]), nil
}

// fakeServer records the actions it receives.
type fakeServer struct {
	mu        sync.Mutex
	responses []wire.InputResponse
	sent      []string
	cancels   []string
	resets    []string
}

func (s *fakeServer) Health(context.Context) (client.Health, error) {
	return client.Health{OK: true}, nil
}
func (s *fakeServer) Info(context.Context) (client.Info, error) {
	return client.Info{Agent: "test-agent", Version: "v1"}, nil
}
func (s *fakeServer) Send(_ context.Context, id, text string) (*wire.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, id+":"+text)
	return &wire.Run{ID: id, State: runtime.RunCompleted}, nil
}
func (s *fakeServer) RespondWith(_ context.Context, id string, r []wire.InputResponse) (*wire.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, r...)
	return &wire.Run{ID: id, State: runtime.RunRunning}, nil
}
func (s *fakeServer) RequestCancel(_ context.Context, id, turn string) (wire.CancelResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels = append(s.cancels, id+":"+turn)
	return wire.CancelResult{Status: wire.CancelRequested}, nil
}
func (s *fakeServer) Reset(_ context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets = append(s.resets, id+":"+reason)
	return nil
}
func (s *fakeServer) Clear(context.Context, string) error   { return nil }
func (s *fakeServer) Compact(context.Context, string) error { return nil }

// harness drives a model the way Bubble Tea does: every command a step
// returns runs, and its message goes back through Update, except ticks,
// which would loop forever.
type harness struct {
	t *testing.T
	m model
}

func newHarness(t *testing.T, j Journal, srv Server) *harness {
	t.Helper()
	opts := Options{Journal: j, JournalPath: "/tmp/journal.db"}
	if srv != nil {
		opts.Server = srv
		opts.ServerAddr = "http://127.0.0.1:8080"
	}
	m := newModel(t.Context(), opts)
	m.now = func() time.Time { return t0.Add(2 * time.Hour) }
	h := &harness{t: t, m: m}
	h.send(tea.WindowSizeMsg{Width: 140, Height: 40})
	h.send(m.load(m.gen)())
	if srv != nil {
		h.send(checkServer(t.Context(), srv)())
	}
	return h
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	next, cmd := h.m.Update(msg)
	h.m = next.(model)
	h.run(cmd)
}

// run executes cmd and feeds its messages back, skipping timers.
func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(50 * time.Millisecond):
		return // a tick: let it go
	}
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			h.run(c)
		}
	case nil, refreshMsg, healthMsg, toastExpiredMsg:
	default:
		if _, quit := msg.(tea.QuitMsg); quit {
			return
		}
		next, cmd := h.m.Update(msg)
		h.m = next.(model)
		h.run(cmd)
	}
}

func (h *harness) keys(keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		h.send(keyPress(k))
	}
}

func keyPress(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	}
	r := []rune(k)[0]
	return tea.KeyPressMsg{Code: r, Text: k}
}

func (h *harness) screen() string {
	return ansi.Strip(h.m.View().Content)
}

// twoRuns is a journal with a waiting approval and a completed run that
// was updated earlier.
func twoRuns(t *testing.T) *fakeJournal {
	j := newFakeJournal()
	b := sampleRun(t)
	j.put("run-1", runtime.RunWaiting, b.recs)
	old := newBuilder(t, "run-0")
	old.at = t0.Add(-time.Hour)
	old.turn("T0", "").state(runtime.RunRunning).after(time.Second).state(runtime.RunCompleted)
	j.put("run-0", runtime.RunCompleted, old.recs)
	j.put(runtime.ReservedRunPrefix+"addresses", runtime.RunPending, nil)
	return j
}

func TestConsoleListsRunsNewestFirstAndHidesReserved(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	if len(h.m.runs) != 2 || h.m.runs[0].ID != "run-1" || h.m.selID != "run-1" {
		t.Fatalf("runs = %v, selected %q", h.m.runs, h.m.selID)
	}
	s := h.screen()
	for _, want := range []string{"Deploy the app", "run-0", "Overview", "waiting for approval", "read only"} {
		if !strings.Contains(s, want) {
			t.Fatalf("screen is missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, runtime.ReservedRunPrefix) {
		t.Fatal("a reserved run reached the console")
	}
}

// Every frame fills the terminal exactly: no line wider than the screen,
// and no more lines than it has, at any size and on any tab.
func TestConsoleFitsTheScreen(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), &fakeServer{})
	for _, size := range [][2]int{{140, 40}, {90, 24}, {60, 20}, {200, 60}} {
		h.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for range tabCount {
			h.keys("]")
			lines := strings.Split(h.m.View().Content, "\n")
			if len(lines) > size[1] {
				t.Fatalf("%v tab %s: %d lines", size, h.m.tab, len(lines))
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > size[0] {
					t.Fatalf("%v tab %s: line %d is %d wide: %q", size, h.m.tab, i, w, ansi.Strip(l))
				}
			}
		}
	}
}

func TestConsoleNavigation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	h.keys("j")
	if h.m.selID != "run-0" {
		t.Fatalf("j selected %q", h.m.selID)
	}
	h.keys("k", "tab")
	if h.m.selID != "run-1" || h.m.focus != focusTurns {
		t.Fatalf("selected %q focus %d", h.m.selID, h.m.focus)
	}
	// Enter on a turn opens its trace.
	h.keys("enter")
	if h.m.focus != focusMain || h.m.tab != tabTrace {
		t.Fatalf("focus %d tab %s", h.m.focus, h.m.tab)
	}
	if s := h.screen(); !strings.Contains(s, "invoke_agent") || !strings.Contains(s, "0af7651916cd43dd8448eb211c80319c") {
		t.Fatalf("trace tab:\n%s", s)
	}
	h.keys("j", "j")
	if h.m.spanCursor != 2 || !strings.Contains(h.screen(), "finish_reason") {
		t.Fatalf("span cursor %d:\n%s", h.m.spanCursor, h.screen())
	}
	h.keys("]")
	if h.m.tab != tabJournal {
		t.Fatalf("tab = %s", h.m.tab)
	}
	h.keys("G", "enter")
	if !h.m.recOpen || !strings.Contains(h.screen(), "state") {
		t.Fatal("enter did not open the last record")
	}
}

// While the filter is open, printable keys are text, not commands.
func TestConsoleFilterOwnsKeys(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	h.keys("/", "q", "esc", "/")
	for _, k := range []string{"r", "u", "n", "-", "0"} {
		h.keys(k)
	}
	if h.m.filter.Value() != "run-0" || len(h.m.runs) != 1 || h.m.selID != "run-0" {
		t.Fatalf("filter %q runs %d selected %q", h.m.filter.Value(), len(h.m.runs), h.m.selID)
	}
	h.keys("enter")
	if h.m.filtering || h.m.filter.Value() != "run-0" {
		t.Fatal("enter must keep the filter and close the input")
	}
	h.keys("f")
	if h.m.stateFilt != FilterActive || len(h.m.runs) != 0 {
		t.Fatalf("state filter %s left %d runs", h.m.stateFilt, len(h.m.runs))
	}
}

// Without a server, an action is refused with a reason, and no dialog
// opens.
func TestConsoleReadOnlyRefusesActions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	h.keys("a")
	if h.m.modal != nil || !strings.Contains(h.m.toast, "read only") {
		t.Fatalf("modal %v toast %q", h.m.modal, h.m.toast)
	}
}

// Answering an approval sends the decision and the suspended turn's ID,
// so a stale answer cannot approve a later request.
func TestConsoleAnswersAnApproval(t *testing.T) {
	t.Parallel()
	srv := &fakeServer{}
	h := newHarness(t, twoRuns(t), srv)
	if !h.m.online {
		t.Fatal("server not seen")
	}
	h.keys("a")
	if h.m.modal == nil || !h.m.modal.approval {
		t.Fatalf("modal = %+v", h.m.modal)
	}
	if s := h.screen(); !strings.Contains(s, "Approve prod?") || !strings.Contains(s, "Reject") {
		t.Fatalf("dialog:\n%s", s)
	}
	h.keys("j", "tab")
	for _, k := range "no" {
		h.keys(string(k))
	}
	h.keys("enter")
	if h.m.modal != nil || len(srv.responses) != 1 {
		t.Fatalf("modal %v responses %v", h.m.modal, srv.responses)
	}
	r := srv.responses[0]
	if r.TurnID != "T1" || r.Approved == nil || *r.Approved || r.Text != "no" {
		t.Fatalf("response = %+v", r)
	}
	if !strings.Contains(h.m.toast, "answer sent") {
		t.Fatalf("toast = %q", h.m.toast)
	}
}

func TestConsoleRetireNeedsConfirmation(t *testing.T) {
	t.Parallel()
	srv := &fakeServer{}
	h := newHarness(t, twoRuns(t), srv)
	h.keys("x", "esc")
	if len(srv.resets) != 0 || h.m.modal != nil {
		t.Fatal("esc must close the dialog without retiring")
	}
	h.keys("x")
	for _, k := range "dup" {
		h.keys(string(k))
	}
	h.keys("enter")
	if len(srv.resets) != 1 || srv.resets[0] != "run-1:dup" {
		t.Fatalf("resets = %v", srv.resets)
	}
}

// A completed run has nothing to answer or cancel; the console says why.
func TestConsoleRefusesActionsTheRunCannotTake(t *testing.T) {
	t.Parallel()
	srv := &fakeServer{}
	h := newHarness(t, twoRuns(t), srv)
	h.keys("j", "a")
	if h.m.modal != nil || !strings.Contains(h.m.toast, "not waiting") {
		t.Fatalf("modal %v toast %q", h.m.modal, h.m.toast)
	}
	h.keys("c")
	if h.m.modal != nil || !strings.Contains(h.m.toast, "no turn to cancel") {
		t.Fatalf("toast %q", h.m.toast)
	}
}

// A refresh replays only the runs whose record count changed.
func TestConsoleRefreshIsIncremental(t *testing.T) {
	t.Parallel()
	j := twoRuns(t)
	h := newHarness(t, j, nil)
	b := newBuilder(t, "run-0")
	b.turn("T0", "").state(runtime.RunRunning).after(time.Second).state(runtime.RunCompleted).state(runtime.RunRetired)
	j.put("run-0", runtime.RunRetired, b.recs)
	h.send(h.m.load(h.m.gen)())
	if j.replays["run-1"] != 1 || j.replays["run-0"] != 2 {
		t.Fatalf("replays = %v", j.replays)
	}
	if h.m.cache["run-0"].State != runtime.RunRetired {
		t.Fatal("the changed run was not read again")
	}
}

// A load that a newer one has replaced must not overwrite it.
func TestConsoleIgnoresStaleLoad(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	h.m.gen = 5
	h.send(loadedMsg{gen: 4, err: errors.New("old")})
	if h.m.err != nil || len(h.m.runs) != 2 {
		t.Fatal("a stale load changed the model")
	}
}

func TestConsoleMouse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	// The second row of the runs panel: its border is one line below the
	// panel's top.
	r := h.m.rRuns
	h.send(tea.MouseClickMsg{X: r.x + 3, Y: r.y + 2, Button: tea.MouseLeft})
	if h.m.selID != "run-0" || h.m.focus != focusRuns {
		t.Fatalf("click selected %q focus %d", h.m.selID, h.m.focus)
	}
	h.send(tea.MouseClickMsg{X: h.m.tabX[tabJournal] + 1, Y: h.m.rTabs.y, Button: tea.MouseLeft})
	if h.m.tab != tabJournal || h.m.focus != focusMain {
		t.Fatalf("tab click: tab %s focus %d", h.m.tab, h.m.focus)
	}
	h.send(tea.MouseWheelMsg{X: r.x + 3, Y: r.y + 2, Button: tea.MouseWheelUp})
	if h.m.selID != "run-1" {
		t.Fatalf("wheel selected %q", h.m.selID)
	}
}

// The conversation tab shows each tool call with its result, and search
// finds text in it.
func TestConsoleConversationAndSearch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	h.keys("]", "3")
	s := h.screen()
	for _, want := range []string{"deploy it", "⚙ shell", "deploy failed", "Approve prod?"} {
		if !strings.Contains(s, want) {
			t.Fatalf("conversation is missing %q:\n%s", want, s)
		}
	}
	h.keys("/")
	for _, k := range "deploy" {
		h.keys(string(k))
	}
	h.keys("enter")
	if h.m.query != "deploy" || !strings.Contains(h.screen(), "/deploy") {
		t.Fatalf("query %q:\n%s", h.m.query, h.screen())
	}
}

// The wheel over the detail pane of the trace and journal tabs scrolls the
// detail. Over the list above it, the wheel still moves the cursor. Before,
// the wheel moved the cursor everywhere, and a long detail could not be read
// with the mouse.
func TestConsoleWheelScrollsTheDetail(t *testing.T) {
	t.Parallel()
	h := newHarness(t, twoRuns(t), nil)
	wheel := func(y int, b tea.MouseButton) {
		h.send(tea.MouseWheelMsg{X: h.m.rMain.x + 3, Y: y, Button: b})
	}

	h.send(tea.MouseClickMsg{X: h.m.tabX[tabJournal] + 1, Y: h.m.rTabs.y, Button: tea.MouseLeft})
	h.keys("enter") // open the first record's payload
	h.screen()      // the view measures the detail pane
	r, _ := h.m.current()
	long := -1
	for i, rec := range r.Records {
		if strings.Count(recordBody(rec, h.m.rMain.w-2), "\n") > h.m.detailH+3 {
			long = i
			break
		}
	}
	if long < 0 {
		t.Fatal("the sample run has no record longer than the detail pane")
	}
	h.m.recCursor = long
	h.screen()
	if h.m.detailY == 0 {
		t.Fatal("the journal shows no detail pane")
	}
	wheel(h.m.detailY+1, tea.MouseWheelDown)
	if h.m.detailTop != 3 || h.m.recCursor != long {
		t.Fatalf("wheel on the payload: detailTop %d cursor %d", h.m.detailTop, h.m.recCursor)
	}
	wheel(h.m.detailY+1, tea.MouseWheelUp)
	if h.m.detailTop != 0 {
		t.Fatalf("wheel up on the payload: detailTop %d", h.m.detailTop)
	}
	wheel(h.m.rMain.y+3, tea.MouseWheelDown)
	if h.m.recCursor != long+1 {
		t.Fatalf("wheel on the list: cursor %d, want %d", h.m.recCursor, long+1)
	}

	h.send(tea.MouseClickMsg{X: h.m.tabX[tabTrace] + 1, Y: h.m.rTabs.y, Button: tea.MouseLeft})
	h.screen()
	if h.m.detailY == 0 {
		t.Fatal("the trace shows no detail pane")
	}
	cursor := h.m.spanCursor
	wheel(h.m.detailY, tea.MouseWheelDown)
	if h.m.spanCursor != cursor {
		t.Fatalf("wheel on the span detail moved the cursor to %d", h.m.spanCursor)
	}
}
