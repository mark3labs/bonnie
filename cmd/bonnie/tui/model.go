// Package tui is the BONNIE terminal user interface.
//
// It is an HTTP client of the same channel a server exposes, so the transcript
// a developer sees is the transcript any client sees: one durable run, streamed
// from the journal. The model talks only to the wire the channel defines,
// which is what makes the TUI usable against any running bonnie server, not
// only the one `dev` started.
package tui

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// entryKind classes one transcript line so the view can style it.
type entryKind int

const (
	kindUser entryKind = iota
	kindAssistant
	kindQuestion
	kindTool
	kindReasoning
	kindStatus
	kindError
)

// entry is one line of the transcript. An assistant entry may be "open" while
// it is still streaming; the view shows it dimmed until the turn closes it.
type entry struct {
	kind       entryKind
	text       string
	streamed   bool
	toolCallID string
	toolResult string
	toolDone   bool
	toolError  bool
}

type status int

const (
	statusIdle status = iota
	statusRunning
	statusWaiting
	statusDone
	statusError
)

// Msg types.

// runMsg reports the outcome of an entry point that ended a turn.
type runMsg struct {
	run *runtime.Run
	err error
}

// streamReadyMsg carries the event stream once it is open.
type streamReadyMsg struct {
	ch    <-chan runtime.Event
	unsub func()
	err   error
}

// streamMsg carries one event from the run's stream.
type streamMsg struct {
	ev runtime.Event
}

// streamEndMsg reports the stream channel closed.
type streamEndMsg struct{}

// reconnectMsg asks the model to reopen a closed event stream.
type reconnectMsg struct{}

// cancelMsg reports the outcome of a cancel request.
type cancelMsg struct {
	err error
}

// spinnerMsg is a tick of the activity spinner.
type spinnerMsg struct{}

// spinnerFrames are the frames of the progress indicator shown while a turn
// runs. The model owns the index, so it survives value copies.
var spinnerFrames = []string{"◐", "◓", "◑", "◒"}

// spinnerRate is how often the progress frame advances.
const spinnerRate = 80 * time.Millisecond

// Model is the TUI. It drives one conversation over a [Client] and renders it
// as a scrollable transcript with a textarea for the next message.
type Model struct {
	client Client
	ctx    context.Context

	// address is the channel-local conversation key. The channel resolves it
	// to one run for the whole session.
	address string
	// runID is the run the address resolved to, set on the first turn.
	runID string

	state status
	label string

	input   textarea.Model
	initCmd tea.Cmd
	spin    bool

	// frame is the activity frame index; a fixed list is rendered while a
	// turn is running.
	frame int

	// entries is the permanent transcript.
	entries []entry
	// cursor is the last event sequence already folded into entries.
	cursor int

	// streamCh is the open event stream; a re-arming command reads it.
	streamCh <-chan runtime.Event
	streamUn func()

	width  int
	height int

	quitting bool
}

// New returns a Model that drives one conversation at address over client.
func New(client Client, ctx context.Context, address string) Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message… (enter to send, ctrl+c to quit)"
	ta.Prompt = "❯ "
	ta.SetVirtualCursor(false)
	ta.SetWidth(60)
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	focusCmd := ta.Focus()

	m := Model{
		client:  client,
		ctx:     ctx,
		address: address,
		state:   statusIdle,
		label:   "ready",
		input:   ta,
		initCmd: focusCmd,
	}
	return m
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.initCmd, func() tea.Msg { return spinnerMsg{} })
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case spinnerMsg:
		if m.spin {
			m.frame++
			return m, tea.Tick(spinnerRate, func(time.Time) tea.Msg { return spinnerMsg{} })
		}
		return m, nil

	case streamReadyMsg:
		if msg.err != nil {
			m.streamCh = nil
			m.streamUn = nil
			if m.quitting {
				return m, nil
			}
			m.label = "reconnecting"
			return m, tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return reconnectMsg{} })
		}
		m.streamCh = msg.ch
		m.streamUn = msg.unsub
		return m, m.readStream()

	case streamMsg:
		m.apply(msg.ev)
		return m, m.readStream()

	case streamEndMsg:
		m.stopStream()
		m.streamCh = nil
		m.streamUn = nil
		if m.quitting || m.runID == "" {
			return m, nil
		}
		m.label = "reconnecting"
		return m, tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return reconnectMsg{} })

	case reconnectMsg:
		if m.quitting || m.runID == "" || m.streamCh != nil {
			return m, nil
		}
		return m, m.openStream()

	case cancelMsg:
		if msg.err != nil {
			m.state = statusError
			m.label = msg.err.Error()
			m.commitError(msg.err.Error())
			return m, nil
		}
		m.label = "cancelling"
		return m, nil

	case runMsg:
		return m.finishTurn(msg)

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case cursor.BlinkMsg:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case tea.QuitMsg:
		m.quitting = true
		m.stopStream()
		m.streamCh = nil
		return m, nil
	}
	return m, nil
}

