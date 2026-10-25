package runsui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/internal/selection"
	"github.com/mark3labs/bonnie/runtime"
)

// Journal is the read-only journal API the console uses. A journal that
// also implements [runtime.Positioner] is refreshed incrementally: only the
// runs whose record count changed are read again.
type Journal interface {
	Runs(context.Context, runtime.RunState) ([]string, error)
	Replay(context.Context, string) ([]runtime.Record, error)
	State(context.Context, string) (runtime.RunState, error)
}

// Options configures [Run].
type Options struct {
	// Journal is read on every refresh. The caller owns and closes it.
	Journal Journal
	// JournalPath is shown in the status panel.
	JournalPath string
	// Server sends operator actions. Nil makes the console read only.
	Server Server
	// ServerAddr is shown in the status panel.
	ServerAddr string
	// RunID selects a run when the journal loads.
	RunID string
	// Refresh is the journal poll interval. Zero means one second.
	Refresh time.Duration
}

// Open runs the console and blocks until the user quits or ctx ends.
func Open(ctx context.Context, opts Options) error {
	p := tea.NewProgram(newModel(ctx, opts), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return fmt.Errorf("bonnie: runs console: %w", err)
	}
	return nil
}

type focusArea int

const (
	focusRuns focusArea = iota
	focusTurns
	focusMain
)

type tab int

const (
	tabOverview tab = iota
	tabConversation
	tabTrace
	tabJournal
	tabCount
)

func (t tab) String() string {
	return [...]string{"Overview", "Conversation", "Trace", "Journal"}[t]
}

type loadedMsg struct {
	gen  uint64
	runs map[string]Run
	err  error
}

type refreshMsg struct{}
type healthMsg struct{}
type toastExpiredMsg struct{ id int }

// rect is a screen region, for mouse hit tests.
type rect struct{ x, y, w, h int }

func (r rect) has(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

type model struct {
	ctx  context.Context
	opts Options
	now  func() time.Time

	cache   map[string]Run
	runs    []Run // filtered and sorted
	gen     uint64
	loading bool
	loaded  bool
	err     error

	selID   string
	sel     int
	runsTop int

	turnSel int // index into the selected run's turns, newest first
	turnTop int

	focus focusArea
	tab   tab
	zoom  bool

	filter     textinput.Model
	filtering  bool
	stateFilt  StateFilter
	sortKey    SortKey
	search     textinput.Model
	searching  bool
	query      string
	reasoning  bool
	allResults bool

	main       viewport.Model
	mainKey    string // what main holds, to skip needless renders
	spanCursor int
	spanTop    int
	detailTop  int
	detailY    int // screen row of the detail pane's first line; 0 when none shows
	detailH    int // rows the detail pane shows
	recCursor  int
	recTop     int
	recOpen    bool

	modal *modal

	online     bool
	serverInfo client.Info
	serverErr  error

	toast    string
	toastErr bool
	toastID  int
	busy     string

	spin          spinner.Model
	width, height int

	// Regions from the last layout, for the mouse.
	rRuns, rTurns, rMain, rTabs rect
	tabX                        []int

	// textSel is a mouse selection inside selArea: the body of the main
	// panel, without its border. On the tabs that scroll (selScroll), its
	// lines are rows of mainRows, so it stays on its text while the panel
	// scrolls. On the trace and journal tabs, which are lists drawn for
	// each frame, its lines are screen rows: it marks cells, and anything
	// that moves the list drops it.
	textSel   selection.Selection
	selArea   rect
	selScroll bool
	// mainRows is what main holds on the tabs that scroll, with the plain
	// text and soft breaks that a selection copies. mainShape is the run,
	// tab, width, and options it was built for: when one changes, the rows
	// move, and the selection is dropped.
	mainRows  selection.Wrapped
	mainShape string
	// pressAt is where the left button went down in the main panel. The
	// panel's own click (select a span, open a record) runs on release, and
	// only when the press did not drag: a drag that started with a click
	// would change the text it is selecting.
	pressAt tea.Mouse
}

func newModel(ctx context.Context, opts Options) model {
	if opts.Refresh <= 0 {
		opts.Refresh = time.Second
	}
	fi := textinput.New()
	fi.Prompt = "/ "
	fi.Placeholder = "filter runs"
	si := textinput.New()
	si.Prompt = "search: "
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(sCyan))
	vp := viewport.New()
	vp.SoftWrap = false
	vp.HighlightStyle = lipgloss.NewStyle().Background(lipgloss.Color("58"))
	vp.SelectedHighlightStyle = lipgloss.NewStyle().Background(colorYellow).Foreground(lipgloss.Color("0"))
	m := model{
		ctx: ctx, opts: opts, now: time.Now,
		cache: map[string]Run{}, selID: opts.RunID, loading: true, gen: 1,
		filter: fi, search: si, spin: sp, main: vp,
	}
	if opts.RunID != "" {
		m.focus = focusMain
	}
	return m
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.load(m.gen), m.spin.Tick, tick(m.opts.Refresh, refreshMsg{})}
	if m.opts.Server != nil {
		cmds = append(cmds, checkServer(m.ctx, m.opts.Server))
	}
	return tea.Batch(cmds...)
}

