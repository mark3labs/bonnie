// Package runsui is the operator console for durable runs: a terminal
// interface that lists the runs in a journal, shows each one as an overview,
// a conversation, a trace, and raw journal records, and sends operator
// actions to a running server.
//
// Everything the console shows comes from the journal, so it works against
// a stopped server — which is when an operator most needs it. Actions that
// change a run (answer, send, cancel, retire, clear, compact) go over the
// HTTP channel, the same wire every other client uses; without a reachable
// server the console is read only.
//
// The data side of the package (this file and trace.go) is pure: it turns
// []runtime.Record into summaries and spans, and the CLI's plain-output
// commands use it as well, so the terminal and the scripts agree.
package runsui

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/wire"
)

// Record kinds that the runtime declares outside runtime/journal.go. The
// console reads them by name; a journal from an older release that never
// wrote them still reads correctly.
const (
	kindToolIntent  runtime.RecordKind = "tool_intent"
	kindToolOutcome runtime.RecordKind = "tool_outcome"
	kindCompletion  runtime.RecordKind = "completion"
)

// Run is the operator's summary of one run. Records is the run's full
// journal, kept so that a detail view needs no second read.
type Run struct {
	ID      string
	Title   string
	State   runtime.RunState
	Origin  string
	Caller  string
	Sandbox string
	Model   string

	Created time.Time
	Updated time.Time

	Turns      []Turn
	Steps      int
	ToolCalls  int
	ToolErrors int
	Messages   int

	// Last is the text of the last message or suspension, for listings.
	Last string
	// Suspend is the open question when State is waiting.
	Suspend *wire.SuspendRequest
	// ToolUse counts calls per tool name.
	ToolUse map[string]int

	Records []runtime.Record
}

// Name is the title when the run has one, and the ID when it does not.
func (r Run) Name() string {
	if r.Title != "" {
		return r.Title
	}
	return r.ID
}

// TraceID is the trace of the run's latest turn, or empty.
func (r Run) TraceID() string {
	for _, t := range slices.Backward(r.Turns) {
		if t.TraceID != "" {
			return t.TraceID
		}
	}
	return ""
}

// Active reports whether the run can still change without an operator.
func (r Run) Active() bool {
	switch r.State {
	case runtime.RunPending, runtime.RunRunning, runtime.RunWaiting, runtime.RunInterrupted:
		return true
	}
	return false
}

// Turn is one turn of a run: the records from one RecordTurn up to the next.
type Turn struct {
	// Index counts from 1.
	Index int
	ID    string
	// TraceID and SpanID come from the W3C carrier journalled with the
	// turn. A turn from before tracing has neither.
	TraceID string
	SpanID  string

	Start time.Time
	// End is zero while the turn is still open.
	End   time.Time
	State runtime.RunState

	// Prompt is the first user message of the turn, or the answer that
	// resumed it.
	Prompt     string
	Steps      int
	ToolCalls  int
	ToolErrors int

	// First and Last index the run's Records slice, inclusive.
	First, Last int
}

// Duration is End-Start, or now-Start for an open turn.
func (t Turn) Duration(now time.Time) time.Duration {
	end := t.End
	if end.IsZero() {
		end = now
	}
	if end.Before(t.Start) {
		return 0
	}
	return end.Sub(t.Start)
}

// Open reports whether the turn has not reached a resting state.
func (t Turn) Open() bool { return t.End.IsZero() }

