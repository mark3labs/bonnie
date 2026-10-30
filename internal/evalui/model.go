package evalui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mark3labs/bonnie/internal/runsui"
	"github.com/mark3labs/bonnie/internal/selection"
	"github.com/mark3labs/bonnie/runtime"
)

// RerunFunc starts a rerun of evals after prev, as a new session, and
// returns it at once: go test runs in the background. The new session must
// be seeded with prev (see [Session.Seed]), so the screen shows the whole
// suite, with the rerun evals running from the start.
type RerunFunc func(prev *Session, evals []Result) (*Session, error)

// ExploreOptions configures [Explore].
type ExploreOptions struct {
	// Selected names the eval to select first, or "".
	Selected string
	// Rerun starts a rerun when the user presses r or R. Nil turns the
	// keys off.
	Rerun RerunFunc
}

// Action is what the user did, when the screen closed.
type Action struct {
	// Stopped is true when the user quit before Session finished, so the
	// caller must stop its go test.
	Stopped bool
	// Session is the session the screen showed last: the one it started
	// with, or the last rerun.
	Session *Session
}

// Explore shows the session until the user quits. The session can still be
// running: the screen follows it as go test reports. A rerun replaces the
// session in place, in the same screen. From an eval with a kept journal,
// the user can open the run console (internal/runsui) and come back.
func Explore(ctx context.Context, s *Session, opts ExploreOptions) (Action, error) {
	selected := opts.Selected
	for {
		out, err := explore(ctx, s, selected, opts.Rerun)
		if err != nil {
			return Action{Session: s}, err
		}
		if out.session != nil {
			s = out.session
		}
		if out.open == "" {
			return Action{Stopped: out.stopped, Session: s}, nil
		}
		selected = out.selected
		if err := openConsole(ctx, out.open); err != nil {
			return Action{Session: s}, err
		}
	}
}

// outcome is why the screen closed.
type outcome struct {
	// open is a journal directory to show in the run console.
	open     string
	selected string
	stopped  bool
	// session is the session shown last.
	session *Session
}

func explore(ctx context.Context, s *Session, selected string, rerun RerunFunc) (outcome, error) {
	m := newModel(ctx, s, selected)
	m.rerun = rerun
	final, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return outcome{}, fmt.Errorf("bonnie: eval screen: %w", err)
	}
	if fm, ok := final.(model); ok {
		fm.out.session = fm.session
		return fm.out, nil
	}
	return outcome{}, nil
}

func openConsole(ctx context.Context, dir string) error {
	j, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		return fmt.Errorf("bonnie: open eval journal: %w", err)
	}
	defer func() { _ = j.Close() }()
	return runsui.Open(ctx, runsui.Options{Journal: j, JournalPath: dir, RunID: RunID})
}

type tab int

const (
	tabScores tab = iota
	tabConversation
	tabOutput
	tabCount
)

func (t tab) String() string {
	return [...]string{"Scores", "Conversation", "Output"}[t]
}

type focusArea int

const (
	focusList focusArea = iota
	focusDetail
)

// item is one row of the list: the build output, or one eval.
type item struct {
	build bool
	eval  int // index into snap.Evals
}

type changedMsg struct{}

// rerunMsg carries the session of a rerun that started, or why it did not.
type rerunMsg struct {
	session *Session
	err     error
}
type tickMsg struct{}
type conversationMsg struct {
	dir     string
	entries []entry
	models  []string
	err     error
}

type conversationState struct {
	entries []entry
	// models served the agent's steps, in the order of first use.
	models  []string
	err     error
	loading bool
}