func tick(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

// load reads the journal. Runs whose record count is unchanged are reused
// from prev without a replay, so a refresh of a large journal is cheap.
func (m model) load(gen uint64) tea.Cmd {
	prev, j, ctx := m.cache, m.opts.Journal, m.ctx
	return func() tea.Msg {
		ids, err := j.Runs(ctx, "")
		if err != nil {
			return loadedMsg{gen: gen, err: err}
		}
		pos, _ := j.(runtime.Positioner)
		next := make(map[string]Run, len(ids))
		for _, id := range ids {
			if runtime.IsReservedRun(id) {
				continue
			}
			if old, ok := prev[id]; ok && pos != nil {
				if n, err := pos.Position(ctx, id); err == nil && n == len(old.Records) {
					next[id] = old
					continue
				}
			}
			recs, err := j.Replay(ctx, id)
			if errors.Is(err, runtime.ErrRunNotFound) {
				continue
			}
			if err != nil {
				return loadedMsg{gen: gen, err: err}
			}
			st, err := j.State(ctx, id)
			if err != nil && !errors.Is(err, runtime.ErrRunNotFound) {
				return loadedMsg{gen: gen, err: err}
			}
			next[id] = Summarize(id, st, recs)
		}
		return loadedMsg{gen: gen, runs: next}
	}
}

func (m *model) startLoad() tea.Cmd {
	m.gen++
	m.loading = true
	return m.load(m.gen)
}

// rebuild applies the filter and sort to the cache and keeps the
// selection on the same run when it is still listed.
func (m *model) rebuild() {
	all := make([]Run, 0, len(m.cache))
	q := m.filter.Value()
	for _, r := range m.cache {
		if m.stateFilt.Match(r) && matchText(r, q) {
			all = append(all, r)
		}
	}
	SortRuns(all, m.sortKey)
	m.runs = all
	m.sel = 0
	for i, r := range all {
		if r.ID == m.selID {
			m.sel = i
			break
		}
	}
	if len(all) > 0 {
		if all[m.sel].ID != m.selID {
			m.selectRun(m.sel)
		}
	} else {
		m.selID = ""
	}
	m.clampTurn()
}

func (m *model) selectRun(i int) {
	if len(m.runs) == 0 {
		return
	}
	i = min(max(i, 0), len(m.runs)-1)
	changed := m.runs[i].ID != m.selID
	m.sel = i
	m.selID = m.runs[i].ID
	if changed {
		m.turnSel, m.turnTop = 0, 0
		m.spanCursor, m.spanTop, m.detailTop = 0, 0, 0
		m.recCursor, m.recTop, m.recOpen = 0, 0, false
		m.main.GotoTop()
		m.mainKey = ""
		m.clearSearch()
	}
}

func (m *model) current() (Run, bool) {
	if m.sel < len(m.runs) && len(m.runs) > 0 {
		return m.runs[m.sel], true
	}
	return Run{}, false
}

// turns returns the selected run's turns, newest first.
func (m *model) turns() []Turn {
	r, ok := m.current()
	if !ok {
		return nil
	}
	out := make([]Turn, len(r.Turns))
	for i, t := range r.Turns {
		out[len(r.Turns)-1-i] = t
	}
	return out
}

func (m *model) clampTurn() {
	n := len(m.turns())
	m.turnSel = min(max(m.turnSel, 0), max(0, n-1))
}

func (m *model) currentTrace() (Trace, bool) {
	r, ok := m.current()
	ts := m.turns()
	if !ok || len(ts) == 0 {
		return Trace{}, false
	}
	return BuildTrace(r, ts[m.turnSel], m.now()), true
}

func (m *model) setToast(s string, isErr bool) tea.Cmd {
	m.toastID++
	m.toast, m.toastErr = s, isErr
	id := m.toastID
	return tick(4*time.Second, toastExpiredMsg{id: id})
}

func (m *model) clearSearch() {
	m.query = ""
	m.searching = false
	m.search.SetValue("")
	m.main.ClearHighlights()
}

// Update handles msg, then lays the screen out once. View has a value
// receiver, so anything it computes is lost; the layout pass here is what
// keeps scroll positions, mouse regions, and the rendered main content.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	nm := next.(model)
	if nm.width > 0 && nm.height > 0 {
		_ = nm.render()
	}
	return nm, cmd
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.mainKey = ""
		if !m.selScroll {
			// The panels move. A selection of rows in main stays, unless
			// the new width wraps them again (see mainShape).
			m.textSel.Clear()
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case loadedMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		m.loading = false
		m.err = msg.err
		if msg.err != nil {
			return m, nil
		}
		first := !m.loaded
		m.loaded = true
		m.cache = msg.runs
		m.rebuild()
		m.mainKey = ""
		if first && m.opts.RunID != "" {
			if _, ok := m.cache[m.opts.RunID]; !ok {
				return m, m.setToast(fmt.Sprintf("run %q not found", m.opts.RunID), true)
			}
		}
		return m, nil

	case refreshMsg:
		next := tick(m.opts.Refresh, refreshMsg{})
		if m.loading {
			return m, next
		}
		return m, tea.Batch(m.startLoad(), next)

	case healthMsg:
		if m.opts.Server == nil {
			return m, nil
		}
		return m, checkServer(m.ctx, m.opts.Server)

	case serverMsg:
		m.online = msg.ok
		m.serverErr = msg.err
		if msg.ok {
			m.serverInfo = msg.info
		}
		return m, tick(5*time.Second, healthMsg{})

	case actionDoneMsg:
		m.busy = ""
		if msg.err != nil {
			return m, tea.Batch(m.setToast(actionError(msg.act, msg.err), true), m.startLoad())
		}
		text := actionPast(msg.act)
		if msg.run != nil && msg.run.State != "" {
			text += " · run is " + string(msg.run.State)
		}
		return m, tea.Batch(m.setToast(text, false), m.startLoad())

	case toastExpiredMsg:
		if msg.id == m.toastID {
			m.toast = ""
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.key(msg)

	case tea.MouseClickMsg:
		return m.press(msg.Mouse())

	case tea.MouseMotionMsg:
		return m.drag(msg.Mouse())

	case tea.MouseReleaseMsg:
		return m.release()

	case tea.MouseWheelMsg:
		if !m.selScroll {
			m.textSel.Clear()
		}
		return m.wheel(msg.Mouse())

	case tea.PasteMsg:
		return m.paste(msg)
	}
	return m, nil
}

// paste sends a bracketed paste to the text field that has the keys: a
// modal's input, the filter, or the search. Elsewhere a paste does nothing.
// A textarea that is not focused, or not set up, ignores it.
func (m model) paste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch {
	case m.modal != nil:
		m.modal.input, cmd = m.modal.input.Update(msg)
	case m.filtering:
		m.filter, cmd = m.filter.Update(msg)
		m.rebuild()
		m.mainKey = ""
	case m.searching:
		m.search, cmd = m.search.Update(msg)
	}
	return m, cmd
}