// View implements tea.Model.
func (m Model) View() tea.View {
	prefix := m.renderPrefix()
	v := tea.NewView(prefix + m.input.View() + footer)
	if c := m.input.Cursor(); c != nil {
		// prefix ends with the newline directly before the textarea. Count
		// separators, not rendered lines, or that trailing newline adds one
		// extra row and puts the cursor on the footer.
		c.Y += strings.Count(prefix, "\n")
		v.Cursor = c
	}
	v.AltScreen = false
	return v
}

// handleKey routes a key press: the textarea owns typing, the model owns the
// keys that send, cancel, or quit.
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		m.spin = false
		m.stopStream()
		m.streamCh = nil
		return m, tea.Quit
	case "ctrl+w":
		if !m.spin || m.runID == "" {
			return m, nil
		}
		m.label = "cancelling"
		return m, func() tea.Msg {
			return cancelMsg{err: m.client.Cancel(m.ctx, m.runID)}
		}
	case "enter":
		if m.spin {
			return m, nil // a turn is running; do not start another
		}
		value := m.input.Value()
		if strings.TrimSpace(value) == "" {
			return m, nil
		}
		m.input.Reset()
		return m.send(value)
	}

	// Anything else is typing. The textarea's own enter binding is not used:
	// the model intercepts enter above and sends.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// send routes the typed text to the right entry point: the first message
// starts the run, a message while waiting answers the suspension, anything
// else sends a new turn. It returns the updated model and the command that
// drives the turn.
func (m Model) send(text string) (tea.Model, tea.Cmd) {
	if text == "" || m.spin {
		return m, nil
	}
	start := m.runID == ""
	respond := m.state == statusWaiting && !start
	m.spin = true
	m.state = statusRunning
	m.label = "working"
	m.commitUser(text)

	var turn tea.Cmd
	switch {
	case start:
		turn = m.run(func() (*runtime.Run, error) {
			return m.client.Start(m.ctx, m.address, text)
		})
	case respond:
		turn = m.run(func() (*runtime.Run, error) {
			return m.client.Respond(m.ctx, m.runID, text)
		})
	default:
		turn = m.run(func() (*runtime.Run, error) {
			return m.client.Send(m.ctx, m.runID, text)
		})
	}
	return m, tea.Batch(turn, func() tea.Msg { return spinnerMsg{} })
}

// run performs one entry point. The first turn of a session also opens the
// durable stream once the run ID is known.
func (m Model) run(fn func() (*runtime.Run, error)) tea.Cmd {
	return func() tea.Msg {
		run, err := fn()
		if err != nil {
			return runMsg{err: err}
		}
		return runMsg{run: run, err: err}
	}
}