type model struct {
	ctx     context.Context
	session *Session
	snap    Snapshot
	now     func() time.Time

	items      []item
	sel        int
	selName    string
	top        int
	failedOnly bool

	focus     focusArea
	tab       tab
	detailTop int
	detailH   int

	convs map[string]*conversationState
	spin  spinner.Model
	// rerun starts a rerun, or is nil. starting is true while it starts.
	rerun    RerunFunc
	starting bool
	// notice is a message for the footer, shown until the next key. It is
	// an error unless noticeOK is set.
	notice   string
	noticeOK bool
	// confirmStop is true after one q while evals run.
	confirmStop bool
	out         outcome

	width, height int

	// textSel is a mouse selection in the detail text. Its lines are lines
	// of detailLines, so it stays on its text while the detail scrolls;
	// a key, a wheel, or another eval drops it.
	textSel selection.Selection
	// pressAt is where the left button went down in the detail text. A
	// press that does not drag is a click, run on release.
	pressAt tea.Mouse
}

// rect is a screen region, for mouse hit tests.
type rect struct{ x, y, w, h int }

func (r rect) has(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

func newModel(ctx context.Context, s *Session, selected string) model {
	m := model{
		ctx: ctx, session: s, now: time.Now, selName: selected,
		convs: map[string]*conversationState{},
		spin:  spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(sCyan)),
	}
	m.refresh()
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.waitChange(), tick(), m.loadConversation())
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

// waitChange waits for the next change of a live session.
func (m model) waitChange() tea.Cmd {
	if m.snap.Done() {
		return nil
	}
	ch, ctx := m.session.Changed(), m.ctx
	return func() tea.Msg {
		select {
		case <-ch:
			return changedMsg{}
		case <-ctx.Done():
			return nil
		}
	}
}

// refresh takes a new snapshot and keeps the selection on the same eval.
func (m *model) refresh() {
	m.snap = m.session.Snapshot()
	m.items = m.items[:0]
	if len(m.snap.Build) > 0 {
		m.items = append(m.items, item{build: true})
	}
	for i, r := range m.snap.Evals {
		if m.failedOnly && r.Status != StatusFail {
			continue
		}
		m.items = append(m.items, item{eval: i})
	}
	m.sel = 0
	for i, it := range m.items {
		if !it.build && m.snap.Evals[it.eval].Name == m.selName {
			m.sel = i
			break
		}
	}
	m.selName = m.selectedName()
}

func (m model) selected() (Result, bool) {
	if m.sel < 0 || m.sel >= len(m.items) || m.items[m.sel].build {
		return Result{}, false
	}
	return m.snap.Evals[m.items[m.sel].eval], true
}

func (m model) selectedName() string {
	if r, ok := m.selected(); ok {
		return r.Name
	}
	return ""
}

func (m *model) move(step int) {
	if len(m.items) == 0 {
		return
	}
	m.sel = min(max(0, m.sel+step), len(m.items)-1)
	m.selName = m.selectedName()
	m.detailTop = 0
}

// loadConversation reads the selected eval's journal once it has ended.
func (m model) loadConversation() tea.Cmd {
	r, ok := m.selected()
	if !ok || r.Journal == "" || r.Status == StatusRunning {
		return nil
	}
	if st := m.convs[r.Journal]; st != nil {
		return nil
	}
	m.convs[r.Journal] = &conversationState{loading: true}
	ctx, dir := m.ctx, r.Journal
	return func() tea.Msg {
		entries, models, err := loadConversation(ctx, dir)
		return conversationMsg{dir: dir, entries: entries, models: models, err: err}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clamp()
		return m, nil
	case changedMsg:
		m.refresh()
		if m.snap.Done() {
			m.confirmStop = false
		}
		return m, tea.Batch(m.waitChange(), m.loadConversation())
	case tickMsg:
		return m, tick()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case rerunMsg:
		m.starting = false
		if msg.err != nil {
			m.notice = msg.err.Error()
			return m, nil
		}
		m.session, m.detailTop = msg.session, 0
		m.refresh()
		m.clamp()
		return m, tea.Batch(m.waitChange(), m.loadConversation())
	case conversationMsg:
		m.convs[msg.dir] = &conversationState{entries: msg.entries, models: msg.models, err: msg.err}
		m.clamp()
		return m, nil
	case tea.KeyPressMsg:
		m.textSel.Clear()
		return m.key(msg)
	case tea.MouseClickMsg:
		return m.press(msg.Mouse())
	case tea.MouseMotionMsg:
		return m.drag(msg.Mouse())
	case tea.MouseReleaseMsg:
		return m.release()
	case tea.MouseWheelMsg:
		m.textSel.Clear()
		return m.wheel(msg.Mouse())
	}
	return m, nil
}