// key routes a key press. An open modal, filter, or search owns the keys;
// otherwise global keys apply, then the focused panel's.
func (m model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.textSel.Clear()
	k := msg.String()
	if m.modal != nil {
		cmd, submit, closed := m.modal.update(msg)
		if !submit {
			if closed {
				m.modal = nil
			}
			return m, cmd
		}
		md := m.modal
		m.modal = nil
		text, approved := md.answer()
		m.busy = string(md.act) + "…"
		return m, perform(m.ctx, m.opts.Server, md.act, md.runID, md.turnID, text, approved)
	}

	if m.filtering {
		switch k {
		case "esc":
			m.filtering = false
			m.filter.SetValue("")
			m.filter.Blur()
			m.rebuild()
			return m, nil
		case "enter", "down", "up":
			m.filtering = false
			m.filter.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.rebuild()
		m.mainKey = ""
		return m, cmd
	}

	if m.searching {
		switch k {
		case "esc":
			m.clearSearch()
			return m, nil
		case "enter":
			m.searching = false
			m.search.Blur()
			m.query = m.search.Value()
			m.mainKey = ""
			return m, nil
		}
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		return m, cmd
	}

	switch k {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "?":
		m.modal = &modal{kind: modalHelp, title: "Keys", body: helpText(m.width)}
		return m, nil
	case "tab":
		m.focus = (m.focus + 1) % 3
		return m, nil
	case "shift+tab":
		m.focus = (m.focus + 2) % 3
		return m, nil
	case "1":
		m.focus = focusRuns
		return m, nil
	case "2":
		m.focus = focusTurns
		return m, nil
	case "3", "0":
		m.focus = focusMain
		return m, nil
	case "]":
		m.tab = (m.tab + 1) % tabCount
		m.mainKey = ""
		return m, nil
	case "[":
		m.tab = (m.tab + tabCount - 1) % tabCount
		m.mainKey = ""
		return m, nil
	case "z", "+":
		m.zoom = !m.zoom
		m.mainKey = ""
		return m, nil
	case "ctrl+r":
		return m, m.startLoad()
	case "y":
		if r, ok := m.current(); ok {
			return m, tea.Batch(tea.SetClipboard(r.ID), m.setToast("copied run ID "+r.ID, false))
		}
	case "Y":
		if r, ok := m.current(); ok && r.TraceID() != "" {
			return m, tea.Batch(tea.SetClipboard(r.TraceID()), m.setToast("copied trace ID "+r.TraceID(), false))
		}
		return m, m.setToast("this run has no trace ID", true)
	case "a", "n", "c", "x", "C", "m":
		return m.startAction(k)
	case "f":
		m.stateFilt = (m.stateFilt + 1) % stateFilters
		m.rebuild()
		m.mainKey = ""
		return m, m.setToast("showing "+m.stateFilt.String()+" runs", false)
	case "s":
		m.sortKey = (m.sortKey + 1) % sortKeys
		m.rebuild()
		return m, m.setToast("sorted by "+m.sortKey.String(), false)
	case "/":
		if m.focus == focusMain && (m.tab == tabConversation || m.tab == tabOverview) {
			m.searching = true
			m.search.SetValue("")
			return m, m.search.Focus()
		}
		m.focus = focusRuns
		m.filtering = true
		return m, m.filter.Focus()
	case "esc":
		if m.query != "" {
			m.clearSearch()
			m.mainKey = ""
			return m, nil
		}
		if m.zoom {
			m.zoom = false
			return m, nil
		}
		if m.focus == focusMain {
			m.focus = focusRuns
		}
		return m, nil
	}

	switch m.focus {
	case focusRuns:
		return m.runsKey(k)
	case focusTurns:
		return m.turnsKey(k)
	default:
		return m.mainKeyPress(msg)
	}
}

func (m model) startAction(k string) (tea.Model, tea.Cmd) {
	acts := map[string]action{"a": actAnswer, "n": actSend, "c": actCancel, "x": actRetire, "C": actClear, "m": actCompact}
	act := acts[k]
	r, ok := m.current()
	if !ok {
		return m, nil
	}
	if m.opts.Server == nil || !m.online {
		why := "no server is reachable"
		if m.opts.ServerAddr != "" {
			why += " at " + m.opts.ServerAddr
		}
		return m, m.setToast(fmt.Sprintf("%s: %s — the console is read only", act, why), true)
	}
	if m.busy != "" {
		return m, m.setToast("wait for "+strings.TrimSuffix(m.busy, "…")+" to finish", true)
	}
	if ok, why := canAct(r, act); !ok {
		return m, m.setToast(string(act)+": "+why, true)
	}
	switch act {
	case actAnswer:
		m.modal = answerModal(r)
	case actSend:
		m.modal = sendModal(r)
	default:
		m.modal = confirmModal(r, act)
	}
	if m.modal.kind == modalText || (m.modal.note && m.modal.kind == modalConfirm) {
		return m, m.modal.input.Focus()
	}
	return m, nil
}

func (m model) runsKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "down", "j":
		m.selectRun(m.sel + 1)
	case "up", "k":
		m.selectRun(m.sel - 1)
	case "g", "home":
		m.selectRun(0)
	case "G", "end":
		m.selectRun(len(m.runs) - 1)
	case "pgdown", "ctrl+d":
		m.selectRun(m.sel + max(1, m.rRuns.h-2))
	case "pgup", "ctrl+u":
		m.selectRun(m.sel - max(1, m.rRuns.h-2))
	case "enter", "right", "l":
		m.focus = focusMain
	}
	return m, nil
}

func (m model) turnsKey(k string) (tea.Model, tea.Cmd) {
	n := len(m.turns())
	move := func(i int) {
		m.turnSel = min(max(i, 0), max(0, n-1))
		m.spanCursor, m.spanTop, m.detailTop = 0, 0, 0
		m.mainKey = ""
	}
	switch k {
	case "down", "j":
		move(m.turnSel + 1)
	case "up", "k":
		move(m.turnSel - 1)
	case "g", "home":
		move(0)
	case "G", "end":
		move(n - 1)
	case "enter", "right", "l":
		m.tab = tabTrace
		m.focus = focusMain
		m.mainKey = ""
	case "left", "h":
		m.focus = focusRuns
	}
	return m, nil
}

