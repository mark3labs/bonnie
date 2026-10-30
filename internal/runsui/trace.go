package runsui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// SpanKind classifies a [Span]. The names follow the spans that package
// observe exports, so the console and a tracing backend tell one story.
type SpanKind string

// Span kinds.
const (
	SpanAgent   SpanKind = "invoke_agent"
	SpanChat    SpanKind = "chat"
	SpanTool    SpanKind = "execute_tool"
	SpanSuspend SpanKind = "suspend"
	SpanEvent   SpanKind = "event"
)

// SpanStatus is the outcome of a span.
type SpanStatus string

// Span outcomes.
const (
	StatusOK      SpanStatus = "ok"
	StatusError   SpanStatus = "error"
	StatusUnknown SpanStatus = "unknown"
	StatusOpen    SpanStatus = "open"
)

// Attr is one key and value of a span's detail.
type Attr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Span is one bar of a turn's waterfall. The console rebuilds spans from
// journal timestamps, so they show what the journal saw: a chat span ends
// when its first tool starts or its step is checkpointed, and a tool span
// runs from its intent record to its outcome.
type Span struct {
	Name   string     `json:"name"`
	Kind   SpanKind   `json:"kind"`
	Depth  int        `json:"depth"`
	Start  time.Time  `json:"start"`
	End    time.Time  `json:"end"`
	Status SpanStatus `json:"status"`
	Attrs  []Attr     `json:"attrs,omitempty"`
	// Seq is the journal record that opened the span.
	Seq int `json:"seq"`
}

// Instant reports whether the span is a point event, not an interval.
func (s Span) Instant() bool { return s.Kind == SpanEvent }

// Duration is End-Start.
func (s Span) Duration() time.Duration {
	if s.End.Before(s.Start) {
		return 0
	}
	return s.End.Sub(s.Start)
}

