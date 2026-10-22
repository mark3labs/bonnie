package runsui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/wire"
)

// Server is the slice of the HTTP channel that operator actions use.
// [client.Client] satisfies it; a test substitutes a fake.
type Server interface {
	Health(ctx context.Context) (client.Health, error)
	Info(ctx context.Context) (client.Info, error)
	Send(ctx context.Context, runID, text string) (*wire.Run, error)
	RespondWith(ctx context.Context, runID string, responses []wire.InputResponse) (*wire.Run, error)
	RequestCancel(ctx context.Context, runID, expectedTurnID string) (wire.CancelResult, error)
	Reset(ctx context.Context, runID, reason string) error
	Clear(ctx context.Context, runID string) error
	Compact(ctx context.Context, runID string) error
}

var _ Server = (*client.Client)(nil)

// action names one operator action.
type action string

const (
	actAnswer  action = "answer"
	actSend    action = "send"
	actCancel  action = "cancel"
	actRetire  action = "retire"
	actClear   action = "clear"
	actCompact action = "compact"
)

// actionDoneMsg reports the result of one action.
type actionDoneMsg struct {
	act   action
	runID string
	run   *wire.Run
	err   error
}

// serverMsg reports a health check.
type serverMsg struct {
	ok   bool
	info client.Info
	err  error
}

func checkServer(ctx context.Context, s Server) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		h, err := s.Health(ctx)
		if err != nil {
			return serverMsg{err: err}
		}
		info, _ := s.Info(ctx)
		return serverMsg{ok: h.OK, info: info}
	}
}

// perform runs one action against the server. A turn runs detached from
// the request on the server side, so quitting the console while a send
// waits does not stop the turn.
func perform(ctx context.Context, s Server, act action, runID, turnID, text string, approved *bool) tea.Cmd {
	return func() tea.Msg {
		done := actionDoneMsg{act: act, runID: runID}
		switch act {
		case actAnswer:
			done.run, done.err = s.RespondWith(ctx, runID, []wire.InputResponse{{TurnID: turnID, Text: text, Approved: approved}})
		case actSend:
			done.run, done.err = s.Send(ctx, runID, text)
		case actCancel:
			var res wire.CancelResult
			res, done.err = s.RequestCancel(ctx, runID, turnID)
			_ = res
		case actRetire:
			done.err = s.Reset(ctx, runID, text)
		case actClear:
			done.err = s.Clear(ctx, runID)
		case actCompact:
			done.err = s.Compact(ctx, runID)
		}
		return done
	}
}

// actionPast is the toast for a finished action.
func actionPast(act action) string {
	switch act {
	case actAnswer:
		return "answer sent"
	case actSend:
		return "message sent"
	case actCancel:
		return "cancellation requested"
	case actRetire:
		return "run retired"
	case actClear:
		return "context cleared"
	case actCompact:
		return "context compacted"
	}
	return string(act)
}

// actionError makes a server error readable in one line.
func actionError(act action, err error) string {
	switch {
	case errors.Is(err, client.ErrNotFound):
		return fmt.Sprintf("%s: the server does not know this run — is it serving this journal?", act)
	case errors.Is(err, client.ErrUnauthenticated):
		return fmt.Sprintf("%s: the server refused the token (use --token)", act)
	}
	return fmt.Sprintf("%s: %v", act, err)
}

// modalKind selects how a modal takes input.
type modalKind int

const (
	modalConfirm modalKind = iota // y/n, with an optional note
	modalText                     // free text
	modalChoice                   // one of a list, with an optional note
	modalHelp                     // key reference
)

// modal is a dialog drawn over the console. Only one is open at a time,
// and it owns every key until it closes.
type modal struct {
	kind    modalKind
	act     action
	title   string
	body    string
	runID   string
	turnID  string
	danger  bool
	choices []string
	choice  int
	// approval marks a choice of Approve/Reject: the answer carries
	// Approved rather than the choice's text.
	approval bool
	note     bool
	input    textarea.Model
}