func (m model) mainKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch m.tab {
	case tabTrace:
		tr, ok := m.currentTrace()
		if !ok {
			break
		}
		switch k {
		case "down", "j":
			m.spanCursor = min(m.spanCursor+1, len(tr.Spans)-1)
			m.detailTop = 0
		case "up", "k":
			m.spanCursor = max(m.spanCursor-1, 0)
			m.detailTop = 0
		case "g", "home":
			m.spanCursor, m.detailTop = 0, 0
		case "G", "end":
			m.spanCursor, m.detailTop = len(tr.Spans)-1, 0
		case "J":
			m.detailTop++
		case "K":
			m.detailTop = max(0, m.detailTop-1)
		case "ctrl+d", "pgdown":
			m.detailTop += max(1, m.detailH-1)
		case "ctrl+u", "pgup":
			m.detailTop = max(0, m.detailTop-max(1, m.detailH-1))
		case "left", "h":
			m.focus = focusTurns
		}
		return m, nil
	case tabJournal:
		r, ok := m.current()
		if !ok {
			break
		}
		n := len(r.Records)
		switch k {
		case "down", "j":
			m.recCursor = min(m.recCursor+1, n-1)
			m.detailTop = 0
		case "up", "k":
			m.recCursor = max(m.recCursor-1, 0)
			m.detailTop = 0
		case "g", "home":
			m.recCursor, m.detailTop = 0, 0
		case "G", "end":
			m.recCursor, m.detailTop = n-1, 0
		case "pgdown", "ctrl+d":
			m.recCursor = min(m.recCursor+10, n-1)
		case "pgup", "ctrl+u":
			m.recCursor = max(m.recCursor-10, 0)
		case "enter", "space":
			m.recOpen = !m.recOpen
			m.detailTop = 0
		case "J":
			m.detailTop++
		case "K":
			m.detailTop = max(0, m.detailTop-1)
		case "left", "h":
			m.focus = focusRuns
		}
		return m, nil
	}

	switch k {
	case "left", "h":
		m.focus = focusRuns
		return m, nil
	case "g", "home":
		m.main.GotoTop()
		return m, nil
	case "G", "end":
		m.main.GotoBottom()
		return m, nil
	case "n":
		m.main.HighlightNext()
		return m, nil
	case "N":
		m.main.HighlightPrevious()
		return m, nil
	case "R":
		m.reasoning = !m.reasoning
		m.mainKey = ""
		return m, nil
	case "T":
		m.allResults = !m.allResults
		m.mainKey = ""
		return m, nil
	case "J":
		m.main.ScrollDown(1)
		return m, nil
	case "K":
		m.main.ScrollUp(1)
		return m, nil
	}
	var cmd tea.Cmd
	m.main, cmd = m.main.Update(msg)
	return m, cmd
}

func (m model) click(ms tea.Mouse) (tea.Model, tea.Cmd) {
	if m.modal != nil {
		return m, nil
	}
	x, y := ms.X, ms.Y
	switch {
	case m.rTabs.has(x, y):
		for i, tx := range slices.Backward(m.tabX) {
			if x >= tx {
				m.tab = tab(i)
				m.mainKey = ""
				break
			}
		}
		m.focus = focusMain
	case m.rRuns.has(x, y):
		m.focus = focusRuns
		if i := m.runsTop + y - m.rRuns.y - 1; y > m.rRuns.y && i < len(m.runs) {
			m.selectRun(i)
		}
	case m.rTurns.has(x, y):
		m.focus = focusTurns
		if i := m.turnTop + y - m.rTurns.y - 1; y > m.rTurns.y && i < len(m.turns()) {
			m.turnSel = i
			m.spanCursor, m.spanTop, m.detailTop = 0, 0, 0
			m.mainKey = ""
		}
	case m.rMain.has(x, y):
		m.focus = focusMain
		if m.tab == tabTrace {
			if i := m.spanTop + y - m.rMain.y - 4; i >= 0 && y > m.rMain.y+3 {
				if tr, ok := m.currentTrace(); ok && i < len(tr.Spans) {
					m.spanCursor, m.detailTop = i, 0
				}
			}
		}
		if m.tab == tabJournal {
			if i := m.recTop + y - m.rMain.y - 2; i >= 0 && y > m.rMain.y+1 {
				if r, ok := m.current(); ok && i < len(r.Records) {
					if i == m.recCursor {
						m.recOpen = !m.recOpen
					}
					m.recCursor, m.detailTop = i, 0
				}
			}
		}
	}
	return m, nil
}

// mainBody is the main panel without its border: the cells a selection may
// cover.
func (m model) mainBody() rect {
	r := m.rMain
	return rect{r.x + 1, r.y + 1, max(0, r.w-2), max(0, r.h-2)}
}

// mainScrolls reports whether the main panel shows main, the viewport: the
// overview and conversation tabs of a run.
func (m model) mainScrolls() bool {
	_, ok := m.current()
	return ok && (m.tab == tabOverview || m.tab == tabConversation)
}

// selPoint maps a screen cell in selArea to a point in the selection's
// space. y is clamped to selArea by the caller.
func (m model) selPoint(x, y int) selection.Point {
	if m.selScroll {
		a := m.selArea
		return selection.Point{Line: m.main.YOffset() + y - a.y, Col: x - a.x}
	}
	return selection.Point{Line: y, Col: x}
}

// press starts a selection when the left button goes down in the main
// panel's body. Anywhere else it is a click.
func (m model) press(ms tea.Mouse) (tea.Model, tea.Cmd) {
	m.textSel.Clear()
	body := m.mainBody()
	if m.modal != nil || ms.Button != tea.MouseLeft || !body.has(ms.X, ms.Y) {
		return m.click(ms)
	}
	m.selArea, m.pressAt = body, ms
	m.selScroll = m.mainScrolls()
	if m.selScroll {
		m.textSel = selection.Selection{Margin: m.mainRows.Margin}
	} else {
		m.textSel = selection.Selection{Left: body.x, Right: body.x + body.w}
	}
	m.textSel.Press(m.selPoint(ms.X, ms.Y))
	return m, nil
}

