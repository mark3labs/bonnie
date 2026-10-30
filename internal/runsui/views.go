package runsui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"maps"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/internal/selection"
	"github.com/mark3labs/bonnie/internal/tui"
	"github.com/mark3labs/bonnie/runtime"
)

// runRow is one line of the runs panel.
func runRow(r Run, w int, spin string, now time.Time) string {
	right := sMuted.Render(ago(r.Updated, now))
	left := stateGlyph(r.State, spin) + " " + sText.Render(oneLine(r.Name(), w))
	if r.Title != "" && w > 48 {
		left += " " + sFaint.Render(r.ID)
	}
	return row(left, right, w)
}

// turnRow is one line of the turns panel.
func turnRow(t Turn, w int, spin string, now time.Time) string {
	left := stateGlyph(t.State, spin) + " " + sMuted.Render(fmt.Sprintf("#%-2d", t.Index)) + " " + sText.Render(oneLine(t.Prompt, w))
	if t.Prompt == "" {
		left = stateGlyph(t.State, spin) + " " + sMuted.Render(fmt.Sprintf("#%-2d %s", t.Index, t.Start.Local().Format("Jan 02 15:04:05")))
	}
	right := sMuted.Render(fmt.Sprintf("%d steps", t.Steps)) + " " + sCyan.Render(fmt.Sprintf("%6s", dur(t.Duration(now))))
	return row(left, right, w)
}