func newInput(placeholder string, height int) textarea.Model {
	ta := textarea.New()
	ta.Placeholder = placeholder
	ta.ShowLineNumbers = false
	ta.Prompt = "┃ "
	ta.SetHeight(height)
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "ctrl+j"))
	st := ta.Styles()
	st.Focused.Prompt = sAccent
	st.Focused.CursorLine = lipgloss.NewStyle()
	st.Blurred.Prompt = sFaint
	ta.SetStyles(st)
	ta.Focus()
	return ta
}

// answerModal builds the dialog for the run's open question.
func answerModal(r Run) *modal {
	req := r.Suspend
	m := &modal{act: actAnswer, runID: r.ID, turnID: req.TurnID, title: "Answer · " + r.Name(), body: req.Prompt}
	switch {
	case req.Kind == wire.SuspendApproval:
		m.kind, m.approval, m.note = modalChoice, true, true
		m.choices = []string{"Approve", "Reject"}
		m.input = newInput("optional note for the agent", 2)
		m.input.Blur()
	case len(req.Options) > 0:
		m.kind = modalChoice
		m.choices = req.Options
	default:
		m.kind = modalText
		m.input = newInput("your answer", 4)
	}
	return m
}

func sendModal(r Run) *modal {
	return &modal{kind: modalText, act: actSend, runID: r.ID, title: "Send · " + r.Name(), body: "Start a new turn on this run with a message.", input: newInput("message", 5)}
}