// drag extends the selection, held inside the panel it started in. On a tab
// that scrolls, a drag above or below the panel scrolls it one row for each
// motion event, so a selection can be longer than the panel.
func (m model) drag(ms tea.Mouse) (tea.Model, tea.Cmd) {
	if !m.textSel.Dragging() {
		return m, nil
	}
	a := m.selArea
	x := min(max(ms.X, a.x), a.x+a.w-1)
	y := min(max(ms.Y, a.y), a.y+a.h-1)
	if m.selScroll {
		switch {
		case ms.Y < a.y:
			m.main.ScrollUp(1)
		case ms.Y >= a.y+a.h:
			m.main.ScrollDown(1)
		}
	}
	m.textSel.Drag(m.selPoint(x, y))
	return m, nil
}

// release ends the drag and copies the selection. A press that never moved
// is the panel's click.
func (m model) release() (tea.Model, tea.Cmd) {
	if !m.textSel.Dragging() {
		return m, nil
	}
	m.textSel.Release()
	if m.textSel.Empty() {
		m.textSel.Clear()
		return m.click(m.pressAt)
	}
	var text string
	if m.selScroll {
		text = m.textSel.Text(m.mainRows.Plain, m.mainRows.Joined)
	} else {
		text = m.textSel.Text(strings.Split(ansi.Strip(m.render()), "\n"), nil)
	}
	if text == "" {
		m.textSel.Clear()
		return m, nil
	}
	return m, tea.Batch(selection.Copy(text), m.setToast(fmt.Sprintf("copied %d characters", utf8.RuneCountInString(text)), false))
}

// highlight draws the selection over the frame. A selection of main's rows
// is drawn only on the rows and cells of the panel's body: the frame around
// it holds other text.
func (m model) highlight(frame string) string {
	lines := strings.Split(frame, "\n")
	if !m.selScroll {
		return strings.Join(m.textSel.Highlight(lines, 0, sSelection), "\n")
	}
	a := m.selArea
	if a != m.mainBody() || !m.mainScrolls() {
		return frame
	}
	top, bottom := a.y, min(len(lines), a.y+a.h)
	if m.searching {
		bottom-- // the search field is on the last row
	}
	if top >= bottom {
		return frame
	}
	cells := make([]string, 0, bottom-top)
	for _, l := range lines[top:bottom] {
		cells = append(cells, ansi.Cut(l, a.x, a.x+a.w))
	}
	cells = m.textSel.Highlight(cells, m.main.YOffset(), sSelection)
	for i, c := range cells {
		l := lines[top+i]
		lines[top+i] = ansi.Cut(l, 0, a.x) + c + ansi.Cut(l, a.x+a.w, ansi.StringWidth(l))
	}
	return strings.Join(lines, "\n")
}

func (m model) wheel(ms tea.Mouse) (tea.Model, tea.Cmd) {
	if m.modal != nil {
		return m, nil
	}
	step := 1
	if ms.Button == tea.MouseWheelUp {
		step = -1
	}
	x, y := ms.X, ms.Y
	switch {
	case m.rRuns.has(x, y):
		m.selectRun(m.sel + step)
	case m.rTurns.has(x, y):
		m.turnSel = min(max(m.turnSel+step, 0), max(0, len(m.turns())-1))
		m.mainKey = ""
	case m.rMain.has(x, y):
		switch {
		case m.detailY > 0 && y >= m.detailY && (m.tab == tabTrace || m.tab == tabJournal):
			// The view clamps detailTop to the end of the detail.
			m.detailTop = max(0, m.detailTop+3*step)
		case m.tab == tabTrace:
			if tr, ok := m.currentTrace(); ok {
				m.spanCursor = min(max(m.spanCursor+step, 0), len(tr.Spans)-1)
			}
		case m.tab == tabJournal:
			if r, ok := m.current(); ok {
				m.recCursor = min(max(m.recCursor+step, 0), len(r.Records)-1)
			}
		default:
			if step > 0 {
				m.main.ScrollDown(3)
			} else {
				m.main.ScrollUp(3)
			}
		}
	}
	return m, nil
}

func (m model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "bonnie runs"
	if m.width <= 0 || m.height <= 0 {
		v.SetContent("loading journal…")
		return v
	}
	base := m.render()
	if m.modal == nil {
		if !m.textSel.Empty() {
			base = m.highlight(base)
		}
		v.SetContent(base)
		return v
	}
	box := m.modal.view(m.width)
	bw, bh := lipgloss.Width(box), lipgloss.Height(box)
	layer := lipgloss.NewLayer(box).X(max(0, (m.width-bw)/2)).Y(max(0, (m.height-bh)/3)).Z(1)
	v.SetContent(lipgloss.NewCompositor(lipgloss.NewLayer(dim(base)), layer).Render())
	return v
}

// dim fades the screen behind a modal, so the dialog reads as on top.
func dim(s string) string {
	return sFaint.Render(ansi.Strip(s))
}

// render lays out and draws the screen, recording scroll positions and
// mouse regions on m as it goes.
func (m *model) render() string {
	m.layout()
	statusH := 3
	w, h := m.width, m.height
	bodyH := h - 1

	footer := m.footer()

	if m.zoom || w < 70 {
		if m.zoom || m.focus == focusMain {
			return lipgloss.JoinVertical(lipgloss.Left, m.mainPanel(w, bodyH), footer)
		}
	}

	leftW := min(max(32, w*2/7), 52)
	if w < 70 {
		leftW = w
	}
	turnsH := min(max(6, bodyH/3), 14)
	runsH := bodyH - statusH - turnsH
	left := lipgloss.JoinVertical(lipgloss.Left,
		m.statusPanel(leftW, statusH),
		m.runsPanel(leftW, runsH),
		m.turnsPanel(leftW, turnsH),
	)
	if w < 70 {
		return lipgloss.JoinVertical(lipgloss.Left, left, footer)
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.JoinHorizontal(lipgloss.Top, left, m.mainPanel(w-leftW, bodyH)),
		footer)
}