// Summarize builds a [Run] from its journal. state is the journal's own
// answer for the run, which wins over the last state record.
func Summarize(id string, state runtime.RunState, recs []runtime.Record) Run {
	r := Run{ID: id, State: state, Records: recs, ToolUse: map[string]int{}}
	if len(recs) > 0 {
		r.Created = recs[0].Timestamp
		r.Updated = recs[len(recs)-1].Timestamp
	}
	var open *Turn
	// A failed call can be marked by its outcome record, by the tool
	// message's error result, or both. Count each call once.
	failed := map[string]bool{}
	toolFailed := func(id string) {
		if failed[id] {
			return
		}
		failed[id] = true
		r.ToolErrors++
		if open != nil {
			open.ToolErrors++
		}
	}
	closeTurn := func(i int) {
		if open == nil {
			return
		}
		open.Last = i
		r.Turns = append(r.Turns, *open)
		open = nil
	}
	for i, rec := range recs {
		switch rec.Kind {
		case runtime.RecordTurn:
			closeTurn(i - 1)
			t := Turn{Index: len(r.Turns) + 1, ID: rec.Text, Start: rec.Timestamp, First: i, State: runtime.RunRunning}
			t.TraceID, t.SpanID = traceOf(rec.Payload)
			open = &t
		case runtime.RecordState:
			if open != nil {
				open.State = rec.State
				if rec.State != runtime.RunRunning && rec.State != runtime.RunPending {
					open.End = rec.Timestamp
				} else {
					open.End = time.Time{}
				}
			}
		case runtime.RecordStep:
			r.Steps++
			if open != nil {
				open.Steps++
			}
		case kindToolIntent:
			var in struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(rec.Payload, &in) == nil && in.Name != "" {
				r.ToolUse[in.Name]++
			}
			r.ToolCalls++
			if open != nil {
				open.ToolCalls++
			}
		case kindToolOutcome:
			var out struct {
				ID      string `json:"id"`
				IsError bool   `json:"is_error"`
			}
			if json.Unmarshal(rec.Payload, &out) == nil && out.IsError {
				toolFailed(out.ID)
			}
		case runtime.RecordMessage:
			r.Messages++
			text := rec.Text
			if text == "" {
				text = messageSummary(rec.Payload)
			}
			if text != "" && rec.Role != "tool" {
				r.Last = text
			}
			if open != nil && open.Prompt == "" && rec.Role == "user" {
				open.Prompt = rec.Text
			}
			if rec.Role == "tool" {
				for _, id := range failedResults(rec.Payload) {
					toolFailed(id)
				}
			}
		case runtime.RecordSuspend:
			if rec.Text != "" {
				r.Last = rec.Text
			}
		case runtime.RecordResume:
			if open != nil && open.Prompt == "" {
				open.Prompt = rec.Text
			}
		case runtime.RecordExtensionData:
			switch rec.ExtType {
			case runtime.ExtTitle:
				r.Title = rec.Text
			case runtime.ExtOrigin:
				r.Origin = originLabel(rec.Text)
			}
		case runtime.RecordSandbox:
			var sb struct {
				Backend string `json:"backend"`
				ID      string `json:"sandbox_id"`
			}
			if json.Unmarshal(rec.Payload, &sb) == nil && sb.Backend != "" {
				r.Sandbox = sb.Backend
				if sb.ID != "" && sb.ID != id {
					r.Sandbox += " " + sb.ID
				}
			}
		case runtime.RecordCaller:
			r.Caller = callerLabel(rec.Payload)
		case runtime.RecordModelChange:
			r.Model = modelLabel(rec)
		}
	}
	closeTurn(len(recs) - 1)
	if state == runtime.RunWaiting {
		r.Suspend = openSuspension(recs)
	}
	// The run's state is the authority for the last turn: a crash can leave
	// a turn without its closing state record.
	if n := len(r.Turns); n > 0 && !r.Active() && r.Turns[n-1].Open() {
		r.Turns[n-1].End = r.Updated
		r.Turns[n-1].State = state
	}
	return r
}