func confirmModal(r Run, act action) *modal {
	m := &modal{kind: modalConfirm, act: act, runID: r.ID, title: titleCase(string(act)) + " · " + r.Name()}
	switch act {
	case actCancel:
		m.danger = true
		m.body = "Stop the turn in progress and drop its pending input.\nCompleted steps stay in the journal; the run can be started again."
		if n := len(r.Turns); n > 0 {
			m.turnID = r.Turns[n-1].ID
		}
	case actRetire:
		m.danger, m.note = true, true
		m.body = "Close this run for good. Its history stays readable, but it\ntakes no more turns, and its address starts a fresh run."
		m.input = newInput("reason (optional)", 1)
	case actClear:
		m.body = "Drop the conversation from the model's context.\nThe journal keeps every record; the next message starts fresh."
	case actCompact:
		m.body = "Summarise the run's older messages now, to free context."
	}
	return m
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// canAct reports whether act makes sense for r, and why not.
func canAct(r Run, act action) (bool, string) {
	if r.State == runtime.RunRetired && act != actCompact {
		return false, "run is retired"
	}
	switch act {
	case actAnswer:
		if r.State != runtime.RunWaiting || r.Suspend == nil {
			return false, "run is not waiting for input"
		}
	case actCancel:
		if !r.Active() {
			return false, "run has no turn to cancel"
		}
	case actSend, actClear, actCompact:
		if r.State == runtime.RunRunning {
			return false, "run is busy — wait or cancel first"
		}
	}
	return true, ""
}

// view draws the modal box. w is the screen width.
func (m *modal) view(w int) string {
	bw := min(max(44, w*2/3), 92, w-2)
	inner := bw - 4
	bc := colorAccent
	if m.danger {
		bc = colorRed
	}
	var b strings.Builder
	title := sBold.Foreground(bc).Render(m.title)
	b.WriteString(oneLine(title, inner) + "\n\n")
	if m.body != "" {
		b.WriteString(lipgloss.Wrap(m.body, inner, "") + "\n\n")
	}
	switch m.kind {
	case modalConfirm:
		if m.note {
			m.input.SetWidth(inner)
			b.WriteString(m.input.View() + "\n\n")
		}
		yes := sBadgeOn.Background(bc).Render("enter " + string(m.act))
		b.WriteString(yes + "  " + sMuted.Render("esc keep"))
	case modalText:
		m.input.SetWidth(inner)
		b.WriteString(m.input.View() + "\n\n")
		b.WriteString(sKey.Render("enter") + sMuted.Render(" send · ") + sKey.Render("shift+enter") + sMuted.Render(" newline · ") + sKey.Render("esc") + sMuted.Render(" close"))
	case modalChoice:
		for i, c := range m.choices {
			mark, st := "  ", sText
			if i == m.choice && !m.input.Focused() {
				mark, st = sAccent.Render("› "), sAccent.Bold(true)
			} else if i == m.choice {
				mark = sMuted.Render("› ")
			}
			if m.approval {
				if c == "Approve" {
					st = st.Foreground(colorGreen)
				} else {
					st = st.Foreground(colorRed)
				}
			}
			b.WriteString(mark + st.Render(c) + "\n")
		}
		b.WriteString("\n")
		if m.note {
			m.input.SetWidth(inner)
			b.WriteString(m.input.View() + "\n\n")
			b.WriteString(sKey.Render("↑↓") + sMuted.Render(" choose · ") + sKey.Render("tab") + sMuted.Render(" note · "))
		} else {
			b.WriteString(sKey.Render("↑↓") + sMuted.Render(" choose · "))
		}
		b.WriteString(sKey.Render("enter") + sMuted.Render(" send · ") + sKey.Render("esc") + sMuted.Render(" close"))
	case modalHelp:
		b.Reset()
		b.WriteString(m.body)
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(bc).Padding(0, 1).Width(bw).Render(b.String())
}

// update handles a key while the modal is open. It returns submit=true
// with the answer when the user confirms, and closed=true when the modal
// should go away.
func (m *modal) update(msg tea.KeyPressMsg) (cmd tea.Cmd, submit, closed bool) {
	switch msg.String() {
	case "esc":
		return nil, false, true
	case "ctrl+c":
		return tea.Quit, false, true
	}
	switch m.kind {
	case modalHelp:
		return nil, false, true
	case modalConfirm:
		switch msg.String() {
		case "enter":
			return nil, true, true
		case "y", "Y":
			if !m.note {
				return nil, true, true
			}
		case "n", "N", "q":
			if !m.note {
				return nil, false, true
			}
		}
		if m.note {
			var c tea.Cmd
			m.input, c = m.input.Update(msg)
			return c, false, false
		}
	case modalText:
		if msg.String() == "enter" {
			if strings.TrimSpace(m.input.Value()) == "" {
				return nil, false, false
			}
			return nil, true, true
		}
		var c tea.Cmd
		m.input, c = m.input.Update(msg)
		return c, false, false
	case modalChoice:
		if m.note && m.input.Focused() {
			switch msg.String() {
			case "tab", "shift+tab", "up":
				m.input.Blur()
				return nil, false, false
			case "enter":
				return nil, true, true
			}
			var c tea.Cmd
			m.input, c = m.input.Update(msg)
			return c, false, false
		}
		switch msg.String() {
		case "up", "k", "shift+tab":
			m.choice = (m.choice - 1 + len(m.choices)) % len(m.choices)
		case "down", "j":
			m.choice = (m.choice + 1) % len(m.choices)
		case "tab":
			if m.note {
				return m.input.Focus(), false, false
			}
			m.choice = (m.choice + 1) % len(m.choices)
		case "enter":
			return nil, true, true
		default:
			// A digit picks an option directly.
			if s := msg.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
				if i := int(s[0] - '1'); i < len(m.choices) {
					m.choice = i
					return nil, true, true
				}
			}
		}
	}
	return nil, false, false
}

// answer is what the modal sends: the text and, for an approval, the
// decision.
func (m *modal) answer() (string, *bool) {
	switch m.kind {
	case modalChoice:
		if m.approval {
			yes := m.choice == 0
			return strings.TrimSpace(m.input.Value()), &yes
		}
		return m.choices[m.choice], nil
	case modalText, modalConfirm:
		if m.kind == modalConfirm && !m.note {
			return "", nil
		}
		return strings.TrimSpace(m.input.Value()), nil
	}
	return "", nil
}