// layout records the panel regions for the mouse. It mirrors render.
func (m *model) layout() {
	w, h := m.width, m.height
	bodyH := h - 1
	statusH := 3
	m.rRuns, m.rTurns, m.rMain, m.rTabs = rect{}, rect{}, rect{}, rect{}
	if m.zoom || (w < 70 && m.focus == focusMain) {
		m.rMain = rect{0, 0, w, bodyH}
		m.rTabs = rect{0, 0, w, 1}
		return
	}
	leftW := min(max(32, w*2/7), 52)
	if w < 70 {
		leftW = w
	}
	turnsH := min(max(6, bodyH/3), 14)
	runsH := bodyH - statusH - turnsH
	m.rRuns = rect{0, statusH, leftW, runsH}
	m.rTurns = rect{0, statusH + runsH, leftW, turnsH}
	if w >= 70 {
		m.rMain = rect{leftW, 0, w - leftW, bodyH}
		m.rTabs = rect{leftW, 0, w - leftW, 1}
	}
}

func (m *model) statusPanel(w, h int) string {
	inner := w - 2
	server := sMuted.Render("○ read only")
	switch {
	case m.opts.Server == nil:
	case m.online:
		name := m.serverInfo.Agent
		if name == "" {
			name = "server"
		}
		server = sGreen.Render("● ") + sText.Render(oneLine(name+" "+m.serverInfo.Version, inner/2))
	case m.serverErr != nil:
		server = sRed.Render("○ offline")
	default:
		server = sMuted.Render("◌ connecting")
	}
	counts := map[runtime.RunState]int{}
	for _, r := range m.cache {
		counts[r.State]++
	}
	// Glyph and count per state that may need an operator, so the line
	// stays short beside the server status.
	var summary strings.Builder
	summary.WriteString(sMuted.Render(fmt.Sprintf("%d runs", len(m.cache))))
	for _, s := range []runtime.RunState{runtime.RunRunning, runtime.RunWaiting, runtime.RunFailed} {
		if counts[s] > 0 {
			summary.WriteString("  " + stateGlyph(s, "●") + lipgloss.NewStyle().Foreground(stateColor(s)).Render(fmt.Sprintf(" %d", counts[s])))
		}
	}
	title := sAccent.Bold(true).Render("BONNIE") + sMuted.Render(" runs")
	body := row(summary.String(), server, inner)
	return panel(title, tailPath(m.opts.JournalPath, inner-4), body, w, h, false)
}

func (m *model) runsPanel(w, h int) string {
	inner, rows := w-2, h-2
	focused := m.focus == focusRuns
	title := "[1] Runs"
	if m.stateFilt != FilterAll {
		title += " · " + m.stateFilt.String()
	}
	var lines []string
	if m.filtering || m.filter.Value() != "" {
		m.filter.SetWidth(inner - 3)
		lines = append(lines, m.filter.View())
		rows--
	}
	switch {
	case !m.loaded && m.loading:
		lines = append(lines, m.spin.View()+sMuted.Render(" reading journal…"))
	case m.err != nil:
		lines = append(lines, sRed.Render(oneLine("error: "+m.err.Error(), inner)))
	case len(m.runs) == 0:
		if len(m.cache) == 0 {
			lines = append(lines, sMuted.Render("No runs in this journal yet."))
		} else {
			lines = append(lines, sMuted.Render("No runs match. esc clears the filter, f cycles states."))
		}
	}
	m.runsTop = scrollTo(m.sel, m.runsTop, rows, len(m.runs))
	now := m.now()
	for i := m.runsTop; i < len(m.runs) && i < m.runsTop+rows; i++ {
		l := runRow(m.runs[i], inner, m.spin.View(), now)
		if i == m.sel {
			l = highlight(l, inner, focused)
		}
		lines = append(lines, l)
	}
	footer := ""
	if len(m.runs) > 0 {
		footer = fmt.Sprintf("%d of %d", m.sel+1, len(m.runs))
	}
	if m.sortKey != SortUpdated {
		footer = "by " + m.sortKey.String() + " · " + footer
	}
	return panel(title, footer, strings.Join(lines, "\n"), w, h, focused)
}

func (m *model) turnsPanel(w, h int) string {
	inner, rows := w-2, h-2
	focused := m.focus == focusTurns
	ts := m.turns()
	m.turnTop = scrollTo(m.turnSel, m.turnTop, rows, len(ts))
	var lines []string
	now := m.now()
	for i := m.turnTop; i < len(ts) && i < m.turnTop+rows; i++ {
		l := turnRow(ts[i], inner, m.spin.View(), now)
		if i == m.turnSel {
			l = highlight(l, inner, focused)
		}
		lines = append(lines, l)
	}
	if len(ts) == 0 && len(m.runs) > 0 {
		lines = append(lines, sMuted.Render("No turns yet."))
	}
	footer := ""
	if len(ts) > 0 {
		footer = fmt.Sprintf("%d of %d", m.turnSel+1, len(ts))
	}
	return panel("[2] Turns", footer, strings.Join(lines, "\n"), w, h, focused)
}

// tailPath shortens a path from the left, where it matters least.
func tailPath(p string, n int) string {
	if n <= 1 || ansi.StringWidth(p) <= n {
		return p
	}
	r := []rune(p)
	return "…" + string(r[max(0, len(r)-n+1):])
}

// scrollTo keeps sel inside a window of rows that starts at top.
func scrollTo(sel, top, rows, n int) int {
	if rows <= 0 {
		return 0
	}
	if sel < top {
		top = sel
	}
	if sel >= top+rows {
		top = sel - rows + 1
	}
	return max(0, min(top, max(0, n-rows)))
}

func (m *model) tabsTitle(x0 int) string {
	var parts []string
	m.tabX = m.tabX[:0]
	x := x0 + 3 // "╭─ "
	for t := range tabCount {
		label := t.String()
		m.tabX = append(m.tabX, x)
		if t == m.tab {
			parts = append(parts, sAccent.Bold(true).Underline(true).Render(label))
		} else {
			parts = append(parts, sMuted.Render(label))
		}
		x += ansi.StringWidth(label) + 3
	}
	return strings.Join(parts, sFaint.Render(" │ "))
}