// layout returns the screen regions of the list and the detail panels. It
// mirrors render: a region that is not on the screen is empty.
func (m model) layout() (list, detail rect) {
	bodyH := max(3, m.height-2)
	if m.width < 80 {
		if m.focus == focusDetail {
			return rect{}, rect{0, 1, m.width, bodyH}
		}
		return rect{0, 1, m.width, bodyH}, rect{}
	}
	lw := m.listWidth()
	return rect{0, 1, lw, bodyH}, rect{lw, 1, m.width - lw, bodyH}
}

// detailBody is the region of the detail text: the detail panel without its
// border, the tab row, and the margin of one cell that each line has.
func (m model) detailBody() rect {
	_, d := m.layout()
	if d.w == 0 {
		return rect{}
	}
	return rect{d.x + 2, d.y + 2, max(0, d.w-4), max(0, d.h-3)}
}

// click selects the eval under the mouse, or the tab, and moves the focus to
// the panel it is in.
func (m model) click(ms tea.Mouse) (tea.Model, tea.Cmd) {
	list, detail := m.layout()
	x, y := ms.X, ms.Y
	switch {
	case list.has(x, y):
		m.focus = focusList
		rows := list.h - 2
		top := scrollTo(m.sel, m.top, rows, len(m.items))
		if i := top + y - list.y - 1; y > list.y && y < list.y+list.h-1 && i < len(m.items) && i != m.sel {
			m.sel, m.top, m.detailTop = i, top, 0
			m.selName = m.selectedName()
		}
	case detail.has(x, y):
		m.focus = focusDetail
		if y == detail.y+1 {
			if t, ok := tabAt(x - detail.x - 2); ok {
				m.tab, m.detailTop = t, 0
			}
		}
	default:
		return m, nil
	}
	m.clamp()
	return m, m.loadConversation()
}

// tabAt returns the tab whose label covers cell x of the tab row, counted
// from the first label. It mirrors detailPanel.
func tabAt(x int) (tab, bool) {
	at := 0
	for t := range tabCount {
		w := ansi.StringWidth(fmt.Sprintf("%d %s", t+1, t))
		if x >= at && x < at+w {
			return t, true
		}
		at += w + ansi.StringWidth("  │  ")
	}
	return 0, false
}

// selPoint maps a screen cell to a point of the detail text.
func (m model) selPoint(x, y int) selection.Point {
	b := m.detailBody()
	return selection.Point{Line: m.detailTop + y - b.y, Col: x - b.x}
}

// press starts a selection when the left button goes down on the detail
// text. Anywhere else it is a click.
func (m model) press(ms tea.Mouse) (tea.Model, tea.Cmd) {
	m.textSel.Clear()
	if ms.Button != tea.MouseLeft {
		return m, nil
	}
	b := m.detailBody()
	if !b.has(ms.X, ms.Y) {
		return m.click(ms)
	}
	m.pressAt = ms
	m.textSel.Press(m.selPoint(ms.X, ms.Y))
	return m, nil
}

// drag extends the selection, held inside the detail text. A drag above or
// below it scrolls one row for each motion event, so a selection can be
// longer than the panel.
func (m model) drag(ms tea.Mouse) (tea.Model, tea.Cmd) {
	if !m.textSel.Dragging() {
		return m, nil
	}
	b := m.detailBody()
	switch {
	case ms.Y < b.y:
		m.scroll(-1)
	case ms.Y >= b.y+b.h:
		m.scroll(1)
	}
	x := min(max(ms.X, b.x), b.x+b.w-1)
	y := min(max(ms.Y, b.y), b.y+b.h-1)
	m.textSel.Drag(m.selPoint(x, y))
	return m, nil
}