// traceOf reads the trace and span IDs from a turn payload's traceparent.
func traceOf(payload json.RawMessage) (traceID, spanID string) {
	var p struct {
		Trace map[string]string `json:"trace"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &p) != nil {
		return "", ""
	}
	// traceparent is version-traceid-spanid-flags.
	parts := strings.Split(p.Trace["traceparent"], "-")
	if len(parts) != 4 || len(parts[1]) != 32 || len(parts[2]) != 16 {
		return "", ""
	}
	return parts[1], parts[2]
}

// openSuspension returns the last suspension that no resume has answered.
func openSuspension(recs []runtime.Record) *wire.SuspendRequest {
	for _, rec := range slices.Backward(recs) {
		switch rec.Kind {
		case runtime.RecordResume:
			return nil
		case runtime.RecordSuspend:
			req := suspendOf(rec)
			return &req
		}
	}
	return nil
}

// suspendOf decodes a suspension record. A record without a payload is
// still a question: its Text is the prompt.
func suspendOf(rec runtime.Record) wire.SuspendRequest {
	var req wire.SuspendRequest
	if len(rec.Payload) == 0 || json.Unmarshal(rec.Payload, &req) != nil {
		req = wire.SuspendRequest{}
	}
	if req.Kind == "" {
		req.Kind = wire.SuspendQuestion
	}
	if req.Prompt == "" {
		req.Prompt = rec.Text
	}
	return req
}

func originLabel(text string) string {
	var o struct {
		Channel string `json:"channel"`
		Kind    string `json:"kind"`
	}
	if json.Unmarshal([]byte(text), &o) != nil {
		return text
	}
	switch {
	case o.Channel != "" && o.Kind != "":
		return o.Channel + "/" + o.Kind
	case o.Channel != "":
		return o.Channel
	default:
		return o.Kind
	}
}

func callerLabel(payload json.RawMessage) string {
	var c runtime.Caller
	if len(payload) == 0 || string(payload) == "null" || json.Unmarshal(payload, &c) != nil || c.ID == "" {
		return ""
	}
	label := c.ID
	if c.Authenticator != "" {
		label = c.Authenticator + ":" + label
	}
	if c.Verified {
		label += " ✓"
	}
	return label
}

func modelLabel(rec runtime.Record) string {
	var m struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if json.Unmarshal(rec.Payload, &m) == nil && m.Model != "" {
		if m.Provider != "" {
			return m.Provider + "/" + m.Model
		}
		return m.Model
	}
	return rec.Text
}

// decodeMessage decodes a message record's lossless payload.
func decodeMessage(payload json.RawMessage) (kit.LLMMessage, bool) {
	var msg kit.LLMMessage
	if len(payload) == 0 || json.Unmarshal(payload, &msg) != nil {
		return msg, false
	}
	return msg, true
}

// failedResults returns the call IDs of a tool message's error results.
func failedResults(payload json.RawMessage) []string {
	msg, ok := decodeMessage(payload)
	if !ok {
		return nil
	}
	var ids []string
	for _, part := range msg.Content {
		if res, ok := part.(kit.LLMToolResultPart); ok {
			if _, isErr := res.Output.(kit.LLMToolResultOutputContentError); isErr {
				ids = append(ids, res.ToolCallID)
			}
		}
	}
	return ids
}

// messageSummary describes a message that has no text: the tools an
// assistant called, or the results a tool message carries.
func messageSummary(payload json.RawMessage) string {
	msg, ok := decodeMessage(payload)
	if !ok {
		return ""
	}
	var names []string
	results, failures := 0, 0
	for _, part := range msg.Content {
		switch p := part.(type) {
		case kit.LLMToolCallPart:
			names = append(names, p.ToolName)
		case kit.LLMToolResultPart:
			results++
			if _, isErr := p.Output.(kit.LLMToolResultOutputContentError); isErr {
				failures++
			}
		}
	}
	switch {
	case len(names) > 0:
		return "called " + strings.Join(names, ", ")
	case results == 1 && failures == 1:
		return "1 tool result, failed"
	case results == 1:
		return "1 tool result"
	case results > 0 && failures > 0:
		return fmt.Sprintf("%d tool results, %d failed", results, failures)
	case results > 0:
		return fmt.Sprintf("%d tool results", results)
	}
	return ""
}

// SortKey orders runs in a listing.
type SortKey int

// Sort orders.
const (
	SortUpdated SortKey = iota
	SortCreated
	SortState
	SortName
	sortKeys
)

func (k SortKey) String() string {
	return [...]string{"updated", "created", "state", "name"}[k]
}

// stateRank puts the runs that need an operator first.
func stateRank(s runtime.RunState) int {
	switch s {
	case runtime.RunWaiting:
		return 0
	case runtime.RunRunning:
		return 1
	case runtime.RunPending:
		return 2
	case runtime.RunInterrupted:
		return 3
	case runtime.RunFailed:
		return 4
	case runtime.RunCancelled:
		return 5
	case runtime.RunCompleted:
		return 6
	default:
		return 7
	}
}

// SortRuns orders runs in place. Ties keep the ID order, so a refresh
// never shuffles equal rows.
func SortRuns(runs []Run, key SortKey) {
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		switch key {
		case SortCreated:
			if !a.Created.Equal(b.Created) {
				return a.Created.After(b.Created)
			}
		case SortState:
			if ra, rb := stateRank(a.State), stateRank(b.State); ra != rb {
				return ra < rb
			}
			if !a.Updated.Equal(b.Updated) {
				return a.Updated.After(b.Updated)
			}
		case SortName:
			if an, bn := strings.ToLower(a.Name()), strings.ToLower(b.Name()); an != bn {
				return an < bn
			}
		default:
			if !a.Updated.Equal(b.Updated) {
				return a.Updated.After(b.Updated)
			}
		}
		return a.ID < b.ID
	})
}

// StateFilter narrows a listing to a group of states.
type StateFilter int

// State filters.
const (
	FilterAll StateFilter = iota
	FilterActive
	FilterWaiting
	FilterFailed
	FilterDone
	stateFilters
)

func (f StateFilter) String() string {
	return [...]string{"all", "active", "waiting", "failed", "done"}[f]
}

// Match reports whether a run passes the filter.
func (f StateFilter) Match(r Run) bool {
	switch f {
	case FilterActive:
		return r.Active()
	case FilterWaiting:
		return r.State == runtime.RunWaiting
	case FilterFailed:
		return r.State == runtime.RunFailed || r.State == runtime.RunInterrupted || r.State == runtime.RunCancelled
	case FilterDone:
		return r.State == runtime.RunCompleted || r.State == runtime.RunRetired
	default:
		return true
	}
}

// matchText reports whether every word of query appears in the run's ID,
// title, state, origin, or last message. Case does not matter.
func matchText(r Run, query string) bool {
	hay := strings.ToLower(strings.Join([]string{r.ID, r.Title, string(r.State), r.Origin, r.Last, r.Caller}, " "))
	for word := range strings.FieldsSeq(strings.ToLower(query)) {
		if !strings.Contains(hay, word) {
			return false
		}
	}
	return true
}
