package chat

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// The statuses every transport shows before the model reaches a tool. They
// are the two moments a person recognises: the message arrived, and the
// turn began.
const (
	// StatusThinking is shown from the moment the message is accepted
	// until the agent's first event. It says "received", nothing more.
	StatusThinking = "Thinking…"
	// StatusWorking is shown while a turn runs with nothing more telling
	// to say — between tool calls, and before the first one.
	StatusWorking = "Working…"
)

// StatusLimit caps an activity line. A status is read at a glance, so the
// limit is a glance, not a platform maximum.
const StatusLimit = 50

// ActivityFunc renders one activity status for an address. It is never
// called concurrently for one address, and the empty status is always last:
// a second message that steers into a running turn joins that turn's
// indicator instead of opening a second one.
//
// An empty status is the end of the turn: clear the indicator. The reply
// follows it, so a transport that posted a placeholder message deletes it
// here rather than leaving a stale "Working…" above the answer.
//
// A renderer must not block for long and must not panic: the turn's
// delivery waits behind the clear.
type ActivityFunc func(address, status string)

// WithActivity turns on the live activity indicator for a channel: while a
// turn runs, the adapter is told what the agent is doing — thinking,
// working, the reasoning it is following, the tool it is calling.
//
// The statuses come from Kit's own lifecycle events, forwarded by the
// runner ([runtime.Runner.Events]), so an indicator costs no extra model
// work and no journal record. Activity is observability: an event that
// arrives late, or not at all, changes a status line and nothing else.
func WithActivity(fn ActivityFunc) CoreOption {
	return func(c *Core) { c.activity = fn }
}

// watchActivity mirrors one run's live events onto the core's activity
// renderer until the returned stop function is called.
//
// One address carries one indicator. A message that lands mid-turn is
// steered into the turn already running, and that turn's watcher is already
// describing the same run — so the second dispatch gets a watcher that does
// nothing rather than a second status line racing the first.
//
// stop waits for the watching goroutine to return before it clears the
// indicator. Without that wait, a status still in flight could land after
// the clear and strand a placeholder above the answer for ever — the one
// failure this whole surface must not have, because the thread keeps it.
//
// The watcher is live-only by construction: it subscribes to the bus rather
// than to [runtime.Runner.StreamEvents], because a status line has nothing
// to catch up on — what the agent did two minutes ago is not what it is
// doing now. Events published before the watch began belong to an earlier
// turn on the same run, and are skipped for the same reason.
func watchActivity(core *Core, address, runID string) func() {
	render := core.activity
	if render == nil || runID == "" || !core.claimActivity(address) {
		return func() {}
	}

	events, unsubscribe := core.runner.Events().Subscribe(runID, 0)
	start := time.Now()

	done := make(chan struct{})
	exited := make(chan struct{})
	a := &activity{render: func(status string) { render(address, status) }}
	a.show(StatusThinking)

	go func() {
		defer close(exited)
		for {
			select {
			case ev, open := <-events:
				if !open {
					return
				}
				if ev.Time.Before(start) {
					continue // an earlier turn on this run
				}
				a.apply(ev)
			case <-done:
				return
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-exited // no status can be in flight past this point
			unsubscribe()
			render(address, "")
			core.releaseActivity(address)
		})
	}
}

// claimActivity reports whether this caller now owns the address's
// indicator. It is the whole of the one-indicator-per-address rule.
func (c *Core) claimActivity(address string) bool {
	c.activityMu.Lock()
	defer c.activityMu.Unlock()
	if c.watching[address] {
		return false
	}
	if c.watching == nil {
		c.watching = make(map[string]bool)
	}
	c.watching[address] = true
	return true
}

// releaseActivity gives the address's indicator back.
func (c *Core) releaseActivity(address string) {
	c.activityMu.Lock()
	defer c.activityMu.Unlock()
	delete(c.watching, address)
}

// reasoningRefresh is the shortest gap between two status lines cut from
// the same block of reasoning. Reasoning streams a token at a time, and a
// chat platform rate-limits edits: without a floor, one thought costs one
// API call per token.
const reasoningRefresh = 5 * time.Second

// reasoningProgress is how many characters a reasoning line must gain
// before it refreshes ahead of [reasoningRefresh]. A line that is still
// growing reads as live; a line that jitters by a character reads as noise.
const reasoningProgress = 4