// finishTurn classifies the run a turn ended with and, on the first turn of a
// session, opens the durable stream for the conversation. The turn result is
// also a fallback for clients whose stream is not ready yet. A later durable
// event confirms or replaces that entry instead of adding a duplicate.
func (m Model) finishTurn(msg runMsg) (tea.Model, tea.Cmd) {
	m.spin = false
	if msg.err != nil {
		m.state = statusError
		m.label = msg.err.Error()
		m.commitError(msg.err.Error())
		return m, nil
	}
	run := msg.run
	if run == nil {
		m.state = statusError
		m.label = "run ended without a result"
		return m, nil
	}
	if m.runID == "" {
		m.runID = run.ID
		m.cursor = 0
	}
	// State comes from the turn result so the next send routes correctly
	// even before the stream delivers the matching event. Transcript text
	// comes from the stream, so it is not committed here.
	switch run.State {
	case runtime.RunWaiting:
		m.state = statusWaiting
		if run.Suspend != nil {
			m.label = run.Suspend.Prompt
			m.commit(kindQuestion, run.Suspend.Prompt)
		} else {
			m.label = "waiting"
		}
	case runtime.RunCompleted:
		m.state = statusDone
		m.label = "done"
		m.finishAssistant(run.Response)
	default:
		m.state = statusDone
		m.label = string(run.State)
	}

	// Open the stream once, after the first turn establishes the run ID.
	// Subsequent turns already have it open and keep it live.
	if m.streamCh == nil && m.runID != "" {
		return m, m.openStream()
	}
	return m, nil
}

// openStream subscribes to the run's event stream. The stream is durable: it
// replays the journal from the cursor, then stays live. Events arrive through
// a re-arming [Model.readStream] command.
func (m Model) openStream() tea.Cmd {
	return func() tea.Msg {
		ch, unsub, err := m.client.Stream(m.ctx, m.runID, m.cursor)
		return streamReadyMsg{ch: ch, unsub: unsub, err: err}
	}
}

// readStream reads the next event from the open stream. It re-arms once per
// event, which is how a continuous stream fits a value-receiver model.
func (m Model) readStream() tea.Cmd {
	return func() tea.Msg {
		if m.streamCh == nil {
			return streamEndMsg{}
		}
		ev, ok := <-m.streamCh
		if !ok {
			return streamEndMsg{}
		}
		return streamMsg{ev: ev}
	}
}

// stopStream closes the stream. Cancelling the context is the caller's job;
// this only calls the unsubscribe. Callers that want to clear the channel set
// [Model.streamCh] to nil on the model they return, because a value-receiver
// method cannot persist an assignment.
func (m Model) stopStream() {
	if m.streamUn != nil {
		m.streamUn()
	}
}

// apply folds one event into the transcript. Durable events are rendered; live
// Kit deltas are folded into the current assistant entry so the transcript does
// not grow a line per token.
func (m *Model) apply(ev runtime.Event) {
	if ev.Seq > m.cursor {
		m.cursor = ev.Seq
	}
	switch ev.Type {
	case runtime.EventState:
		switch ev.State {
		case runtime.RunWaiting:
			m.state = statusWaiting
		case runtime.RunRunning:
			m.state = statusRunning
			m.label = "working"
		case runtime.RunCompleted:
			m.closeStreamEntry()
		case runtime.RunFailed:
			m.closeStreamEntry()
		case runtime.RunCancelled:
			m.closeStreamEntry()
			m.state = statusIdle
			m.label = "cancelled"
		}

	case runtime.EventSuspend:
		m.commit(kindQuestion, ev.Text)
		m.state = statusWaiting
		m.label = ev.Text
		m.spin = false
		m.closeStreamEntry()

	case runtime.EventResume:
		// The resumed answer is already in the transcript as the user's reply.

	case runtime.EventResponse:
		m.finishAssistant(ev.Text)
		m.state = statusDone
		m.label = "done"
		m.spin = false

	default:
		m.applyKit(ev)
	}
}

