// Package inspect provides a read-only terminal view of BONNIE's durable runs.
package inspect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mark3labs/bonnie/runtime"
)

// Journal is the read-only journal API used by the inspector.
type Journal interface {
	Runs(context.Context, runtime.RunState) ([]string, error)
	Replay(context.Context, string) ([]runtime.Record, error)
	State(context.Context, string) (runtime.RunState, error)
}

// Run opens the terminal inspector. The caller owns and closes journal.
// A non-empty runID selects that run when the journal loads.
func Run(ctx context.Context, journal Journal, runID string) error {
	p := tea.NewProgram(newModel(ctx, journal, runID), tea.WithContext(ctx))
	_, err := p.Run()
	if err != nil {
		return fmt.Errorf("bonnie: run inspector: %w", err)
	}
	return nil
}

type runItem struct {
	id, title, state, last string
	steps                  int
}

func (i runItem) FilterValue() string { return i.id + " " + i.title + " " + i.state + " " + i.last }
func (i runItem) Title() string {
	if i.title == "" {
		return i.id
	}
	return i.title
}
func (i runItem) Description() string {
	return fmt.Sprintf("%s · %d steps · %s", i.state, i.steps, oneLine(i.last, 64))
}

type loadedMsg struct {
	generation uint64
	items      []runItem
	records    []runtime.Record
	state      runtime.RunState
	selected   string
	err        error
}
type refreshMsg struct{}
type keys struct{ Up, Down, Tab, Quit, Help, Refresh key.Binding }

func (k keys) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Tab, k.Help, k.Refresh, k.Quit}
}
func (k keys) FullHelp() [][]key.Binding { return [][]key.Binding{k.ShortHelp()} }

type model struct {
	ctx           context.Context
	journal       Journal
	selected      string
	items         []runItem
	records       []runtime.Record
	state         runtime.RunState
	list          list.Model
	viewport      viewport.Model
	width, height int
	loading       bool
	err           error
	help          bool
	showPayload   bool
	generation    uint64
	focus         int
	keymap        keys
	helpModel     help.Model
	spinner       spinner.Model
}

var (
	headStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	stateStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
)