// release ends the drag and copies the selection. A press that never moved
// is a click.
func (m model) release() (tea.Model, tea.Cmd) {
	if !m.textSel.Dragging() {
		return m, nil
	}
	m.textSel.Release()
	if m.textSel.Empty() {
		m.textSel.Clear()
		return m.click(m.pressAt)
	}
	lines := m.detailLines(m.detailBody().w)
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = ansi.Strip(l)
	}
	text := m.textSel.Text(plain, nil)
	if text == "" {
		m.textSel.Clear()
		return m, nil
	}
	m.notice, m.noticeOK = fmt.Sprintf("copied %d characters", utf8.RuneCountInString(text)), true
	return m, selection.Copy(text)
}

// wheel scrolls the panel under the mouse: the list moves the selection,
// the detail scrolls its text.
func (m model) wheel(ms tea.Mouse) (tea.Model, tea.Cmd) {
	step := 1
	if ms.Button == tea.MouseWheelUp {
		step = -1
	}
	list, detail := m.layout()
	switch {
	case list.has(ms.X, ms.Y):
		m.move(step)
		m.clamp()
		return m, m.loadConversation()
	case detail.has(ms.X, ms.Y):
		m.scroll(3 * step)
	}
	return m, nil
}

// highlight draws the selection over the cells of the detail text.
func (m model) highlight(frame string) string {
	b := m.detailBody()
	lines := strings.Split(frame, "\n")
	top, bottom := b.y, min(len(lines), b.y+b.h)
	if b.w == 0 || top >= bottom {
		return frame
	}
	cells := make([]string, 0, bottom-top)
	for _, l := range lines[top:bottom] {
		cells = append(cells, ansi.Cut(l, b.x, b.x+b.w))
	}
	cells = m.textSel.Highlight(cells, m.detailTop, sSelection)
	for i, c := range cells {
		l := lines[top+i]
		lines[top+i] = ansi.Cut(l, 0, b.x) + c + ansi.Cut(l, b.x+b.w, ansi.StringWidth(l))
	}
	return strings.Join(lines, "\n")
}

func (m model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	m.notice, m.noticeOK = "", false
	if k != "q" {
		m.confirmStop = false
	}
	if m.starting && (k == "q" || k == "esc" || k == "ctrl+c") {
		// The rerun is about to replace the session. Quit after it has,
		// so the caller stops and reports the right run.
		m.notice = "The rerun is starting; quit in a moment."
		return m, nil
	}
	switch k {
	case "ctrl+c":
		m.out.stopped = !m.snap.Done()
		return m, tea.Quit
	case "q", "esc":
		if !m.snap.Done() && !m.confirmStop {
			m.confirmStop = true
			return m, nil
		}
		m.out.stopped = !m.snap.Done()
		return m, tea.Quit
	case "tab":
		m.focus = 1 - m.focus
	case "1", "2", "3":
		m.tab, m.detailTop = tab(k[0]-'1'), 0
	case "left", "h", "[":
		m.tab, m.detailTop = (m.tab+tabCount-1)%tabCount, 0
	case "right", "l", "]":
		m.tab, m.detailTop = (m.tab+1)%tabCount, 0
	case "f":
		m.failedOnly = !m.failedOnly
		m.refresh()
	case "o", "enter":
		if r, ok := m.selected(); ok && r.Journal != "" && r.Status != StatusRunning {
			m.out.open, m.out.selected = r.Journal, r.Name
			return m, tea.Quit
		}
	case "r":
		if r, ok := m.selected(); ok && m.canRerun() {
			return m.startRerun([]Result{r})
		}
	case "R":
		if failed := m.failed(); len(failed) > 0 && m.canRerun() {
			return m.startRerun(failed)
		}
	case "up", "k":
		if m.focus == focusList {
			m.move(-1)
		} else {
			m.scroll(-1)
		}
	case "down", "j":
		if m.focus == focusList {
			m.move(1)
		} else {
			m.scroll(1)
		}
	case "pgup":
		m.scroll(-max(1, m.detailH-2))
	case "pgdown", " ":
		m.scroll(max(1, m.detailH-2))
	case "home", "g":
		if m.focus == focusList {
			m.move(-len(m.items))
		} else {
			m.detailTop = 0
		}
	case "end", "G":
		if m.focus == focusList {
			m.move(len(m.items))
		} else {
			m.detailTop = 1 << 30
		}
	}
	m.clamp()
	return m, m.loadConversation()
}