// applyKit decodes one live lifecycle event forwarded from Kit and folds it
// into the transcript. These are live-only; they never replay.
func (m *Model) applyKit(ev runtime.Event) {
	switch ev.Type {
	case string(kit.EventMessageUpdate):
		var e kit.MessageUpdateEvent
		if json.Unmarshal(ev.Data, &e) == nil && e.Chunk != "" {
			m.appendAssistant(e.Chunk)
		}
	case string(kit.EventMessageEnd):
		m.closeStreamEntry()
	case string(kit.EventReasoningDelta):
		var e kit.ReasoningDeltaEvent
		if json.Unmarshal(ev.Data, &e) == nil && e.Delta != "" {
			m.appendReasoning(e.Delta)
		}
	case string(kit.EventReasoningComplete):
		m.closeReasoningEntry()
	case string(kit.EventToolCallStart):
		var e kit.ToolCallStartEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			m.startTool(e.ToolCallID, e.ToolName, "")
		}
	case string(kit.EventToolCall):
		var e kit.ToolCallEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			m.startTool(e.ToolCallID, e.ToolName, e.ToolArgs)
		}
	case string(kit.EventToolExecutionStart):
		var e kit.ToolExecutionStartEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			m.startTool(e.ToolCallID, e.ToolName, e.ToolArgs)
		}
	case string(kit.EventToolResult):
		var e kit.ToolResultEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			m.finishTool(e.ToolCallID, e.ToolName, e.ToolArgs, e.Result, e.IsError)
		}
	}
}

// appendAssistant appends a streamed chunk to the live assistant entry, or
// opens one when nothing is streaming yet.
func (m *Model) appendAssistant(chunk string) {
	m.openEntry()
	m.entries[len(m.entries)-1].text += chunk
	m.entries[len(m.entries)-1].streamed = true
}

// appendReasoning streams a reasoning chunk into a dim entry.
func (m *Model) appendReasoning(chunk string) {
	// Reasoning is shown collapsed as a single dim line; fold chunks.
	if len(m.entries) > 0 && m.entries[len(m.entries)-1].kind == kindReasoning {
		m.entries[len(m.entries)-1].text += chunk
	} else {
		m.entries = append(m.entries, entry{kind: kindReasoning, text: chunk, streamed: true})
	}
}

// closeReasoningEntry marks the reasoning entry closed.
func (m *Model) closeReasoningEntry() {
	if len(m.entries) > 0 && m.entries[len(m.entries)-1].kind == kindReasoning {
		m.entries[len(m.entries)-1].streamed = false
	}
}

// closeStreamEntry marks any open streaming entry closed, so the next turn
// opens fresh entries instead of appending to the previous turn's tail.
func (m *Model) closeStreamEntry() {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if m.entries[i].streamed &&
			(m.entries[i].kind == kindAssistant || m.entries[i].kind == kindReasoning) {
			m.entries[i].streamed = false
			break
		}
	}
}

// openEntry ensures a line can receive streamed chunks, so the view renders it
// as live. It opens an assistant entry when nothing is open.
func (m *Model) openEntry() {
	if len(m.entries) == 0 {
		m.entries = append(m.entries, entry{kind: kindAssistant, streamed: true})
		return
	}
	last := m.entries[len(m.entries)-1]
	if last.streamed && (last.kind == kindAssistant || last.kind == kindReasoning) {
		return
	}
	if last.kind == kindAssistant && last.text != "" && !last.streamed {
		m.entries = append(m.entries, entry{kind: kindAssistant, streamed: true})
		return
	}
	m.entries = append(m.entries, entry{kind: kindAssistant, streamed: true})
}

// finishAssistant replaces the live draft with the durable final response.
// A replay after a reconnect can then confirm the same response without
// adding a duplicate line.
func (m *Model) finishAssistant(text string) {
	if len(m.entries) > 0 {
		last := &m.entries[len(m.entries)-1]
		if last.kind == kindAssistant {
			last.streamed = false
			if text != "" {
				last.text = text
			}
			return
		}
	}
	m.commit(kindAssistant, text)
}

func (m *Model) commitUser(text string) {
	m.entries = append(m.entries, entry{kind: kindUser, text: text})
}

func (m *Model) commit(kind entryKind, text string) {
	if text == "" {
		return
	}
	if n := len(m.entries); n > 0 {
		last := m.entries[n-1]
		if last.kind == kind && last.text == text {
			return
		}
	}
	m.entries = append(m.entries, entry{kind: kind, text: text})
}

func (m *Model) commitError(text string) {
	m.entries = append(m.entries, entry{kind: kindError, text: text})
}