// Trace is the span tree of one turn, flattened in display order.
type Trace struct {
	Turn    Turn      `json:"turn"`
	TraceID string    `json:"trace_id,omitempty"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Spans   []Span    `json:"spans"`
}

// Duration is the length of the whole turn.
func (t Trace) Duration() time.Duration {
	if t.End.Before(t.Start) {
		return 0
	}
	return t.End.Sub(t.Start)
}

// BuildTrace rebuilds the spans of one turn of r. An open turn ends at now.
func BuildTrace(r Run, turn Turn, now time.Time) Trace {
	end := turn.End
	if end.IsZero() {
		end = now
	}
	if end.Before(turn.Start) {
		end = turn.Start
	}
	tr := Trace{Turn: turn, TraceID: turn.TraceID, Start: turn.Start, End: end}

	rootStatus := StatusOK
	switch turn.State {
	case runtime.RunFailed, runtime.RunInterrupted, runtime.RunCancelled:
		rootStatus = StatusError
	}
	if turn.Open() {
		rootStatus = StatusOpen
	}
	root := Span{
		Name: string(SpanAgent), Kind: SpanAgent, Start: turn.Start, End: end,
		Status: rootStatus, Seq: r.Records[turn.First].Seq,
		Attrs: []Attr{{"turn.id", turn.ID}, {"turn.state", string(turn.State)}},
	}
	if turn.TraceID != "" {
		root.Attrs = append(root.Attrs, Attr{"trace.id", turn.TraceID}, Attr{"span.id", turn.SpanID})
	}
	if turn.Prompt != "" {
		root.Attrs = append(root.Attrs, Attr{"input", turn.Prompt})
	}
	spans := []Span{root}

	// The open chat span, the tool spans that belong to it, and where the
	// next step begins.
	var (
		chat    *Span
		tools   []Span
		toolAt  = map[string]int{}
		cursor  = turn.Start
		step    = 0
		suspend = -1
	)
	openChat := func(seq int) {
		if chat != nil {
			return
		}
		step++
		chat = &Span{
			Name: fmt.Sprintf("chat · step %d", step), Kind: SpanChat, Depth: 1,
			Start: cursor, End: end, Status: StatusOpen, Seq: seq,
		}
	}
	flush := func(at time.Time, finish string) {
		if chat == nil {
			return
		}
		if chat.Status == StatusOpen && !at.IsZero() {
			chat.End = at
			chat.Status = StatusOK
		}
		if finish != "" {
			chat.Attrs = append([]Attr{{"finish_reason", finish}}, chat.Attrs...)
		}
		spans = append(spans, *chat)
		for _, t := range tools {
			if t.Status == StatusOpen && !turn.Open() {
				// An intent with no outcome in a closed turn: the process
				// stopped mid-call, and the call's effect is unknown.
				t.End = end
				t.Status = StatusUnknown
				t.Attrs = append(t.Attrs, Attr{"outcome", "unknown: no outcome was journalled"})
			}
			spans = append(spans, t)
		}
		chat, tools = nil, nil
		toolAt = map[string]int{}
	}

	for i := turn.First + 1; i <= turn.Last && i < len(r.Records); i++ {
		rec := r.Records[i]
		switch rec.Kind {
		case kindToolIntent:
			var in struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Args string `json:"args"`
				Safe bool   `json:"safe"`
			}
			_ = json.Unmarshal(rec.Payload, &in)
			openChat(rec.Seq)
			if chat.Status == StatusOpen && len(tools) == 0 {
				chat.End = rec.Timestamp
				chat.Status = StatusOK
			}
			span := Span{
				Name: string(SpanTool) + " " + in.Name, Kind: SpanTool, Depth: 2,
				Start: rec.Timestamp, End: end, Status: StatusOpen, Seq: rec.Seq,
				Attrs: []Attr{{"tool.name", in.Name}, {"tool.call_id", in.ID}},
			}
			if in.Safe {
				span.Attrs = append(span.Attrs, Attr{"tool.replay_safe", "true"})
			}
			if in.Args != "" {
				span.Attrs = append(span.Attrs, Attr{"tool.arguments", in.Args})
			}
			toolAt[in.ID] = len(tools)
			tools = append(tools, span)
		case kindToolOutcome:
			var out struct {
				ID      string `json:"id"`
				Result  string `json:"result"`
				IsError bool   `json:"is_error"`
				Unknown bool   `json:"unknown"`
			}
			_ = json.Unmarshal(rec.Payload, &out)
			j, ok := toolAt[out.ID]
			if !ok {
				continue
			}
			t := &tools[j]
			t.End = rec.Timestamp
			t.Status = StatusOK
			switch {
			case out.Unknown:
				t.Status = StatusUnknown
			case out.IsError:
				t.Status = StatusError
			}
			t.Attrs = append(t.Attrs, Attr{"tool.result", out.Result})
		case runtime.RecordMessage:
			msg, ok := decodeMessage(rec.Payload)
			if !ok {
				continue
			}
			switch rec.Role {
			case "assistant":
				openChat(rec.Seq)
				for _, part := range msg.Content {
					switch p := part.(type) {
					case kit.LLMReasoningPart:
						if strings.TrimSpace(p.Text) != "" {
							chat.Attrs = append(chat.Attrs, Attr{"reasoning", p.Text})
						}
					case kit.LLMTextPart:
						if strings.TrimSpace(p.Text) != "" {
							chat.Attrs = append(chat.Attrs, Attr{"output", p.Text})
						}
					case kit.LLMToolCallPart:
						chat.Attrs = append(chat.Attrs, Attr{"tool_call", p.ToolName + " " + p.Input})
					}
				}
			case "tool":
				// A tool error that the outcome hook did not mark is still
				// an error result in the lossless message.
				for _, id := range failedResults(rec.Payload) {
					if j, ok := toolAt[id]; ok {
						tools[j].Status = StatusError
					}
				}
			}
		case runtime.RecordStep:
			openChat(rec.Seq)
			if model := runtime.StepModel(rec); model != "" {
				chat.Attrs = append([]Attr{{"model", model}}, chat.Attrs...)
			}
			flush(rec.Timestamp, rec.Text)
			cursor = rec.Timestamp
		case runtime.RecordSuspend:
			flush(rec.Timestamp, "")
			req := suspendOf(rec)
			s := Span{
				Name: "suspended · " + req.Kind, Kind: SpanSuspend, Depth: 1,
				Start: rec.Timestamp, End: end, Status: StatusOpen, Seq: rec.Seq,
				Attrs: []Attr{{"kind", req.Kind}, {"prompt", req.Prompt}},
			}
			if len(req.Options) > 0 {
				s.Attrs = append(s.Attrs, Attr{"options", strings.Join(req.Options, ", ")})
			}
			if !turn.Open() {
				s.Status = StatusOK
			}
			suspend = len(spans)
			spans = append(spans, s)
			cursor = rec.Timestamp
		case runtime.RecordResume:
			if suspend >= 0 {
				spans[suspend].End = rec.Timestamp
				spans[suspend].Status = StatusOK
				spans[suspend].Attrs = append(spans[suspend].Attrs, Attr{"answer", rec.Text})
				suspend = -1
			}
			spans = append(spans, event(rec, "resumed", Attr{"answer", rec.Text}))
			cursor = rec.Timestamp
		case runtime.RecordDecision:
			d, err := runtime.DecodeDecisionRecord(rec)
			if err != nil {
				continue
			}
			span := event(rec, "decide "+d.Model,
				Attr{"decision.key", d.Key}, Attr{"decision.source", d.Source},
				Attr{"decision.answers", decisionSummary(d)}, Attr{"decision.error", d.Error},
				Attr{"decision.input_tokens", fmt.Sprint(d.Usage.InputTokens)})
			if d.Error != "" {
				span.Status = StatusError
			}
			// A decision in a step follows the step's buffered chat and
			// tool spans; one made by a tool sits under that tool.
			switch _, inTool := toolAt[d.ToolCallID]; {
			case inTool:
				span.Depth = 3
				tools = append(tools, span)
			case chat != nil:
				span.Depth = 2
				tools = append(tools, span)
			default:
				spans = append(spans, span)
			}
		case runtime.RecordSandbox:
			spans = append(spans, event(rec, "sandbox", Attr{"detail", rec.Text}))
		case runtime.RecordCompaction:
			spans = append(spans, event(rec, "compaction", Attr{"detail", rec.Text}))
		case runtime.RecordModelChange:
			spans = append(spans, event(rec, "model change", Attr{"model", modelLabel(rec)}))
		case runtime.RecordRepair:
			spans = append(spans, event(rec, "torn-write repair", Attr{"detail", string(rec.Payload)}))
		case runtime.RecordCancel:
			spans = append(spans, event(rec, "cancel requested", Attr{"detail", rec.Text}))
		case runtime.RecordClear:
			spans = append(spans, event(rec, "context cleared"))
		case runtime.RecordState:
			if rec.State != runtime.RunRunning && rec.State != runtime.RunPending {
				flush(rec.Timestamp, "")
			}
		}
	}
	// A chat span still open at the end is the model call in flight, or the
	// call a crash interrupted.
	if chat != nil {
		if !turn.Open() {
			chat.Status = StatusUnknown
		}
		flush(time.Time{}, "")
	} else if rootStatus != StatusOK && end.Sub(cursor) > time.Second && suspend < 0 {
		// The turn ended badly after its last checkpoint, with no record
		// of what it was doing: a model call in flight, most likely. Show
		// the gap rather than leave the time unaccounted for.
		step++
		status := StatusUnknown
		if turn.Open() {
			status = StatusOpen
		}
		spans = append(spans, Span{
			Name: fmt.Sprintf("chat · step %d", step), Kind: SpanChat, Depth: 1,
			Start: cursor, End: end, Status: status, Seq: r.Records[turn.Last].Seq,
			Attrs: []Attr{{"note", "no step was journalled after this point"}},
		})
	}
	tr.Spans = spans
	return tr
}

// decisionSummary is one line of a decision's answers, for example
// "risky=0.80 team=billing urgency=high", or its error class.
func decisionSummary(d runtime.DecisionRecord) string {
	if d.Error != "" {
		return "error: " + d.Error
	}
	levels := map[string][]string{}
	for _, q := range d.Questions {
		levels[q.ID] = q.Levels
	}
	parts := make([]string, 0, len(d.Answers))
	for _, a := range d.Answers {
		var v string
		switch {
		case a.Refused:
			v = "refused"
		case a.Type == kit.DecisionBoolean:
			v = fmt.Sprintf("%.2f", a.Probability)
		case a.Type == kit.DecisionChoice:
			v = a.Choice
		case a.Type == kit.DecisionScore && a.Level >= 0 && a.Level < len(levels[a.ID]):
			v = levels[a.ID][a.Level]
		default:
			v = fmt.Sprintf("%.2f", a.Score)
		}
		parts = append(parts, a.ID+"="+v)
	}
	return strings.Join(parts, " ")
}

func event(rec runtime.Record, name string, attrs ...Attr) Span {
	var keep []Attr
	for _, a := range attrs {
		if a.Value != "" {
			keep = append(keep, a)
		}
	}
	return Span{Name: name, Kind: SpanEvent, Depth: 1, Start: rec.Timestamp, End: rec.Timestamp, Status: StatusOK, Seq: rec.Seq, Attrs: keep}
}