// activity reduces a run's live events to a single status line. It holds no
// lock: one watcher goroutine owns one activity.
type activity struct {
	render func(status string)
	last   string

	// calls are the tool calls in flight, oldest first. The oldest is the
	// one shown; the rest are the "+N more".
	calls []toolCall

	// reasoning accumulates the current block, so a status can be cut from
	// the whole first line rather than from one delta.
	reasoning  string
	shownAt    time.Time
	shownLine  string
	narration  string
	inProgress bool
}

// toolCall is one tool call the model asked for, as far as a status line
// cares about it.
type toolCall struct {
	id    string
	label string
}

// apply folds one event into the status line.
func (a *activity) apply(ev runtime.Event) {
	switch ev.Type {
	case runtime.EventState:
		// A turn that is no longer running has nothing to report. The
		// dispatch clears the indicator at the boundary; this keeps a
		// status from outliving the turn when it does not.
		if ev.State != runtime.RunRunning && ev.State != runtime.RunPending {
			a.reset()
		}

	case string(kit.EventTurnStart):
		a.reset()
		a.inProgress = true
		a.show(StatusWorking)

	case string(kit.EventReasoningDelta):
		var e kit.ReasoningDeltaEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			a.appendReasoning(e.Delta)
		}

	case string(kit.EventReasoningComplete):
		a.reasoning = ""
		a.shownLine = ""
		a.shownAt = time.Time{}

	case string(kit.EventMessageEnd):
		// The model's own words before it calls a tool are better than any
		// label derived from arguments: "Checking the deploy logs" says
		// why, where `read_file deploy.log` says only what. Kept until the
		// next tool call consumes it.
		var e kit.MessageEndEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			a.narration = FirstLine(strings.TrimSpace(e.Content))
		}

	case string(kit.EventToolCall):
		var e kit.ToolCallEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			a.startCall(e.ToolCallID, ToolLabel(e.ToolName, e.ParsedArgs, e.ToolArgs))
		}

	case string(kit.EventToolExecutionStart):
		var e kit.ToolExecutionStartEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			a.startCall(e.ToolCallID, ToolLabel(e.ToolName, nil, e.ToolArgs))
		}

	case string(kit.EventToolExecutionEnd):
		var e kit.ToolExecutionEndEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			a.endCall(e.ToolCallID)
		}

	case string(kit.EventToolResult):
		var e kit.ToolResultEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			a.endCall(e.ToolCallID)
		}
	}
}

// startCall records a tool call as in flight and shows it.
func (a *activity) startCall(id, label string) {
	if label == "" {
		return
	}
	for i, c := range a.calls {
		if c.id == id {
			// Execution follows the parsed call: the second event knows no
			// more than the first, so the better label wins.
			if len(label) > len(c.label) {
				a.calls[i].label = label
			}
			a.showCalls()
			return
		}
	}
	// The model's narration, when it wrote any, describes this call better
	// than its arguments do — and describes only this one.
	if a.narration != "" && len(a.calls) == 0 {
		label = a.narration
	}
	a.narration = ""
	a.calls = append(a.calls, toolCall{id: id, label: label})
	a.showCalls()
}

// endCall drops a finished call and falls back to what is left.
func (a *activity) endCall(id string) {
	for i, c := range a.calls {
		if c.id == id {
			a.calls = append(a.calls[:i:i], a.calls[i+1:]...)
			break
		}
	}
	if len(a.calls) == 0 {
		if a.inProgress {
			a.show(StatusWorking)
		}
		return
	}
	a.showCalls()
}

// showCalls renders the calls in flight: the oldest one, and how many
// others the model asked for in the same breath.
func (a *activity) showCalls() {
	if len(a.calls) == 0 {
		return
	}
	status := a.calls[0].label
	if n := len(a.calls) - 1; n > 0 {
		status += " +" + strconv.Itoa(n) + " more"
	}
	a.show(status)
}

// appendReasoning grows the current reasoning block and shows its first
// line, rate-limited: a line that is visibly growing refreshes at once, and
// anything else waits for [reasoningRefresh].
func (a *activity) appendReasoning(delta string) {
	if delta == "" || len(a.calls) > 0 {
		return // a tool in flight is the more specific thing to say
	}
	a.reasoning += delta
	line := FirstLine(strings.TrimSpace(a.reasoning))
	if line == "" {
		return
	}
	status := TruncateStatus(line)
	grew := a.shownLine != "" &&
		strings.HasPrefix(status, a.shownLine) &&
		len(status) >= len(a.shownLine)+reasoningProgress
	if !grew && !a.shownAt.IsZero() && time.Since(a.shownAt) < reasoningRefresh {
		return
	}
	a.shownAt = time.Now()
	a.shownLine = status
	a.show(status)
}