func (m model) canRerun() bool {
	return m.rerun != nil && m.snap.Done() && !m.starting
}

// startRerun asks for a rerun of evals. The screen stays: the new session
// replaces the old one when it starts.
func (m model) startRerun(evals []Result) (tea.Model, tea.Cmd) {
	m.starting = true
	rerun, prev := m.rerun, m.session
	return m, func() tea.Msg {
		s, err := rerun(prev, evals)
		return rerunMsg{session: s, err: err}
	}
}

// failed returns the evals that failed.
func (m model) failed() []Result {
	var out []Result
	for _, r := range m.snap.Evals {
		if r.Status == StatusFail {
			out = append(out, r)
		}
	}
	return out
}

func (m *model) scroll(step int) {
	m.detailTop = max(0, m.detailTop+step)
	m.clamp()
}

// detailSize returns the width and the rows of the detail text. It mirrors
// render and detailPanel.
func (m model) detailSize() (w, rows int) {
	pw := m.width
	if m.width >= 80 {
		pw = m.width - m.listWidth()
	}
	return pw - 4, max(3, m.height-2) - 3
}

// clamp keeps the detail scroll inside its text. View draws a copy of the
// model, so the limit must be set here, not while drawing.
func (m *model) clamp() {
	w, rows := m.detailSize()
	m.detailH = rows
	m.detailTop = max(0, min(m.detailTop, len(m.detailLines(w))-rows))
}

func (m model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "bonnie eval"
	if m.width <= 0 || m.height <= 0 {
		v.SetContent("starting evals…")
		return v
	}
	frame := m.render()
	if !m.textSel.Empty() {
		frame = m.highlight(frame)
	}
	v.SetContent(frame)
	return v
}

func (m model) listWidth() int {
	if m.width < 80 {
		return m.width
	}
	return min(max(30, m.width/3), 56)
}

func (m *model) render() string {
	w, h := m.width, m.height
	header := m.header(w)
	footer := m.footer(w)
	bodyH := max(3, h-2)
	if w < 80 {
		if m.focus == focusDetail {
			return lipgloss.JoinVertical(lipgloss.Left, header, m.detailPanel(w, bodyH), footer)
		}
		return lipgloss.JoinVertical(lipgloss.Left, header, m.listPanel(w, bodyH), footer)
	}
	lw := m.listWidth()
	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		lipgloss.JoinHorizontal(lipgloss.Top, m.listPanel(lw, bodyH), m.detailPanel(w-lw, bodyH)),
		footer)
}

func (m model) header(w int) string {
	c := m.snap.Counts()
	title := sAccent.Bold(true).Render("BONNIE") + sMuted.Render(" eval")
	if m.snap.Tree != "" {
		title += sMuted.Render(" · ") + sText.Render(lastElem(m.snap.Tree))
	}
	title += sMuted.Render(" · " + m.snap.ID)
	if m.snap.From != "" {
		title += sMuted.Render(" · rerun of " + m.snap.From)
	}
	var parts []string
	switch {
	case !m.snap.Done():
		parts = append(parts, m.spin.View()+sCyan.Render(fmt.Sprintf(" %d running", c.Running)))
	case c.Failed > 0 || m.snap.Err != "":
		parts = append(parts, badge("FAIL", colorRed))
	case c.Evals == 0:
		parts = append(parts, badge("NO EVALS", colorYellow))
	default:
		parts = append(parts, badge("PASS", colorGreen))
	}
	parts = append(parts,
		sGreen.Render(fmt.Sprintf("✓ %d", c.Passed)),
		sRed.Render(fmt.Sprintf("✗ %d", c.Failed)))
	if c.Skipped > 0 {
		parts = append(parts, sMuted.Render(fmt.Sprintf("⊘ %d", c.Skipped)))
	}
	if n := c.Scores["passed"] + c.Scores["scored"] + c.Scores["failed"] + c.Scores["recorded"]; n > 0 {
		parts = append(parts, sMuted.Render("scores"),
			sGreen.Render(fmt.Sprintf("%d passed", c.Scores["passed"])),
			sYellow.Render(fmt.Sprintf("%d scored", c.Scores["scored"])),
			sRed.Render(fmt.Sprintf("%d failed", c.Scores["failed"])))
	}
	parts = append(parts, sMuted.Render(m.elapsed()))
	return row(" "+title, strings.Join(parts, "  ")+" ", w)
}

