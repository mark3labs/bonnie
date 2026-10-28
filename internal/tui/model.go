package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/internal/selection"
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
	kindError
	// kindLog is a line of process output, such as the dev loop's rebuild
	// messages and the child's startup banner. See [LogWriter].
	kindLog
)

// entry is one line of the transcript. An assistant entry may be "open" while
// it is still streaming; the view renders it live while open and from the
// markdown render cache once closed.
type entry struct {
	kind       entryKind
	text       string
	streamed   bool
	toolCallID string
	toolResult string
	toolDone   bool
	toolError  bool

	// rendered caches the markdown render of a closed assistant entry. The
	// render does not depend on the width: the transcript is wrapped as a
	// whole afterwards (see [Model.syncViewport]). The open streaming entry
	// re-renders every frame instead, because its text is still growing.
	rendered string
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
	generation int
	run        *runtime.Run
	err        error
}

// lookupMsg reports whether this address already has a durable run.
type lookupMsg struct {
	generation int
	snapshot   *client.Snapshot
	run        *runtime.Run
	runID      string
	cursor     int
	err        error
}

// streamReadyMsg carries the event stream once it is open.
type streamReadyMsg struct {
	generation int
	ch         <-chan runtime.Event
	unsub      func()
	err        error
}

// ensureMsg reports the run an address resolves to, created if it was new.
// It runs no turn; it only teaches the TUI its run ID early enough to
// subscribe.
type ensureMsg struct {
	generation int
	snapshot   *client.Snapshot
	runID      string
	cursor     int
	err        error
}

// streamMsg carries one event from the run's stream.
type streamMsg struct {
	generation int
	ev         runtime.Event
}

// streamEndMsg reports the stream channel closed.
type streamEndMsg struct{ generation int }

// reconnectMsg asks the model to reopen a closed event stream.
type reconnectMsg struct{ generation int }

// cancelMsg reports the outcome of a cancel request.
type cancelMsg struct {
	generation int
	err        error
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

	// address is the channel-local conversation key, or the selected run ID
	// shown in the header when selectedRun is set.
	address string
	// selectedRun is an explicit run ID. It must never resolve as an address.
	selectedRun string
	// runID is the loaded run, selected by ID or resolved from an address.
	// It is loaded before the
	// first turn is sent, not after it returns: a turn's reasoning and tool
	// events are live-only, so a stream opened after the turn has already
	// missed them.
	runID  string
	turnID string

	// pending holds the first message while the run is resolved and the
	// stream opens. It is sent once the stream is live, so the turn cannot
	// outrun its own subscription. Empty at every other time.
	pending string

	state status
	label string

	viewport   viewport.Model
	generation int

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

	// rows is the transcript as the viewport shows it, wrapped to the width,
	// with the plain text and soft breaks that a mouse selection copies.
	rows selection.Wrapped
	// sel is the mouse selection, in content rows of the transcript, so it
	// stays on its text while the transcript scrolls.
	sel selection.Selection
	// followAfter records that the reader was at the bottom when the button
	// went down. The transcript stops following new output during a drag,
	// so the text under the pointer stays still; a click that selects
	// nothing goes back to following.
	followAfter bool

	quitting bool
	// loading prevents a send from racing the initial history read.
	loading bool
}

// New returns a Model that drives one conversation at address over client.
func New(client Client, ctx context.Context, address string) Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message or /help…"
	// The prompt marks where the message starts; the rows below it, when
	// the input grows, are indented to line up with the first.
	ta.SetPromptFunc(2, func(p textarea.PromptInfo) string {
		if p.LineNumber == 0 {
			return "❯ "
		}
		return "  "
	})
	ta.SetVirtualCursor(false)
	ta.MaxWidth = 0
	ta.SetWidth(80)
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline.SetKeys("shift+enter", "alt+enter", "ctrl+m")
	// The input grows with its text, up to maxInputRows; layout sets
	// MaxHeight. MaxContentHeight is the textarea's own limit on lines.
	// Without it, the textarea takes MaxHeight as that limit too, and
	// shift+enter would stop at the last row that shows.
	ta.DynamicHeight = true
	ta.MaxContentHeight = maxInputLines
	focusCmd := ta.Focus()

	m := Model{
		client:   client,
		ctx:      ctx,
		address:  address,
		state:    statusIdle,
		label:    "ready",
		input:    ta,
		viewport: viewport.New(viewport.WithWidth(80), viewport.WithHeight(20)),
		initCmd:  focusCmd,
		loading:  true,
	}
	m.layout()
	return m
}