// reset forgets a turn's accumulated state, so the next turn on the same
// run does not inherit a stale tool or half a thought.
func (a *activity) reset() {
	a.calls = nil
	a.reasoning = ""
	a.shownLine = ""
	a.narration = ""
	a.shownAt = time.Time{}
	a.inProgress = false
}

// show renders a status unless it is the one already on screen. Every
// transport pays an API call per status, and a repeated line buys nothing.
func (a *activity) show(status string) {
	status = TruncateStatus(status)
	if status == "" || status == a.last {
		return
	}
	a.last = status
	a.render(status)
}

// salientKeys names the argument worth showing for the tools BONNIE and Kit
// ship, most telling first. A tool that is not listed is probed with
// [genericKeys].
var salientKeys = map[string][]string{
	// BONNIE's sandbox tools.
	"bash":       {"command"},
	"read_file":  {"path"},
	"write_file": {"path"},
	"list_files": {"path"},
	// Kit's core tools.
	"shell":    {"command"},
	"read":     {"path"},
	"write":    {"path"},
	"edit":     {"path"},
	"grep":     {"pattern", "path"},
	"find":     {"pattern", "path"},
	"ls":       {"path"},
	"subagent": {"task", "agent"},
	// BONNIE's human-in-the-loop tools.
	"ask_human":        {"question"},
	"request_approval": {"action"},
}

// genericKeys are probed on a tool with no [salientKeys] entry — an MCP
// tool, or one the host registered. They are the argument names that carry
// the subject of a call across most tool vocabularies.
var genericKeys = []string{
	"path", "file_path", "filePath", "command", "pattern",
	"query", "url", "name", "task", "question", "repo", "id",
}

// ToolLabel names one tool call for a person watching: the tool's name and
// its most telling argument — `grep ErrRunNotFound`, `read_file runner.go`
// — rather than the tool's name alone, which says the agent is busy but not
// with what.
//
// parsed is Kit's pre-parsed arguments when it has them; raw is the
// JSON-encoded arguments, decoded only when parsed is nil. Arguments that
// cannot be read at all leave the name standing on its own.
func ToolLabel(name string, parsed map[string]any, raw string) string {
	if name == "" {
		return ""
	}
	if parsed == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &parsed)
	}
	arg := salientArg(name, parsed)
	if arg == "" {
		return name
	}
	return name + " " + shortenArg(arg)
}

// salientArg picks the argument to show, by the tool's own list when it has
// one and by [genericKeys] otherwise.
func salientArg(name string, args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys, ok := salientKeys[name]
	if !ok {
		keys = genericKeys
	}
	for _, key := range keys {
		switch v := args[key].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			// A JSON number is a float; an issue number reads as 42,
			// never as 42.000000.
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}

// argChars caps the argument half of a label, so a long path or a long
// command cannot push the tool's own name off the line.
const argChars = 40

// shortenArg makes one line of an argument and keeps its telling end: a
// path shows its last two segments, because `triage/cards.go` identifies a
// file where `agent/lib/triage/…` identifies a directory.
func shortenArg(text string) string {
	line := FirstLine(strings.TrimSpace(text))
	line = strings.Join(strings.Fields(line), " ")
	if len(line) <= argChars {
		return line
	}
	if strings.Contains(line, "/") && !strings.Contains(line, " ") {
		segments := strings.FieldsFunc(line, func(r rune) bool { return r == '/' })
		if len(segments) >= 2 {
			tail := strings.Join(segments[len(segments)-2:], "/")
			if len(tail) <= argChars {
				return tail
			}
		}
	}
	return truncateRunes(line, argChars)
}

// TruncateStatus normalises a status line: one line, single-spaced, capped
// at [StatusLimit]. A transport calls it on anything it renders, so a tool
// name from an MCP server cannot overrun a platform's status field.
func TruncateStatus(status string) string {
	status = strings.Join(strings.Fields(status), " ")
	return truncateRunes(status, StatusLimit)
}

// truncateRunes caps a string by runes, not bytes, and marks the cut. A
// byte cut splits a multi-byte character and the platform renders the
// wreckage.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return strings.TrimRight(string(runes[:limit-1]), " ") + "…"
}