func (m model) elapsed() string {
	end := m.snap.Finished
	if end.IsZero() {
		end = m.now()
	}
	return seconds(end.Sub(m.snap.Started).Seconds())
}

func (m model) footer(w int) string {
	switch {
	case m.confirmStop:
		return fit(" "+sYellow.Render("Evals are still running. Press q again to stop them, or any other key to continue."), w)
	case m.notice != "" && m.noticeOK:
		return fit(" "+sGreen.Render(m.notice), w)
	case m.notice != "":
		return fit(" "+sRed.Render(m.notice), w)
	case m.starting:
		return fit(" "+m.spin.View()+sCyan.Render(" starting the rerun…"), w)
	}
	keys := []string{"↑↓ select", "tab focus", "1-3 tab", "f failed only", "q quit"}
	if m.snap.Done() && m.rerun != nil {
		if len(m.failed()) > 0 {
			keys = append([]string{"R rerun failed"}, keys...)
		}
		if _, ok := m.selected(); ok {
			keys = append([]string{"r rerun"}, keys...)
		}
	}
	if r, ok := m.selected(); ok && r.Journal != "" && r.Status != StatusRunning {
		keys = append([]string{"o open run console"}, keys...)
	}
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(sFaint.Render("  ·  "))
		}
		key, rest, _ := strings.Cut(k, " ")
		b.WriteString(sKey.Render(key) + " " + sMuted.Render(rest))
	}
	return fit(" "+b.String(), w)
}

func (m *model) listPanel(w, h int) string {
	inner, rows := w-2, h-2
	focused := m.focus == focusList
	m.top = scrollTo(m.sel, m.top, rows, len(m.items))
	var lines []string
	if len(m.items) == 0 {
		msg := m.spin.View() + sMuted.Render(" building the evals…")
		switch {
		case m.failedOnly:
			msg = sMuted.Render("no failed evals")
		case m.snap.Done():
			msg = sMuted.Render("no evals ran")
		}
		lines = append(lines, " "+msg)
	}
	for i := m.top; i < len(m.items) && i < m.top+rows; i++ {
		var line string
		if m.items[i].build {
			line = sYellow.Render(" ⚠ build output")
		} else {
			r := m.snap.Evals[m.items[i].eval]
			right := sMuted.Render(m.duration(r))
			if r.Carried() {
				// A result from an earlier run is muted, with that run's time.
				right = sFaint.Render(runTime(r.Run) + " " + m.duration(r))
			}
			line = row(" "+resultGlyph(r.Status, m.spin.View())+" "+evalName(r.Name), right+" ", inner)
		}
		if i == m.sel {
			line = highlight(line, inner, focused)
		}
		lines = append(lines, line)
	}
	title := "Evals"
	if m.failedOnly {
		title += " · failed"
	}
	return panel(title, fmt.Sprintf("%d of %d", min(m.sel+1, len(m.items)), len(m.items)), strings.Join(lines, "\n"), w, h, focused)
}

func (m model) duration(r Result) string {
	if r.Status == StatusRunning {
		if r.Started.IsZero() {
			return ""
		}
		return seconds(m.now().Sub(r.Started).Seconds())
	}
	return seconds(r.Elapsed)
}