func (m *model) mainPanel(w, h int) string {
	focused := m.focus == focusMain
	m.detailY = 0 // the trace and journal views set it when they show a detail
	inner, rows := w-2, h-2
	r, ok := m.current()
	title := m.tabsTitle(m.rMain.x)
	if !ok {
		msg := sMuted.Render("Select a run.")
		if m.loading && !m.loaded {
			msg = m.spin.View() + sMuted.Render(" reading journal…")
		}
		return panel(title, "", msg, w, h, focused)
	}

	switch m.tab {
	case tabTrace:
		return panel(title, m.traceFooter(), m.traceView(r, inner, rows, focused), w, h, focused)
	case tabJournal:
		return panel(title, fmt.Sprintf("record %d of %d", min(m.recCursor+1, len(r.Records)), len(r.Records)), m.journalView(r, inner, rows, focused), w, h, focused)
	}

	key := fmt.Sprintf("%s|%d|%d|%d|%v|%v|%s|%s", r.ID, len(r.Records), m.tab, inner, m.reasoning, m.allResults, m.query, r.State)
	if m.tab == tabOverview {
		key += "|" + ago(r.Updated, m.now())
	}
	m.main.SetWidth(inner)
	m.main.SetHeight(rows)
	if shape := fmt.Sprintf("%s|%d|%d|%v|%v", r.ID, m.tab, inner, m.reasoning, m.allResults); shape != m.mainShape {
		m.mainShape = shape
		if m.selScroll {
			m.textSel.Clear()
		}
	}
	if key != m.mainKey {
		// A drag holds the text still: following new output would move the
		// rows under the pointer.
		dragging := m.selScroll && m.textSel.Dragging()
		follow := m.main.AtBottom() && m.mainKey != "" && m.tab == tabConversation && !dragging
		if m.tab == tabOverview {
			m.mainRows = selection.Lines(overview(r, inner, m.now()))
		} else {
			lines := 6
			if m.allResults {
				lines = 0
			}
			m.mainRows = conversation(r, inner, conversationOptions{reasoning: m.reasoning, toolLines: lines})
		}
		if m.selScroll {
			m.textSel.Margin = m.mainRows.Margin
		}
		content := strings.Join(m.mainRows.Rows, "\n")
		off := m.main.YOffset()
		m.main.SetContent(content)
		m.main.ClearHighlights()
		if m.query != "" {
			m.main.SetHighlights(findAll(ansi.Strip(content), m.query))
		} else {
			m.main.SetYOffset(off)
		}
		if follow || (m.mainKey == "" && m.tab == tabConversation && off == 0 && r.State == runtime.RunRunning) {
			m.main.GotoBottom()
		}
		m.mainKey = key
	}
	footer := fmt.Sprintf("%d%%", int(m.main.ScrollPercent()*100))
	if m.query != "" {
		footer = "/" + m.query + " · n/N · " + footer
	}
	body := m.main.View()
	if m.searching {
		m.search.SetWidth(inner - 10)
		lines := strings.Split(body, "\n")
		if len(lines) > 0 {
			lines[len(lines)-1] = m.search.View()
		}
		body = strings.Join(lines, "\n")
	}
	return panel(title, footer, body, w, h, focused)
}

// findAll returns the byte ranges of every case-insensitive match.
func findAll(content, q string) [][]int {
	if q == "" {
		return nil
	}
	lc, lq := strings.ToLower(content), strings.ToLower(q)
	var out [][]int
	for i := 0; ; {
		j := strings.Index(lc[i:], lq)
		if j < 0 {
			return out
		}
		out = append(out, []int{i + j, i + j + len(lq)})
		i += j + len(lq)
	}
}

func (m *model) traceFooter() string {
	tr, ok := m.currentTrace()
	if !ok {
		return ""
	}
	f := fmt.Sprintf("turn %d · %d spans · %s", tr.Turn.Index, len(tr.Spans), dur(tr.Duration()))
	if tr.TraceID != "" {
		f += " · trace " + tr.TraceID[:12] + "…"
	}
	return f
}

// bodyRows is the number of screen rows the lines in out take: one line
// may hold more than one row.
func bodyRows(out []string) int {
	n := 0
	for _, l := range out {
		n += strings.Count(l, "\n") + 1
	}
	return n
}

func (m *model) traceView(r Run, w, h int, focused bool) string {
	tr, ok := m.currentTrace()
	if !ok {
		return sMuted.Render("This run has no turns yet.")
	}
	m.spanCursor = min(max(m.spanCursor, 0), len(tr.Spans)-1)
	listH := max(3, min(len(tr.Spans)+2, h*3/5))
	detailH := h - listH - 1

	head := sAccent.Bold(true).Render(fmt.Sprintf("Turn %d", tr.Turn.Index)) + "  " + stateBadge(tr.Turn.State) + "  " + sMuted.Render(tr.Start.Local().Format("Jan 02 15:04:05"))
	if tr.TraceID != "" {
		head += "  " + sFaint.Render("trace ") + sCyan.Render(tr.TraceID)
	}
	header, lines := waterfall(tr, w, m.spanCursor, focused)
	rows := listH - 2
	m.spanTop = scrollTo(m.spanCursor, m.spanTop, rows, len(lines))
	out := []string{oneLine(head, w), header}
	out = append(out, lines[m.spanTop:min(len(lines), m.spanTop+rows)]...)
	for len(out) < listH {
		out = append(out, "")
	}
	out = append(out, sFaint.Render(strings.Repeat("─", w)))
	m.detailY, m.detailH = m.rMain.y+1+bodyRows(out), detailH
	detail := strings.Split(spanDetail(tr.Spans[m.spanCursor], tr, w), "\n")
	m.detailTop = min(m.detailTop, max(0, len(detail)-detailH))
	out = append(out, detail[m.detailTop:min(len(detail), m.detailTop+detailH)]...)
	return strings.Join(out, "\n")
}