// startTool adds a working tool line or adds arguments to its earlier start
// event. Kit can send both events for one call.
func (m *Model) startTool(callID, name, args string) {
	if i := m.toolEntry(callID, name); i >= 0 {
		if args != "" {
			m.entries[i].text = toolLine(name, args)
		}
		return
	}
	m.entries = append(m.entries, entry{
		kind: kindTool, text: toolLine(name, args), streamed: true,
		toolCallID: callID,
	})
}

// finishTool changes the working marker to a check mark and keeps a compact
// result on the next indented line.
func (m *Model) finishTool(callID, name, args, result string, isError bool) {
	i := m.toolEntry(callID, name)
	if i < 0 {
		m.startTool(callID, name, args)
		i = len(m.entries) - 1
	} else if args != "" {
		m.entries[i].text = toolLine(name, args)
	}
	m.entries[i].streamed = false
	m.entries[i].toolDone = true
	m.entries[i].toolError = isError
	m.entries[i].toolResult = resultSnippet(result)
}

func (m *Model) toolEntry(callID, name string) int {
	for i, e := range slices.Backward(m.entries) {
		if e.kind != kindTool || e.toolDone {
			continue
		}
		if callID != "" && e.toolCallID == callID {
			return i
		}
		if callID == "" && strings.HasPrefix(e.text, name) {
			return i
		}
	}
	return -1
}

// layout sizes the input to the terminal. The transcript is ordinary
// scrollback, not a boxed viewport, so it does not fill the screen.
func (m *Model) layout() {
	w := max(m.width, 20)
	m.input.SetWidth(w)
	m.input.SetHeight(1)
}

// render assembles the frame: header, transcript, status, input, help.
// The viewport content is recomputed from the transcript on every frame, so it
// can never drift from [Model.entries].
func (m *Model) render() string {
	return m.renderPrefix() + m.input.View() + footer
}

func (m *Model) renderPrefix() string {
	var b strings.Builder
	b.WriteString(styles.header.Render(" bonnie chat · " + m.address + " "))
	b.WriteString("\n")
	if body := m.transcript(); body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString(m.statusLine())
	b.WriteString("\n")
	return b.String()
}

// statusLine renders the current mode and activity.
func (m *Model) statusLine() string {
	left := styles.status.Render(m.label)
	right := ""
	if m.spin {
		right = styles.cursor.Render(spinnerFrames[m.frame%len(spinnerFrames)])
	}
	if strings.TrimSpace(left) == "" && right == "" {
		return ""
	}
	return left + " " + right
}

// transcript renders every entry, styled by kind. Open (streamed) entries are
// rendered with an activity marker.
func (m *Model) transcript() string {
	var b strings.Builder
	for i, e := range m.entries {
		if i > 0 {
			b.WriteString("\n")
		}
		switch e.kind {
		case kindUser:
			b.WriteString(styles.user.Render("❯ " + e.text))
		case kindAssistant:
			if e.streamed {
				b.WriteString(styles.assistantStream.Render(e.text))
			} else {
				b.WriteString(styles.assistant.Render(e.text))
			}
		case kindQuestion:
			b.WriteString(styles.question.Render("◇ " + e.text))
		case kindTool:
			marker := "✓"
			if e.streamed {
				marker = spinnerFrames[m.frame%len(spinnerFrames)]
			}
			line := marker + " " + e.text
			if e.toolDone {
				line += "\n  → " + e.toolResult
			}
			if e.toolError {
				b.WriteString(styles.err.Render(line))
			} else {
				b.WriteString(styles.tool.Render(line))
			}
		case kindReasoning:
			b.WriteString(styles.reasoning.Render(e.text))
		case kindError:
			b.WriteString(styles.err.Render(e.text))
		default:
			b.WriteString(styles.status.Render(e.text))
		}
	}
	return b.String()
}

const footer = "\n" + "ctrl+c quit · ctrl+w cancel · enter send\n"

// toolLine formats a tool call compactly.
func toolLine(name, args string) string {
	args = strings.Join(strings.Fields(args), " ")
	if args == "" {
		return name
	}
	return name + "(" + truncate(args, 60) + ")"
}

// resultSnippet makes a one-line tool result and truncates it for the
// transcript.
func resultSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "ok"
	}
	return truncate(s, 80)
}

func truncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-1]) + "…"
}