// NewRun returns a Model that loads an existing run and its saved history.
// An unknown run is an error; it does not create a conversation.
func NewRun(client Client, ctx context.Context, runID string) Model {
	m := New(client, ctx, runID)
	m.selectedRun = runID
	return m
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.initCmd, func() tea.Msg { return spinnerMsg{} }, m.lookup())
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Ignore results from a conversation that /new has left.
	generation := m.generation
	switch v := msg.(type) {
	case lookupMsg:
		generation = v.generation
	case ensureMsg:
		generation = v.generation
	case runMsg:
		generation = v.generation
	case cancelMsg:
		generation = v.generation
	case streamMsg:
		generation = v.generation
	case streamEndMsg:
		generation = v.generation
	case reconnectMsg:
		generation = v.generation
	case streamReadyMsg:
		generation = v.generation
		if generation != m.generation && v.unsub != nil {
			v.unsub()
		}
	}
	if generation != m.generation {
		return m, nil
	}
	next, cmd := m.update(msg)
	result := next.(Model)
	result.fitTranscript()
	result.syncViewport()
	return result, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width != m.width {
			// The rows wrap again at a new width, so the selected cells no
			// longer hold the selected text.
			m.sel.Clear()
		}
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case logMsg:
		m.commitLog(msg.lines)
		return m, nil

	case spinnerMsg:
		if m.spin {
			m.frame++
			return m, tea.Tick(spinnerRate, func(time.Time) tea.Msg { return spinnerMsg{} })
		}
		return m, nil

	case lookupMsg:
		m.loading = false
		if msg.err != nil {
			if errors.Is(msg.err, ErrNotFound) && m.selectedRun == "" {
				return m, nil
			}
			m.state = statusError
			m.label = msg.err.Error()
			m.commitError(msg.err.Error())
			return m, nil
		}
		if msg.runID == "" || m.runID != "" || m.pending != "" {
			// A held first message means the ensure path already owns the
			// handshake. Two openers would leave one stream unreferenced.
			return m, nil
		}
		m.runID = msg.runID
		m.cursor = msg.cursor
		if msg.snapshot != nil {
			m.restoreSnapshot(msg.snapshot)
			return m, tea.Batch(m.openStream(), func() tea.Msg { return spinnerMsg{} })
		}
		if msg.run != nil {
			return m.finishTurn(runMsg{run: msg.run})
		}
		return m, m.openStream()

	case ensureMsg:
		// The run is known (or could not be resolved). Either way the held
		// message must go out; with a run, only after the stream is open.
		if msg.err != nil {
			m.label = "streaming unavailable"
			return m.sendPending()
		}
		if m.runID == "" {
			m.runID = msg.runID
			m.cursor = msg.cursor
		}
		if msg.snapshot != nil {
			m.restoreSnapshot(msg.snapshot)
			m.commitUser(m.pending)
			m.spin = true
		}
		if m.streamCh != nil {
			return m.sendPending()
		}
		return m, m.openStream()

	case streamReadyMsg:
		if msg.err != nil {
			m.streamCh = nil
			m.streamUn = nil
			if m.quitting {
				return m, nil
			}
			// A held first message must not wait on a stream that is not
			// coming. Send it and let the reconnect catch up.
			if m.pending != "" {
				next, turn := m.sendPending()
				next.label = "reconnecting"
				return next, tea.Batch(turn, tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return reconnectMsg{generation: m.generation} }))
			}
			m.label = "reconnecting"
			return m, tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return reconnectMsg{generation: m.generation} })
		}
		m.streamCh = msg.ch
		m.streamUn = msg.unsub
		// The stream is live, so the first turn can now be dispatched
		// without racing its own events.
		if m.pending != "" {
			next, turn := m.sendPending()
			return next, tea.Batch(turn, next.readStream())
		}
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
		return m, tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return reconnectMsg{generation: m.generation} })

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

	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case tea.MouseClickMsg:
		return m.mouseDown(msg.Mouse())

	case tea.MouseMotionMsg:
		return m.mouseDrag(msg.Mouse())

	case tea.MouseReleaseMsg:
		return m.mouseUp()

	case tea.PasteMsg:
		// A terminal with bracketed paste sends a paste as one message, not
		// as key presses. The textarea inserts it, newlines included; a key
		// press per character would send the message at the first newline.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

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
	content := prefix + m.input.View() + m.helpFooter()
	if m.height > 0 && m.height < 5 {
		// Keep the input visible when there is no room for a transcript.
		prefix = ""
		content = m.input.View()
	}
	v := tea.NewView(content)
	if c := m.input.Cursor(); c != nil {
		// prefix ends with the newline directly before the textarea. Count
		// separators, not rendered lines, or that trailing newline adds one
		// extra row and puts the cursor on the footer.
		c.Y += strings.Count(prefix, "\n")

		v.Cursor = c
	}
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// handleKey routes a key press: the textarea owns typing, the model owns the
// keys that send, cancel, or quit.
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// The highlight shows what the last drag copied; a key press moves on.
	m.sel.Clear()
	switch msg.String() {
	case "pgup", "pgdown":
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	case "ctrl+home":
		m.viewport.GotoTop()
		return m, nil
	case "ctrl+end":
		m.viewport.GotoBottom()
		return m, nil
	case "ctrl+c":
		m.quitting = true
		m.spin = false
		m.stopStream()
		m.streamCh = nil
		return m, tea.Quit
	case "ctrl+w":
		if (!m.spin && m.state != statusWaiting) || m.runID == "" {
			return m, nil
		}
		m.label = "cancelling"
		return m, func() tea.Msg {
			if scoped, ok := m.client.(interface {
				RequestCancel(context.Context, string, string) (runtime.CancelResult, error)
			}); ok {
				_, err := scoped.RequestCancel(m.ctx, m.runID, m.turnID)
				return cancelMsg{generation: m.generation, err: err}
			}
			return cancelMsg{generation: m.generation, err: m.client.Cancel(m.ctx, m.runID)}
		}
	case "enter":
		if strings.HasPrefix(strings.TrimSpace(m.input.Value()), "/") {
			value := strings.TrimSpace(m.input.Value())
			m.input.Reset()
			return m.slash(value)
		}
		if m.spin || m.loading {
			return m, nil // a turn or history read is running; wait
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
	if text == "" || m.spin || m.loading || (m.selectedRun != "" && m.runID == "") {
		return m, nil
	}
	start := m.runID == ""
	respond := m.state == statusWaiting && !start
	m.spin = true
	m.state = statusRunning
	m.label = "working"
	m.commitUser(text)
	m.syncViewport()
	m.viewport.GotoBottom()

	// The run is not known yet, so nothing can be subscribed to. Resolve it
	// first and hold the message: the turn is dispatched from
	// streamReadyMsg, once the stream that will carry its reasoning and tool
	// events exists. Sending now would race the subscription and lose them.
	if start {
		m.pending = text
		return m, tea.Batch(m.ensure(), func() tea.Msg { return spinnerMsg{} })
	}

	var turn tea.Cmd
	if respond {
		turn = m.run(func() (*runtime.Run, error) {
			return m.client.Respond(m.ctx, m.runID, text)
		})
	} else {
		turn = m.run(func() (*runtime.Run, error) {
			return m.client.Send(m.ctx, m.runID, text)
		})
	}
	return m, tea.Batch(turn, func() tea.Msg { return spinnerMsg{} })
}

// sendPending dispatches the held first message. The stream is open by now,
// so the turn's events reach the transcript as they happen.
//
// It still goes through the address entry point: the address resolves to the
// run [Model.ensure] just bound, so this is the same turn it always was, only
// now it is sent to a conversation something is already listening to.
func (m Model) sendPending() (Model, tea.Cmd) {
	text := m.pending
	m.pending = ""
	address := m.address
	respond := m.state == statusWaiting && m.runID != ""
	m.state, m.label = statusRunning, "working"
	return m, m.run(func() (*runtime.Run, error) {
		if respond {
			return m.client.Respond(m.ctx, m.runID, text)
		}
		return m.client.Start(m.ctx, address, text)
	})
}

// run performs one entry point. The first turn of a session also opens the
// durable stream once the run ID is known.
func (m Model) run(fn func() (*runtime.Run, error)) tea.Cmd {
	return func() tea.Msg {
		run, err := fn()
		if err != nil {
			return runMsg{generation: m.generation, err: err}
		}
		return runMsg{generation: m.generation, run: run, err: err}
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

// restoreSnapshot projects text parts only. Tool calls, tool results, and
// reasoning remain in the lossless wire snapshot, but are not display text.
// Append each message directly so repeated text remains separate history.
func (m *Model) restoreSnapshot(snapshot *client.Snapshot) {
	m.cursor = snapshot.Cursor
	m.entries = nil
	for _, msg := range snapshot.Messages {
		var kind entryKind
		switch string(msg.Role) {
		case "user":
			kind = kindUser
		case "assistant":
			kind = kindAssistant
		default:
			continue
		}
		var text strings.Builder
		for _, part := range msg.Content {
			if p, ok := part.(kit.LLMTextPart); ok {
				text.WriteString(p.Text)
			}
		}
		if text.Len() > 0 {
			m.entries = append(m.entries, entry{kind: kind, text: text.String()})
		}
	}
	m.spin = false
	m.turnID = snapshot.TurnID
	switch snapshot.State {
	case runtime.RunWaiting:
		m.state, m.label = statusWaiting, "waiting"
		if snapshot.Suspend != nil {
			m.label = snapshot.Suspend.Prompt
			m.commit(kindQuestion, snapshot.Suspend.Prompt)
		}
	case runtime.RunRunning:
		m.state, m.label, m.spin = statusRunning, "working", true
	case runtime.RunPending:
		m.state, m.label = statusIdle, "ready"
	case runtime.RunFailed:
		m.state, m.label = statusError, "failed"
	default:
		m.state, m.label = statusDone, string(snapshot.State)
	}
}

func (m Model) lookup() tea.Cmd {
	return func() tea.Msg {
		if m.selectedRun != "" {
			snapshot, err := m.client.Snapshot(m.ctx, m.selectedRun)
			return lookupMsg{generation: m.generation, runID: m.selectedRun, snapshot: snapshot, err: err}
		}
		runID, cursor, err := m.client.Lookup(m.ctx, m.address)
		msg := lookupMsg{generation: m.generation, runID: runID, cursor: cursor, err: err}
		if err == nil && runID != "" {
			msg.snapshot, msg.err = m.client.Snapshot(m.ctx, runID)
			if msg.snapshot != nil {
				msg.cursor = msg.snapshot.Cursor
			}
		}
		return msg
	}
}

// ensure resolves the address to a run, creating one if the address is new.
// It runs no turn: it is what lets the TUI subscribe before it speaks.
func (m Model) ensure() tea.Cmd {
	address := m.address
	return func() tea.Msg {
		runID, cursor, err := m.client.Ensure(m.ctx, address)
		msg := ensureMsg{generation: m.generation, runID: runID, cursor: cursor, err: err}
		if err == nil && runID != "" {
			msg.snapshot, msg.err = m.client.Snapshot(m.ctx, runID)
			if msg.snapshot != nil {
				msg.cursor = msg.snapshot.Cursor
			}
		}
		return msg
	}
}

// openStream subscribes to the run's event stream. The stream is durable: it
// replays the journal from the cursor, then stays live. Events arrive through
// a re-arming [Model.readStream] command.
func (m Model) openStream() tea.Cmd {
	return func() tea.Msg {
		ch, unsub, err := m.client.Stream(m.ctx, m.runID, m.cursor)
		return streamReadyMsg{generation: m.generation, ch: ch, unsub: unsub, err: err}
	}
}

// readStream reads the next event from the open stream. It re-arms once per
// event, which is how a continuous stream fits a value-receiver model.
func (m Model) readStream() tea.Cmd {
	return func() tea.Msg {
		if m.streamCh == nil {
			return streamEndMsg{generation: m.generation}
		}
		ev, ok := <-m.streamCh
		if !ok {
			return streamEndMsg{generation: m.generation}
		}
		return streamMsg{generation: m.generation, ev: ev}
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
	if ev.TurnID != "" {
		m.turnID = ev.TurnID
	}
	if ev.Seq > m.cursor {
		m.cursor = ev.Seq
	}
	switch ev.Type {
	case runtime.EventState:
		switch ev.State {
		case runtime.RunWaiting:
			m.state, m.spin = statusWaiting, false
		case runtime.RunRunning:
			m.state, m.label, m.spin = statusRunning, "working", true
		case runtime.RunCompleted:
			m.closeStreamEntry()
			m.state, m.label, m.spin = statusDone, "done", false
		case runtime.RunFailed:
			m.closeStreamEntry()
			m.state, m.label, m.spin = statusError, "failed", false
		case runtime.RunCancelled:
			m.closeStreamEntry()
			m.state, m.label, m.spin = statusIdle, "cancelled", false
		case runtime.RunRetired:
			m.closeStreamEntry()
			m.state, m.label, m.spin = statusDone, "retired", false
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
	// Fold chunks into one dim entry; the viewport wraps it to the width.
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
				// The text changed, so a cached render of the streamed
				// draft no longer describes this entry.
				last.rendered = ""
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

// commitLog adds process output to the transcript. A streaming entry must
// stay last, because each chunk is added to the last entry, so the lines go
// in front of it.
func (m *Model) commitLog(lines []string) {
	if len(lines) == 0 {
		return
	}
	e := entry{kind: kindLog, text: strings.Join(lines, "\n")}
	if n := len(m.entries); n > 0 && m.entries[n-1].streamed {
		m.entries = slices.Insert(m.entries, n-1, e)
		return
	}
	m.entries = append(m.entries, e)
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

const (
	// maxInputRows is the most rows the input grows to. Text longer than
	// that scrolls inside the input.
	maxInputRows = 8
	// maxInputLines is the most lines the input holds: the textarea's own
	// limit for a paste.
	maxInputLines = 10000
)

// layout reserves fixed rows for the header, status, and help, gives the
// input the rows its text needs, and gives the rest to the transcript.
func (m *Model) layout() {
	w := m.textWidth()
	bottom := m.viewport.AtBottom()
	// The input may take at most half of the rows that are not the header,
	// status, or help, so a short terminal keeps a transcript.
	m.input.MaxHeight = min(max(1, (m.screenHeight()-4)/2), maxInputRows)
	// SetWidth also wraps the text again and sets the input's height.
	m.input.SetWidth(w)
	m.viewport.SetWidth(w)
	m.viewport.FillHeight = true
	m.fitTranscript()
	m.syncViewport()
	if bottom {
		m.viewport.GotoBottom()
	}
}

// screenHeight is the height of the terminal, or 24 before the first size.
func (m *Model) screenHeight() int {
	if m.height <= 0 {
		return 24
	}
	return m.height
}

// fitTranscript gives the transcript the rows that the input does not use.
// The input grows and shrinks with its text, as a key press, a paste, or a
// send changes it; a reader at the bottom stays there.
func (m *Model) fitTranscript() {
	rows := max(0, m.screenHeight()-4-m.input.Height())
	if m.viewport.Height() == rows {
		return
	}
	bottom := m.viewport.AtBottom()
	m.viewport.SetHeight(rows)
	if bottom && !m.sel.Dragging() {
		m.viewport.GotoBottom()
	}
}

// syncViewport follows output only while the reader is at the bottom, and
// never during a drag.
//
// The transcript is wrapped here, once, and not inside each entry's render:
// [selection.Wrap] records which row breaks it made, so a selection copies a
// wrapped paragraph or code line as the one line it is.
func (m *Model) syncViewport() {
	bottom := m.viewport.AtBottom() && !m.sel.Dragging()
	m.rows = selection.Wrap(m.transcript(), m.textWidth())
	m.viewport.SetContentLines(m.sel.Highlight(m.rows.Rows, 0, styles.selection))
	if bottom {
		m.viewport.GotoBottom()
	}
}

// transcriptTop is the screen row of the first transcript row, below the
// header.
const transcriptTop = 1

// contentPoint maps a screen cell to a transcript row and column.
func (m *Model) contentPoint(x, y int) selection.Point {
	return selection.Point{Line: m.viewport.YOffset() + y - transcriptTop, Col: max(0, x)}
}

// mouseDown starts a selection when the left button goes down on the
// transcript. Anywhere else it only drops the old selection.
func (m Model) mouseDown(ms tea.Mouse) (tea.Model, tea.Cmd) {
	m.sel.Clear()
	if ms.Button != tea.MouseLeft || (m.height > 0 && m.height < 5) {
		return m, nil
	}
	if ms.Y < transcriptTop || ms.Y >= transcriptTop+m.viewport.Height() {
		return m, nil
	}
	m.followAfter = m.viewport.AtBottom()
	m.sel.Press(m.contentPoint(ms.X, ms.Y))
	return m, nil
}

// mouseDrag extends the selection. Above or below the transcript it scrolls
// one row for each motion event, so a selection can be longer than the
// screen.
func (m Model) mouseDrag(ms tea.Mouse) (tea.Model, tea.Cmd) {
	if !m.sel.Dragging() {
		return m, nil
	}
	y := ms.Y
	bottom := transcriptTop + m.viewport.Height() - 1
	switch {
	case y < transcriptTop:
		m.viewport.ScrollUp(1)
		y = transcriptTop
	case y > bottom:
		m.viewport.ScrollDown(1)
		y = bottom
	}
	m.sel.Drag(m.contentPoint(ms.X, y))
	return m, nil
}

// mouseUp ends the drag and copies the selection. The highlight stays until
// the next click or key press, so the reader can see what was copied.
func (m Model) mouseUp() (tea.Model, tea.Cmd) {
	if !m.sel.Dragging() {
		return m, nil
	}
	m.sel.Release()
	text := m.sel.Text(m.rows.Plain, m.rows.Joined)
	if text == "" {
		m.sel.Clear()
		if m.followAfter {
			m.viewport.GotoBottom()
		}
		return m, nil
	}
	m.label = fmt.Sprintf("copied %d characters", utf8.RuneCountInString(text))
	return m, selection.Copy(text)
}

func (m *Model) render() string {
	return m.renderPrefix() + m.input.View() + m.helpFooter()
}

func (m *Model) renderPrefix() string {
	// Also refresh here so a caller that builds a transcript directly gets a view.
	m.syncViewport()
	header := lipgloss.NewStyle().MaxWidth(m.textWidth()).MaxHeight(1).Render(styles.header.Render(" bonnie chat · " + m.address + " "))
	status := lipgloss.NewStyle().MaxWidth(m.textWidth()).MaxHeight(1).Render(m.statusLine())
	return header + "\n" + m.viewport.View() + "\n" + status + "\n"
}

// assistantText renders one assistant entry as markdown. The open streaming
// entry re-renders on every frame — its text is still growing, so any cache
// would go stale on the next chunk. A closed entry renders once per text and
// caches the result on the entry, because transcript() rebuilds the whole
// transcript every frame and herald parsing must not run per frame for a
// transcript that no longer changes.
//
// The render is not wrapped (width 0). [Model.syncViewport] wraps the whole
// transcript and records the soft breaks; a break made here would look the
// same but copy as a hard newline in the middle of a sentence.
func (m *Model) assistantText(i int) string {
	e := &m.entries[i]
	if e.streamed {
		return renderMarkdown(e.text, 0)
	}
	if e.rendered == "" {
		e.rendered = renderMarkdown(e.text, 0)
		m.entries[i] = *e
	}
	return e.rendered
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

// transcript renders every entry, styled by kind. Assistant entries render
// as markdown; the open streaming entry re-renders per frame, closed entries
// come from a per-width cache. See [Model.assistantText].
func (m *Model) transcript() string {
	var b strings.Builder
	for i := range m.entries {
		e := m.entries[i]
		if i > 0 {
			b.WriteString("\n")
		}
		switch e.kind {
		case kindUser:
			b.WriteString(styles.user.Render("❯ " + e.text))
		case kindAssistant:
			b.WriteString(m.assistantText(i))
		case kindQuestion:
			b.WriteString(styles.question.Render("◇ " + e.text))
		case kindTool:
			b.WriteString(renderTool(e, m.frame))
		case kindReasoning:
			b.WriteString(styles.reasoning.Render(e.text))
		case kindError:
			b.WriteString(styles.err.Render(e.text))
		case kindLog:
			b.WriteString(styles.log.Render(e.text))
		default:
			b.WriteString(styles.status.Render(e.text))
		}
	}
	return b.String()
}

func (m Model) helpFooter() string {
	return "\n" + lipgloss.NewStyle().MaxWidth(m.textWidth()).MaxHeight(1).Render("/help · pgup/pgdown scroll · drag to copy · ctrl+c exit") + "\n"
}

func renderTool(e entry, frame int) string {
	marker := "✓"
	if e.streamed {
		marker = spinnerFrames[frame%len(spinnerFrames)]
	}
	name, args := splitToolLine(e.text)
	line := styles.toolMarker.Render(marker) + " " + styles.toolName.Render(name)
	if args != "" {
		line += styles.toolArgs.Render(args)
	}
	if e.toolDone {
		line += "\n  " + styles.toolResultMark.Render("→") + " " + styles.toolResult.Render(e.toolResult)
	}
	if e.toolError {
		return styles.err.Render(line)
	}
	return line
}

func splitToolLine(line string) (string, string) {
	if i := strings.IndexByte(line, '('); i >= 0 {
		return line[:i], line[i:]
	}
	return line, ""
}

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