// evalName shows a subtest under its parent with the parent muted.
func evalName(name string) string {
	i := strings.LastIndex(name, "/")
	if i < 0 {
		return sText.Render(name)
	}
	return sMuted.Render(name[:i+1]) + sText.Render(name[i+1:])
}

func (m *model) detailPanel(w, h int) string {
	inner := w - 4
	m.detailH = h - 3
	focused := m.focus == focusDetail
	var tabs []string
	for t := range tabCount {
		label := fmt.Sprintf("%d %s", t+1, t)
		if t == m.tab {
			tabs = append(tabs, sKey.Render(label))
		} else {
			tabs = append(tabs, sMuted.Render(label))
		}
	}
	lines := m.detailLines(inner)
	maxTop := max(0, len(lines)-m.detailH)
	m.detailTop = min(m.detailTop, maxTop)
	body := []string{" " + strings.Join(tabs, sFaint.Render("  │  "))}
	end := min(len(lines), m.detailTop+m.detailH)
	for _, l := range lines[m.detailTop:end] {
		body = append(body, " "+l)
	}
	footer := ""
	if maxTop > 0 {
		footer = fmt.Sprintf("%d/%d", m.detailTop+1, maxTop+1)
	}
	title := "Detail"
	if r, ok := m.selected(); ok {
		title = r.Name
	} else if m.sel < len(m.items) && len(m.items) > 0 && m.items[m.sel].build {
		title = "Build output"
	}
	return panel(title, footer, strings.Join(body, "\n"), w, h, focused)
}

func (m model) detailLines(w int) []string {
	if m.sel < len(m.items) && len(m.items) > 0 && m.items[m.sel].build {
		var out []string
		for _, l := range m.snap.Build {
			out = append(out, wrap(l, w, sText)...)
		}
		return out
	}
	r, ok := m.selected()
	if !ok {
		return []string{sMuted.Render("Select an eval.")}
	}
	switch m.tab {
	case tabConversation:
		return m.conversationLines(r, w)
	case tabOutput:
		var out []string
		for _, o := range r.Output {
			out = append(out, wrap(strings.TrimRight(o, "\n"), w, sText)...)
		}
		if len(out) == 0 {
			out = []string{sMuted.Render("No output.")}
		}
		return out
	}
	return m.scoreLines(r, w)
}

func (m model) scoreLines(r Result, w int) []string {
	status := map[string]string{StatusPass: "PASS", StatusFail: "FAIL", StatusSkip: "SKIP", StatusRunning: "RUNNING"}[r.Status]
	head := badge(status, resultColor(r.Status)) + "  " + sMuted.Render(m.duration(r))
	switch {
	case r.Turns == 1:
		head += sMuted.Render("  ·  1 turn")
	case r.Turns > 1:
		head += sMuted.Render(fmt.Sprintf("  ·  %d turns", r.Turns))
	}
	out := []string{"", head}
	out = append(out, m.modelLines(r, w)...)
	if r.Carried() {
		out = append(out, sMuted.Render("From run "+r.Run+": this rerun did not run it. Press r to run it again."))
	}
	out = append(out, "")
	if len(r.Reports) == 0 {
		if r.Status == StatusRunning {
			out = append(out, sMuted.Render("The eval is running. Scores show when it ends."))
		} else {
			out = append(out, sMuted.Render("No scores: this eval has gates only."))
		}
	} else {
		labelW := max(10, w-48)
		out = append(out, sMuted.Render(fmt.Sprintf("%-8s %-*s %6s  %-12s %-9s %s", "KIND", labelW, "LABEL", "SCORE", "", "BOUND", "STATUS")))
		for _, rep := range r.Reports {
			st := lipgloss.NewStyle().Foreground(scoreColor(rep.Status))
			gate := ""
			if rep.Gate {
				gate = sMuted.Render(" gate")
			}
			out = append(out, fmt.Sprintf("%s %s %s  %s %s %s%s",
				sCyan.Render(fmt.Sprintf("%-8s", rep.Kind)),
				sText.Render(fit(ansi.Strip(oneLine(rep.Label, labelW)), labelW)),
				st.Render(fmt.Sprintf("%6.3f", rep.Value)),
				bar(rep, 12),
				sMuted.Render(fmt.Sprintf("%-9s", Bounds(rep))),
				st.Render(rep.Status), gate))
			if rep.Error != "" {
				out = append(out, wrap("  ✗ "+rep.Error, w, sRed)...)
			} else if rep.Reason != "" {
				out = append(out, wrap("  ↳ "+rep.Reason, w, sReason)...)
			}
			if oneLine(rep.Label, labelW) != rep.Label && rep.Kind == "llm" {
				out = append(out, wrap("  "+rep.Label, w, sMuted)...)
			}
		}
	}
	if fail := r.Failure(); len(fail) > 0 && r.Status != StatusPass {
		out = append(out, "", sRed.Bold(true).Render("Failures"))
		for _, l := range fail {
			out = append(out, wrap(strings.TrimSpace(l), w, sText)...)
		}
	}
	if r.Journal != "" {
		out = append(out, "", sMuted.Render("journal ")+sFaint.Render(r.Journal))
		if r.Status != StatusRunning {
			out = append(out, sMuted.Render("Press ")+sKey.Render("o")+sMuted.Render(" to open it in the run console, or ")+sKey.Render("2")+sMuted.Render(" for the conversation."))
		}
	}
	return out
}