func (m *model) journalView(r Run, w, h int, focused bool) string {
	if len(r.Records) == 0 {
		return sMuted.Render("No records.")
	}
	m.recCursor = min(max(m.recCursor, 0), len(r.Records)-1)
	listH := h
	if m.recOpen {
		listH = max(4, h/2)
	}
	header := sMuted.Render(fmt.Sprintf("%5s %-12s %7s %-14s %s", "seq", "time", "Δ", "kind", "detail"))
	rows := listH - 1
	m.recTop = scrollTo(m.recCursor, m.recTop, rows, len(r.Records))
	out := []string{header}
	for i := m.recTop; i < len(r.Records) && i < m.recTop+rows; i++ {
		var prev time.Time
		if i > 0 {
			prev = r.Records[i-1].Timestamp
		}
		l := journalLine(r.Records[i], prev, w)
		if i == m.recCursor {
			l = highlight(l, w, focused)
		}
		out = append(out, l)
	}
	if m.recOpen {
		for len(out) < listH {
			out = append(out, "")
		}
		out = append(out, sFaint.Render(strings.Repeat("─", w)))
		detail := strings.Split(recordBody(r.Records[m.recCursor], w), "\n")
		dh := h - listH - 1
		m.detailY, m.detailH = m.rMain.y+1+bodyRows(out), dh
		m.detailTop = min(m.detailTop, max(0, len(detail)-dh))
		out = append(out, detail[m.detailTop:min(len(detail), m.detailTop+dh)]...)
	}
	return strings.Join(out, "\n")
}

func (m *model) footer() string {
	w := m.width
	if m.toast != "" {
		st := sGreen
		mark := "✓ "
		if m.toastErr {
			st, mark = sRed, "✗ "
		}
		return fit(st.Render(mark+m.toast), w)
	}
	if m.busy != "" {
		return fit(m.spin.View()+" "+sText.Render(m.busy), w)
	}
	type hint struct{ k, d string }
	var hints []hint
	switch {
	case m.filtering:
		hints = []hint{{"enter", "keep"}, {"esc", "clear"}}
	case m.searching:
		hints = []hint{{"enter", "search"}, {"esc", "cancel"}}
	case m.focus == focusRuns:
		hints = []hint{{"↑↓", "select"}, {"enter", "open"}, {"/", "filter"}, {"f", "state"}, {"s", "sort"}}
	case m.focus == focusTurns:
		hints = []hint{{"↑↓", "select"}, {"enter", "trace"}}
	case m.tab == tabTrace:
		hints = []hint{{"↑↓", "span"}, {"J/K", "scroll detail"}, {"[ ]", "tab"}, {"z", "zoom"}}
	case m.tab == tabJournal:
		hints = []hint{{"↑↓", "record"}, {"enter", "payload"}, {"J/K", "scroll payload"}, {"[ ]", "tab"}, {"z", "zoom"}}
	default:
		hints = []hint{{"↑↓", "scroll"}, {"/", "search"}, {"R", "reasoning"}, {"T", "full results"}, {"[ ]", "tab"}}
	}
	if !m.filtering && !m.searching {
		if r, ok := m.current(); ok && m.online {
			if r.State == runtime.RunWaiting {
				hints = append(hints, hint{"a", "answer"})
			}
			if r.Active() {
				hints = append(hints, hint{"c", "cancel"})
			} else if r.State != runtime.RunRetired {
				hints = append(hints, hint{"n", "send"})
			}
		}
		hints = append(hints, hint{"?", "keys"}, hint{"q", "quit"})
	}
	var parts []string
	for _, h := range hints {
		parts = append(parts, sKey.Render(h.k)+" "+sMuted.Render(h.d))
	}
	left := strings.Join(parts, sFaint.Render(" · "))
	right := ""
	if m.loading && m.loaded {
		right = sFaint.Render("↻")
	}
	return row(left, right, w)
}

// helpText is the key reference in the help modal.
func helpText(w int) string {
	sections := []struct {
		title string
		keys  [][2]string
	}{
		{"Navigate", [][2]string{
			{"tab / shift+tab", "next / previous panel"},
			{"1 2 3", "runs · turns · main"},
			{"↑↓ j k", "move"},
			{"g G", "first · last"},
			{"[ ]", "previous · next tab"},
			{"z", "zoom the main panel"},
			{"mouse", "click · wheel · drag to copy"},
		}},
		{"Runs", [][2]string{
			{"/", "filter by text"},
			{"f", "cycle state filter"},
			{"s", "cycle sort"},
			{"y Y", "copy run ID · trace ID"},
			{"ctrl+r", "refresh now"},
		}},
		{"Views", [][2]string{
			{"/ n N", "search · next · previous"},
			{"R", "show reasoning in full"},
			{"T", "show tool results in full"},
			{"enter", "open a journal record"},
			{"J K", "scroll the detail pane"},
		}},
		{"Actions (need a server)", [][2]string{
			{"a", "answer a waiting run"},
			{"n", "send a message"},
			{"c", "cancel the turn"},
			{"x", "retire the run"},
			{"C", "clear context"},
			{"m", "compact context"},
		}},
	}
	var cols []string
	for _, s := range sections {
		var b strings.Builder
		b.WriteString(sAccent.Bold(true).Render(s.title) + "\n")
		for _, k := range s.keys {
			b.WriteString(sKey.Render(fmt.Sprintf("%-16s", k[0])) + sText.Render(k[1]) + "\n")
		}
		cols = append(cols, b.String())
	}
	var out string
	if w >= 110 {
		out = lipgloss.JoinHorizontal(lipgloss.Top, lipgloss.NewStyle().Width(46).Render(cols[0]+"\n"+cols[1]), lipgloss.NewStyle().Width(46).Render(cols[2]+"\n"+cols[3]))
	} else {
		out = strings.Join(cols, "\n")
	}
	return out + "\n" + sMuted.Render("Everything here reads the journal; actions go to the server over HTTP.") + "\n" + sMuted.Render("press any key to close")
}