// overview is the main panel's first tab: what an operator needs to know
// about one run at a glance.
func overview(r Run, w int, now time.Time) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\n") }

	line(row(sBold.Render(oneLine(r.Name(), w)), stateBadge(r.State), w))
	var meta []string
	if r.Title != "" {
		meta = append(meta, r.ID)
	}
	if r.Origin != "" {
		meta = append(meta, r.Origin)
	}
	meta = append(meta, "created "+ago(r.Created, now)+" ago", "updated "+ago(r.Updated, now)+" ago")
	line(sMuted.Render(oneLine(strings.Join(meta, " · "), w)))
	line("")

	if r.Suspend != nil {
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorYellow).Padding(0, 1).Width(max(10, min(w, 90)))
		var q strings.Builder
		q.WriteString(sYellow.Bold(true).Render("◆ waiting for " + r.Suspend.Kind))
		q.WriteString("\n" + r.Suspend.Prompt)
		if len(r.Suspend.Options) > 0 {
			q.WriteString("\n" + sMuted.Render("options: ") + strings.Join(r.Suspend.Options, sMuted.Render(" · ")))
		}
		q.WriteString("\n" + sMuted.Render("press ") + sKey.Render("a") + sMuted.Render(" to answer"))
		line(box.Render(q.String()))
		line("")
	}

	stat := func(label string, value string) string {
		return sMuted.Render(label+" ") + sBold.Render(value)
	}
	var last time.Duration
	var total time.Duration
	for _, t := range r.Turns {
		total += t.Duration(now)
		last = t.Duration(now)
	}
	stats := []string{
		stat("turns", fmt.Sprint(len(r.Turns))),
		stat("steps", fmt.Sprint(r.Steps)),
		stat("tool calls", fmt.Sprint(r.ToolCalls)),
		stat("messages", fmt.Sprint(r.Messages)),
		stat("records", fmt.Sprint(len(r.Records))),
	}
	if r.ToolErrors > 0 {
		noun := "tool errors"
		if r.ToolErrors == 1 {
			noun = "tool error"
		}
		stats = append(stats, sRed.Render(fmt.Sprintf("%d %s", r.ToolErrors, noun)))
	}
	line(wrapJoin(stats, "   ", w))
	line(wrapJoin([]string{stat("busy", dur(total)), stat("last turn", dur(last))}, "   ", w))
	line("")

	kv := func(k, v string) {
		if v != "" {
			line(sLabel.Render(k) + oneLine(v, max(1, w-12)))
		}
	}
	kv("trace", r.TraceID())
	kv("sandbox", r.Sandbox)
	kv("caller", r.Caller)
	kv("model", r.Model)
	kv("last", r.Last)

	if len(r.ToolUse) > 0 {
		line("")
		line(sAccent.Bold(true).Render("Tool usage"))
		names := slices.Collect(maps.Keys(r.ToolUse))
		slices.SortFunc(names, func(a, b string) int {
			if d := r.ToolUse[b] - r.ToolUse[a]; d != 0 {
				return d
			}
			return strings.Compare(a, b)
		})
		top := r.ToolUse[names[0]]
		nameW := 4
		for _, n := range names {
			nameW = max(nameW, min(24, ansi.StringWidth(n)))
		}
		barW := max(4, min(48, w-nameW-8))
		for _, n := range names[:min(len(names), 12)] {
			cells := max(1, r.ToolUse[n]*barW/top)
			line(fit(sText.Render(n), nameW) + " " + sGreen.Render(strings.Repeat("█", cells)) + " " + sMuted.Render(fmt.Sprint(r.ToolUse[n])))
		}
		if len(names) > 12 {
			line(sMuted.Render(fmt.Sprintf("… and %d more", len(names)-12)))
		}
	}

	if len(r.Turns) > 1 {
		line("")
		line(sAccent.Bold(true).Render("Turn durations"))
		line(sparkline(r.Turns, now, min(w, 60)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// sparkline draws one bar per turn, scaled to the longest turn.
func sparkline(turns []Turn, now time.Time, w int) string {
	if len(turns) > w {
		turns = turns[len(turns)-w:]
	}
	var top time.Duration
	for _, t := range turns {
		top = max(top, t.Duration(now))
	}
	ticks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, t := range turns {
		i := 0
		if top > 0 {
			i = int(float64(len(ticks)-1) * float64(t.Duration(now)) / float64(top))
		}
		b.WriteString(lipgloss.NewStyle().Foreground(stateColor(t.State)).Render(string(ticks[i])))
	}
	return b.String() + sMuted.Render("  max "+dur(top))
}

// wrapJoin joins parts with sep and starts a new line rather than cut one.
func wrapJoin(parts []string, sep string, w int) string {
	var lines []string
	cur := ""
	for _, p := range parts {
		switch {
		case cur == "":
			cur = p
		case ansi.StringWidth(cur+sep+p) > w:
			lines = append(lines, cur)
			cur = p
		default:
			cur += sep + p
		}
	}
	return strings.Join(append(lines, cur), "\n")
}

// conversationOptions controls the conversation tab.
type conversationOptions struct {
	reasoning bool // show reasoning in full, not one line
	toolLines int  // lines of each tool result to keep
}

// conversation renders the run's messages as a transcript: user and
// assistant turns, each tool call with its result, suspensions, answers,
// and the boundaries between turns.
//
// The rows carry what a mouse selection needs to copy them back as text:
// each block's bar is a margin the copy skips, and a paragraph that wraps to
// the width copies as the one line it is. Thus the text is wrapped here,
// with [selection.Wrap], and not by lipgloss or the markdown render.
func conversation(r Run, w int, opt conversationOptions) selection.Wrapped {
	var out selection.Wrapped
	results := map[string]kit.LLMToolResultPart{}
	for _, rec := range r.Records {
		if rec.Kind != runtime.RecordMessage || rec.Role != "tool" {
			continue
		}
		if msg, ok := decodeMessage(rec.Payload); ok {
			for _, part := range msg.Content {
				if res, ok := part.(kit.LLMToolResultPart); ok {
					results[res.ToolCallID] = res
				}
			}
		}
	}
	// Text in a bar wraps to the cells the bar and its gap leave.
	inner := max(4, w-3)
	wrap := func(text string) selection.Wrapped { return selection.Wrap(text, inner) }
	bar := func(c lipgloss.Style, body selection.Wrapped) {
		body.Indent(lipgloss.NewStyle().Foreground(c.GetForeground()).Render(lipgloss.ThickBorder().Left) + " ")
		out.Append(body)
		out.Append(selection.Lines(""))
	}
	line := func(s string) { out.Append(selection.Lines(s)) }
	divider := func(label string, c lipgloss.Style) {
		label = " " + label + " "
		side := max(2, (w-ansi.StringWidth(label))/2)
		line(sFaint.Render(strings.Repeat("─", side)) + c.Render(label) + sFaint.Render(strings.Repeat("─", max(0, w-side-ansi.StringWidth(label)))))
		line("")
	}
	turnState := map[int]Turn{}
	for _, t := range r.Turns {
		turnState[t.First] = t
	}

	for i, rec := range r.Records {
		if t, ok := turnState[i]; ok {
			label := fmt.Sprintf("turn %d · %s · %s", t.Index, t.Start.Local().Format("Jan 02 15:04:05"), t.State)
			divider(label, lipgloss.NewStyle().Foreground(stateColor(t.State)))
		}
		switch rec.Kind {
		case runtime.RecordMessage:
			msg, ok := decodeMessage(rec.Payload)
			if !ok {
				if rec.Text != "" {
					bar(sMuted, wrap(rec.Text))
				}
				continue
			}
			switch rec.Role {
			case "user":
				text := messageTextOf(msg)
				if text == "" {
					continue
				}
				line(sBlue.Bold(true).Render("▍you") + sFaint.Render("  "+rec.Timestamp.Local().Format("15:04:05")))
				bar(sBlue, wrap(text))
			case "assistant":
				var body selection.Wrapped
				for _, part := range msg.Content {
					switch p := part.(type) {
					case kit.LLMReasoningPart:
						text := strings.TrimSpace(p.Text)
						if text == "" {
							continue
						}
						var rows selection.Wrapped
						if !opt.reasoning {
							rows = selection.Lines("∴ " + oneLine(text, max(4, w-6)))
						} else {
							rows = wrap("∴ " + text)
						}
						for j := range rows.Rows {
							rows.Rows[j] = sReason.Render(rows.Rows[j])
						}
						body.Append(rows)
					case kit.LLMTextPart:
						if strings.TrimSpace(p.Text) != "" {
							body.Append(wrap(tui.RenderMarkdown(p.Text, 0)))
						}
					case kit.LLMToolCallPart:
						body.Append(selection.Lines(toolCall(p, results[p.ToolCallID], w-3, opt.toolLines)))
					}
				}
				if len(body.Rows) == 0 {
					continue
				}
				line(sAccent.Bold(true).Render("▍agent") + sFaint.Render("  "+rec.Timestamp.Local().Format("15:04:05")))
				bar(sAccent, body)
			}
		case runtime.RecordSuspend:
			req := suspendOf(rec)
			text := sYellow.Bold(true).Render("◆ "+req.Kind) + "  " + req.Prompt
			if len(req.Options) > 0 {
				text += "\n" + sMuted.Render("options: "+strings.Join(req.Options, " · "))
			}
			bar(sYellow, wrap(text))
		case runtime.RecordResume:
			bar(sGreen, wrap(sGreen.Bold(true).Render("▶ answered")+"  "+rec.Text))
		case runtime.RecordClear:
			divider("context cleared", sYellow)
		case runtime.RecordCompaction:
			divider("context compacted", sPurple)
		}
	}
	// Drop the blank rows after the last block.
	n := len(out.Rows)
	for n > 0 && out.Plain[n-1] == "" {
		n--
	}
	if n == 0 {
		return selection.Lines(sMuted.Render("No conversation yet."))
	}
	out.Rows, out.Plain, out.Joined = out.Rows[:n], out.Plain[:n], out.Joined[:n]
	if out.Margin != nil {
		out.Margin = out.Margin[:n]
	}
	return out
}

func messageTextOf(msg kit.LLMMessage) string {
	var parts []string
	for _, part := range msg.Content {
		if t, ok := part.(kit.LLMTextPart); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toolCall renders one call and the start of its result.
func toolCall(call kit.LLMToolCallPart, res kit.LLMToolResultPart, w, keep int) string {
	head := sGreen.Bold(true).Render("⚙ "+call.ToolName) + " " + sMuted.Render(oneLine(compactJSON(call.Input), max(4, w-ansi.StringWidth(call.ToolName)-3)))
	if res.ToolCallID == "" {
		return head + "\n" + sFaint.Render("  ⋯ no result journalled")
	}
	text, isErr := resultText(res.Output)
	mark, style := "  ↳ ", lipgloss.NewStyle().Foreground(lipgloss.Color("248"))
	if isErr {
		mark, style = "  ✗ ", sRed
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	more := 0
	if keep > 0 && len(lines) > keep {
		more = len(lines) - keep
		lines = lines[:keep]
	}
	var out []string
	for i, l := range lines {
		prefix := "    "
		if i == 0 {
			prefix = mark
		}
		out = append(out, style.Render(prefix+ansi.Truncate(strings.ReplaceAll(l, "\t", "  "), max(4, w-4), "…")))
	}
	if more > 0 {
		out = append(out, sFaint.Render(fmt.Sprintf("    … %d more lines", more)))
	}
	return head + "\n" + strings.Join(out, "\n")
}

func resultText(out kit.LLMToolResultOutputContent) (string, bool) {
	switch o := out.(type) {
	case kit.LLMToolResultOutputContentText:
		return o.Text, false
	case kit.LLMToolResultOutputContentError:
		if o.Error != nil {
			return o.Error.Error(), true
		}
		return "error", true
	case kit.LLMToolResultOutputContentMedia:
		return "[" + o.MediaType + " media]", false
	default:
		return "", false
	}
}

func compactJSON(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return s
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func prettyJSON(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	v = expandJSON(v)
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if enc.Encode(v) != nil {
		return string(raw)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// expandJSON replaces string values that hold a JSON object or array with
// the decoded value, so tool arguments read as structure, not escapes.
func expandJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = expandJSON(x)
		}
	case []any:
		for i, x := range t {
			t[i] = expandJSON(x)
		}
	case string:
		s := strings.TrimSpace(t)
		if len(s) > 1 && (s[0] == '{' || s[0] == '[') {
			var inner any
			if json.Unmarshal([]byte(s), &inner) == nil {
				return expandJSON(inner)
			}
		}
	}
	return v
}

// colorJSON tints keys and values of indented JSON. It works line by line,
// which is all MarshalIndent output needs.
func colorJSON(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		trim := strings.TrimLeft(l, " ")
		indent := l[:len(l)-len(trim)]
		if k, v, ok := strings.Cut(trim, `": `); ok && strings.HasPrefix(trim, `"`) {
			lines[i] = indent + sCyan.Render(k+`"`) + sMuted.Render(": ") + jsonValue(v)
			continue
		}
		lines[i] = indent + jsonValue(trim)
	}
	return strings.Join(lines, "\n")
}

func jsonValue(v string) string {
	core := strings.TrimSuffix(v, ",")
	tail := v[len(core):]
	switch {
	case strings.HasPrefix(core, `"`):
		return sGreen.Render(core) + tail
	case core == "true" || core == "false" || core == "null":
		return sPurple.Render(core) + tail
	case core == "{" || core == "}" || core == "[" || core == "]" || core == "{}" || core == "[]":
		return sMuted.Render(core) + tail
	case core != "" && (core[0] == '-' || (core[0] >= '0' && core[0] <= '9')):
		return sYellow.Render(core) + tail
	default:
		return v
	}
}

// waterfall draws a turn's spans as horizontal bars on a shared time axis.
// It returns one line per span, so the caller can place a cursor.
func waterfall(tr Trace, w int, cursor int, focused bool) (header string, lines []string) {
	labelW := min(34, max(16, w/3))
	durW := 8
	barW := max(8, w-labelW-durW-2)
	total := tr.Duration()
	if total <= 0 {
		total = time.Millisecond
	}
	col := func(t time.Time) int {
		c := int(float64(barW) * float64(t.Sub(tr.Start)) / float64(total))
		return min(max(c, 0), barW-1)
	}

	// The axis: four ticks with the offset from the turn start.
	axis := []rune(strings.Repeat(" ", barW))
	labels := strings.Repeat(" ", barW)
	for i := 0; i <= 4; i++ {
		c := min(barW-1, i*(barW-1)/4)
		axis[c] = '┊'
		tick := dur(time.Duration(float64(total) * float64(i) / 4))
		start := min(max(0, c-len(tick)/2), barW-len(tick))
		if start >= 0 {
			labels = labels[:start] + tick + labels[min(len(labels), start+len(tick)):]
		}
	}
	header = fit(sMuted.Render("span"), labelW) + " " + sMuted.Render(fit(labels, barW)) + " " + sMuted.Render(fmt.Sprintf("%*s", durW, "duration"))

	for i, sp := range tr.Spans {
		indent := strings.Repeat("  ", sp.Depth)
		icon := "▸ "
		switch sp.Kind {
		case SpanTool:
			icon = "⚙ "
		case SpanSuspend:
			icon = "◆ "
		case SpanEvent:
			icon = "· "
		case SpanAgent:
			icon = "● "
		}
		c := lipgloss.NewStyle().Foreground(spanColor(sp))
		label := indent + c.Render(icon) + sText.Render(sp.Name)
		if sp.Kind == SpanTool {
			label = indent + c.Render(icon) + sText.Render(strings.TrimPrefix(sp.Name, string(SpanTool)+" "))
		}

		bar := make([]string, barW)
		for j := range bar {
			bar[j] = sFaint.Render(string(axis[j]))
			if axis[j] == ' ' {
				bar[j] = " "
			}
		}
		from := col(sp.Start)
		if sp.Instant() {
			bar[from] = c.Render("◆")
		} else {
			to := max(from, col(sp.End))
			fill := "█"
			switch sp.Kind {
			case SpanSuspend:
				fill = "░"
			case SpanAgent:
				fill = "▬"
			}
			if sp.Status == StatusOpen {
				fill = "▓"
			}
			for j := from; j <= to; j++ {
				bar[j] = c.Render(fill)
			}
		}
		d := dur(sp.Duration())
		if sp.Instant() {
			d = "+" + dur(sp.Start.Sub(tr.Start))
		}
		dstyle := sMuted
		if sp.Status == StatusError {
			dstyle = sRed
		}
		out := fit(label, labelW) + " " + strings.Join(bar, "") + " " + dstyle.Render(fmt.Sprintf("%*s", durW, d))
		if i == cursor {
			out = highlight(out, w, focused)
		}
		lines = append(lines, out)
	}
	return header, lines
}

// spanDetail renders a span's attributes below the waterfall.
func spanDetail(sp Span, tr Trace, w int) string {
	var b strings.Builder
	c := lipgloss.NewStyle().Foreground(spanColor(sp)).Bold(true)
	b.WriteString(c.Render(sp.Name) + "  " + sMuted.Render(string(sp.Status)) + "\n")
	timing := fmt.Sprintf("start +%s · %s · journal #%d", dur(sp.Start.Sub(tr.Start)), sp.Start.Local().Format("15:04:05.000"), sp.Seq)
	if !sp.Instant() {
		timing = fmt.Sprintf("start +%s · duration %s · journal #%d", dur(sp.Start.Sub(tr.Start)), dur(sp.Duration()), sp.Seq)
	}
	b.WriteString(sMuted.Render(timing) + "\n")
	keyW := 0
	for _, a := range sp.Attrs {
		keyW = max(keyW, min(16, len(a.Key)))
	}
	for _, a := range sp.Attrs {
		v := a.Value
		if strings.HasPrefix(strings.TrimSpace(v), "{") {
			v = colorJSON(prettyJSON([]byte(v)))
		} else if a.Key == "reasoning" {
			v = sReason.Render(lipgloss.Wrap(v, max(4, w-2), ""))
		} else {
			v = lipgloss.Wrap(v, max(4, w-2), "")
		}
		if !strings.Contains(v, "\n") && keyW+ansi.StringWidth(v)+2 <= w {
			b.WriteString(sCyan.Render(fmt.Sprintf("%-*s", keyW, a.Key)) + "  " + v + "\n")
			continue
		}
		b.WriteString(sCyan.Render(a.Key) + "\n")
		for l := range strings.SplitSeq(v, "\n") {
			b.WriteString("  " + l + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// journalLine is one row of the journal tab.
func journalLine(rec runtime.Record, prev time.Time, w int) string {
	delta := ""
	if !prev.IsZero() {
		delta = "+" + dur(rec.Timestamp.Sub(prev))
	}
	kind := lipgloss.NewStyle().Foreground(kindColor(rec)).Render(fmt.Sprintf("%-14s", rec.Kind))
	head := sMuted.Render(fmt.Sprintf("%5d ", rec.Seq)) + sText.Render(rec.Timestamp.Local().Format("15:04:05.000")) + " " + sFaint.Render(fmt.Sprintf("%7s", delta)) + " " + kind + " "
	return head + oneLine(recordDetail(rec), max(1, w-ansi.StringWidth(head)))
}

func kindColor(rec runtime.Record) color.Color {
	switch rec.Kind {
	case runtime.RecordTurn:
		return colorAccent
	case runtime.RecordState:
		return stateColor(rec.State)
	case runtime.RecordMessage:
		return colorBlue
	case kindToolIntent, kindToolOutcome:
		return colorGreen
	case runtime.RecordStep:
		return colorCyan
	case runtime.RecordSuspend, runtime.RecordResume:
		return colorYellow
	case runtime.RecordDecision:
		return colorPurple
	default:
		return colorMuted
	}
}

// recordDetail is the human part of a record, one line.
func recordDetail(rec runtime.Record) string {
	switch rec.Kind {
	case runtime.RecordState:
		return string(rec.State)
	case runtime.RecordMessage:
		text := rec.Text
		if text == "" {
			text = messageSummary(rec.Payload)
		}
		if rec.Role != "" {
			return rec.Role + ": " + text
		}
		return text
	case runtime.RecordExtensionData:
		return rec.ExtType + ": " + rec.Text
	case runtime.RecordSuspend:
		req := suspendOf(rec)
		return req.Kind + ": " + req.Prompt
	case kindToolIntent:
		var in struct{ Name, Args string }
		if json.Unmarshal(rec.Payload, &in) == nil {
			return in.Name + " " + compactJSON(in.Args)
		}
	case kindToolOutcome:
		var out struct {
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal(rec.Payload, &out) == nil {
			if out.IsError {
				return "error: " + out.Result
			}
			return out.Result
		}
	case runtime.RecordStep:
		req := suspendOf(rec)
		text := req.Prompt + " · " + rec.Text
		if model := runtime.StepModel(rec); model != "" {
			text += " · " + model
		}
		return text
	case runtime.RecordDecision:
		if d, err := runtime.DecodeDecisionRecord(rec); err == nil {
			return d.Model + " · " + decisionSummary(d)
		}
	case kindCompletion:
		var c struct {
			Phase string `json:"phase"`
		}
		if json.Unmarshal(rec.Payload, &c) == nil && c.Phase != "" {
			return "phase " + c.Phase
		}
	}
	if rec.Text != "" {
		return rec.Text
	}
	return string(rec.Payload)
}

// recordBody is the expanded form of a record: its fields and payload.
func recordBody(rec runtime.Record, w int) string {
	var b strings.Builder
	kv := func(k, v string) {
		if v != "" {
			b.WriteString(sCyan.Render(fmt.Sprintf("%-9s", k)) + " " + v + "\n")
		}
	}
	kv("seq", fmt.Sprint(rec.Seq))
	kv("kind", string(rec.Kind))
	kv("time", rec.Timestamp.Local().Format(time.RFC3339Nano))
	kv("entry", rec.EntryID)
	kv("parent", rec.ParentID)
	kv("role", rec.Role)
	kv("ext_type", rec.ExtType)
	kv("state", string(rec.State))
	if rec.Text != "" {
		kv("text", lipgloss.Wrap(rec.Text, max(4, w-10), ""))
	}
	if len(rec.Payload) > 0 {
		b.WriteString(sCyan.Render("payload") + "\n")
		b.WriteString(colorJSON(prettyJSON(rec.Payload)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderTrace draws a whole trace as plain output: a heading, the time
// axis, and one waterfall line per span. `bonnie runs trace` prints it.
func RenderTrace(tr Trace, w int) string {
	head := sAccent.Bold(true).Render(fmt.Sprintf("turn %d", tr.Turn.Index)) + "  " + stateBadge(tr.Turn.State) + "  " +
		sMuted.Render(tr.Start.Local().Format("2006-01-02 15:04:05")+" · "+dur(tr.Duration()))
	if tr.TraceID != "" {
		head += "  " + sFaint.Render("trace ") + sCyan.Render(tr.TraceID)
	}
	header, lines := waterfall(tr, w, -1, false)
	return strings.Join(append([]string{head, header}, lines...), "\n")
}