func newModel(ctx context.Context, j Journal, selected string) model {
	d := list.NewDefaultDelegate()
	d.ShowDescription = true
	l := list.New([]list.Item{}, d, 30, 20)
	l.Title = "BONNIE · RUNS"
	l.SetShowStatusBar(true)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(true)
	sp := spinner.New()
	focus := 0
	if selected != "" {
		focus = 1
	}
	return model{ctx: ctx, journal: j, selected: selected, focus: focus, list: l, viewport: viewport.New(), loading: true, generation: 1, helpModel: help.New(), spinner: sp, keymap: keys{Up: key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "up")), Down: key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "down")), Tab: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus")), Quit: key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")), Help: key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")), Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh"))}}
}
func (m model) Init() tea.Cmd {
	return tea.Batch(m.load(m.generation, m.selected), m.spinner.Tick, refreshTick())
}
func refreshTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return refreshMsg{} })
}
func (m *model) startLoad() tea.Cmd {
	m.generation++ // Every request invalidates every older result.
	g, selected := m.generation, m.selected
	m.loading = true
	return m.load(g, selected)
}
func (m model) load(g uint64, selected string) tea.Cmd {
	return func() tea.Msg {
		ids, err := m.journal.Runs(m.ctx, "")
		if err != nil {
			return loadedMsg{generation: g, err: err}
		}
		items := []runItem{}
		var records []runtime.Record
		var state runtime.RunState
		for _, id := range ids {
			if runtime.IsReservedRun(id) {
				continue
			}
			recs, e := m.journal.Replay(m.ctx, id)
			if e != nil {
				return loadedMsg{generation: g, err: e}
			}
			st, e := m.journal.State(m.ctx, id)
			if e != nil {
				return loadedMsg{generation: g, err: e}
			}
			it := runItem{id: id, state: string(st)}
			for _, r := range recs {
				if r.Kind == runtime.RecordStep {
					it.steps++
				}
				if r.Kind == runtime.RecordExtensionData && r.ExtType == runtime.ExtTitle {
					it.title = r.Text
				}
				if r.Text != "" && (r.Kind == runtime.RecordMessage || r.Kind == runtime.RecordSuspend) {
					it.last = r.Text
				}
			}
			items = append(items, it)
			if id == selected {
				records, state = recs, st
			}
		}
		if selected == "" && len(items) > 0 {
			selected = items[0].id
			records, err = m.journal.Replay(m.ctx, selected)
			if err != nil {
				return loadedMsg{generation: g, err: err}
			}
			state, err = m.journal.State(m.ctx, selected)
			if err != nil {
				return loadedMsg{generation: g, err: err}
			}
		}
		if selected != "" {
			found := false
			for _, it := range items {
				if it.id == selected {
					found = true
				}
			}
			if !found {
				return loadedMsg{generation: g, err: fmt.Errorf("run %q not found", selected)}
			}
		}
		return loadedMsg{generation: g, items: items, records: records, state: state, selected: selected}
	}
}
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case loadedMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			oldID := msg.selected
			changed := m.selected != msg.selected
			m.items, m.records, m.state, m.selected = msg.items, msg.records, msg.state, msg.selected
			its := make([]list.Item, len(m.items))
			idx := 0
			for i := range m.items {
				its[i] = m.items[i]
				if m.items[i].id == oldID {
					idx = i
				}
			}
			cmd := m.list.SetItems(its)
			if len(its) > 0 && m.list.FilterState() == list.Unfiltered {
				if oldID != "" {
					for i, it := range m.items {
						if it.id == oldID {
							idx = i
							break
						}
					}
				}
				m.list.Select(idx)
			}
			m.syncDetail()
			if changed {
				m.viewport.GotoTop()
			}
			return m, cmd
		}
		m.syncDetail()
		return m, nil
	case refreshMsg:
		if m.loading || m.list.FilterState() != list.Unfiltered {
			return m, refreshTick()
		}
		return m, tea.Batch(m.startLoad(), refreshTick())
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.helpModel.SetWidth(msg.Width)
		m.layout()
		return m, nil
	case tea.KeyPressMsg:
		k := msg.String()
		switch k {
		case "ctrl+c":
			return m, tea.Quit
		case "tab":
			if m.list.FilterState() == list.Filtering {
				return m, nil
			}
			m.focus = (m.focus + 1) % 2
			return m, nil
		case "?":
			if m.list.FilterState() == list.Unfiltered {
				m.help = !m.help
				return m, nil
			}
		case "v":
			if m.focus == 1 {
				m.showPayload = !m.showPayload
				m.syncDetail()
				return m, nil
			}
		case "r":
			if m.list.FilterState() == list.Unfiltered {
				return m, m.startLoad()
			}
		case "enter":
			if m.list.FilterState() != list.Filtering {
				m.focus = 1
				return m, nil
			}
		}
		if m.focus == 1 {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
	}
	// Always route ordinary keys through the list. Filtering owns printable keys such as r and ?.
	old := m.selected
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	if m.focus == 0 {
		if it, ok := m.list.SelectedItem().(runItem); ok && it.id != old {
			m.selected = it.id
			m.viewport.GotoTop()
			return m, tea.Batch(cmd, m.startLoad())
		}
	}
	return m, cmd
}
func (m *model) layout() {
	if m.width < 1 {
		return
	}
	innerW := m.width - 2
	innerH := max(1, m.height-4)
	if innerW < 1 {
		innerW = 1
	}
	if m.width >= 90 {
		left := (innerW - 1) / 3
		right := innerW - left - 1
		m.list.SetSize(left, innerH)
		m.viewport.SetWidth(right)
		m.viewport.SetHeight(innerH)
	} else {
		m.list.SetSize(innerW, innerH)
		m.viewport.SetWidth(innerW)
		m.viewport.SetHeight(innerH)
	}
	m.syncDetail()
}
func (m *model) syncDetail() {
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s  ·  state: %s  ·  records: %d\n\n", m.selected, runStateStyle(m.state).Render(string(m.state)), len(m.records))
	for _, r := range m.records {
		fmt.Fprintf(&b, "%04d  %s  %s", r.Seq, r.Timestamp.Format("15:04:05"), headStyle.Render(string(r.Kind)))
		detail := r.Text
		if r.Kind == runtime.RecordMessage && r.Role != "" {
			detail = r.Role + ": " + detail
		}
		if r.Kind == runtime.RecordState {
			detail = string(r.State)
		}
		if len(r.Payload) > 0 && (m.showPayload || (r.Kind == runtime.RecordMessage && r.Text == "")) {
			var value any
			if json.Unmarshal(r.Payload, &value) == nil {
				if raw, e := json.MarshalIndent(value, "    ", "  "); e == nil {
					detail += "\n    Payload:\n    " + strings.ReplaceAll(string(raw), "\n", "\n    ")
				}
			}
		}
		if r.Kind == runtime.RecordSuspend {
			var req runtime.SuspendRequest
			if json.Unmarshal(r.Payload, &req) == nil {
				detail = fmt.Sprintf("%s: %s", req.Kind, req.Prompt)
				if len(req.Options) > 0 {
					detail += " [" + strings.Join(req.Options, ", ") + "]"
				}
			}
		}
		if detail != "" {
			fmt.Fprintf(&b, "\n    %s", detail)
		}
		b.WriteString("\n\n")
	}
	if len(m.records) == 0 && !m.loading && m.err == nil {
		b.WriteString("No records for this run.\n")
	}
	offset := m.viewport.YOffset()
	m.viewport.SetContent(lipgloss.NewStyle().Width(max(1, m.viewport.Width())).Render(b.String()))
	m.viewport.SetYOffset(offset)
}
func (m model) View() tea.View {
	if m.width <= 0 {
		v := tea.NewView("Loading run journal…")
		v.AltScreen = true
		return v
	}
	innerW := max(1, m.width-2)
	innerH := max(1, m.height-4)
	var panel string
	if m.focus == 0 {
		m.list.Title = "BONNIE · RUNS ●"
	} else {
		m.list.Title = "BONNIE · RUNS"
	}
	if m.width >= 90 {
		leftW := (innerW - 1) / 3
		rightW := innerW - leftW - 1
		m.list.SetSize(leftW, innerH)
		m.viewport.SetWidth(rightW)
		m.viewport.SetHeight(innerH)
		panel = lipgloss.JoinHorizontal(lipgloss.Top, m.list.View(), " ", m.viewport.View())
	} else {
		m.list.SetSize(innerW, innerH)
		m.viewport.SetWidth(innerW)
		m.viewport.SetHeight(innerH)
		if m.focus == 0 {
			panel = m.list.View()
		} else {
			panel = m.viewport.View()
		}
	}
	if m.err != nil {
		panel = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Width(innerW).Render("Error: "+m.err.Error()) + "\n" + panel
	}
	panel = lipgloss.NewStyle().Width(innerW).Height(innerH).MaxWidth(innerW).MaxHeight(innerH).Render(panel)
	frame := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("205")).Render(panel)
	v := tea.NewView(lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(frame + "\n" + lipgloss.NewStyle().MaxWidth(m.width).Render(m.footer())))
	v.AltScreen = true
	return v
}
func (m model) footer() string {
	if m.loading {
		return mutedStyle.Render(m.spinner.View() + " Loading journal…")
	}
	if m.width < 90 {
		if m.help {
			return mutedStyle.Render("/ filter · v payload · r refresh · ctrl+c quit")
		}
		return mutedStyle.Render("tab focus · ↑/↓ move · ? help · ctrl+c quit")
	}
	if m.help {
		return m.helpModel.View(m.keymap) + " · / filter · v payload"
	}
	focus := "runs"
	if m.focus == 1 {
		focus = "detail"
	}
	return headStyle.Render(focus+" · ") + mutedStyle.Render("↑/↓ move · tab focus · / filter · v payload · r refresh · ? help · ctrl+c quit · read only")
}
func runStateStyle(state runtime.RunState) lipgloss.Style {
	color := "81"
	switch state {
	case runtime.RunWaiting:
		color = "227"
	case runtime.RunCompleted:
		color = "42"
	case runtime.RunFailed:
		color = "203"
	}
	return stateStyle.Foreground(lipgloss.Color(color))
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:max(0, n-1)]) + "…"
	}
	return s
}