func (m model) conversationLines(r Result, w int) []string {
	if r.Journal == "" {
		return []string{"", sMuted.Render("No journal was kept for this eval. bonnie eval keeps one in .bonnie/evals.")}
	}
	if r.Status == StatusRunning {
		return []string{"", sMuted.Render("The eval is running. The conversation shows when it ends.")}
	}
	st := m.convs[r.Journal]
	switch {
	case st == nil || st.loading:
		return []string{"", m.spin.View() + sMuted.Render(" reading journal…")}
	case st.err != nil:
		return []string{"", sRed.Render(st.err.Error())}
	case len(st.entries) == 0:
		return []string{"", sMuted.Render("The journal has no conversation.")}
	}
	out := []string{""}
	if len(st.models) > 0 {
		out = append(out, sMuted.Render("model ")+sText.Render(strings.Join(st.models, ", ")), "")
	}
	return append(out, renderConversation(st.entries, w)...)
}

// modelLines name the models of the eval: the agent's, from its journal,
// and each judge's, from the reports. A model that is not known is left
// out, so an eval with no journal and no judge shows none.
func (m model) modelLines(r Result, w int) []string {
	var judges, llms []string
	for _, rep := range r.Reports {
		switch {
		case rep.Model == "":
		case rep.Kind == "llm" && !slices.Contains(llms, rep.Model):
			llms = append(llms, rep.Model)
		case rep.Kind == "judge" && !slices.Contains(judges, rep.Model):
			judges = append(judges, rep.Model)
		}
	}
	var agent []string
	if st := m.convs[r.Journal]; r.Journal != "" && st != nil {
		agent = st.models
	}
	var out []string
	for _, l := range []struct {
		label  string
		models []string
	}{{"agent", agent}, {"judge", judges}, {"llm judge", llms}} {
		if len(l.models) > 0 {
			out = append(out, fit(sMuted.Render(fmt.Sprintf("%-10s", l.label))+sText.Render(strings.Join(l.models, ", ")), w))
		}
	}
	if len(out) > 0 {
		out = append([]string{""}, out...)
	}
	return out
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

// oneLine folds s onto one line and caps it at n cells.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n <= 0 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

// runTime renders the start time in a run ID ("20261029-120000", or
// "20261029-120000-002" for a second run in that second) as "12:00:00".
// Another ID is shown as it is.
func runTime(id string) string {
	const layout = "20060102-150405"
	if len(id) < len(layout) {
		return id
	}
	t, err := time.Parse(layout, id[:len(layout)])
	if err != nil {
		return id
	}
	return t.Format("15:04:05")
}

func lastElem(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
