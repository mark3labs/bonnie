package evalui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// entry is one item of an eval's conversation.
type entry struct {
	kind string // "user", "assistant", "call", "result", "error", "decision", "suspend"
	text string
}

// loadConversation reads the conversation of the eval whose journal is dir.
func loadConversation(ctx context.Context, dir string) ([]entry, error) {
	j, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	defer func() { _ = j.Close() }()
	recs, err := j.Replay(ctx, RunID)
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	return conversation(recs), nil
}

// conversation turns journal records into entries. Tool calls, results and
// decisions are kept, because a check of an eval is often about them.
func conversation(recs []runtime.Record) []entry {
	var out []entry
	names := map[string]string{}
	for _, rec := range recs {
		switch rec.Kind {
		case runtime.RecordMessage:
			var msg kit.LLMMessage
			if len(rec.Payload) == 0 || json.Unmarshal(rec.Payload, &msg) != nil {
				msg = kit.LLMMessage{Role: kit.LLMMessageRole(rec.Role), Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: rec.Text}}}
			}
			for _, part := range msg.Content {
				switch p := part.(type) {
				case kit.LLMTextPart:
					if strings.TrimSpace(p.Text) == "" {
						continue
					}
					kind := "assistant"
					if msg.Role == kit.LLMRoleUser {
						kind = "user"
					}
					out = append(out, entry{kind: kind, text: p.Text})
				case kit.LLMToolCallPart:
					names[p.ToolCallID] = p.ToolName
					out = append(out, entry{kind: "call", text: p.ToolName + " " + p.Input})
				case kit.LLMToolResultPart:
					name := names[p.ToolCallID]
					switch o := p.Output.(type) {
					case kit.LLMToolResultOutputContentText:
						out = append(out, entry{kind: "result", text: name + ": " + o.Text})
					case kit.LLMToolResultOutputContentError:
						text := name + ": error"
						if o.Error != nil {
							text = name + ": " + o.Error.Error()
						}
						out = append(out, entry{kind: "error", text: text})
					}
				}
			}
		case runtime.RecordDecision:
			d, err := runtime.DecodeDecisionRecord(rec)
			if err != nil {
				continue
			}
			var parts []string
			for _, a := range d.Answers {
				switch a.Type {
				case kit.DecisionBoolean:
					parts = append(parts, fmt.Sprintf("%s P=%.2f", a.ID, a.Probability))
				case kit.DecisionChoice:
					parts = append(parts, fmt.Sprintf("%s=%s", a.ID, a.Choice))
				default:
					parts = append(parts, fmt.Sprintf("%s score=%.2f", a.ID, a.Score))
				}
			}
			if d.Error != "" {
				parts = append(parts, "error "+d.Error)
			}
			out = append(out, entry{kind: "decision", text: strings.Join(parts, ", ")})
		case runtime.RecordSuspend:
			var sr runtime.SuspendRequest
			text := "waits for a human answer"
			if json.Unmarshal(rec.Payload, &sr) == nil && sr.Prompt != "" {
				text += ": " + sr.Prompt
			}
			out = append(out, entry{kind: "suspend", text: text})
		}
	}
	return out
}

// renderConversation draws entries in w cells.
func renderConversation(entries []entry, w int) []string {
	var lines []string
	for i, e := range entries {
		if i > 0 && (e.kind == "user" || e.kind == "assistant") {
			lines = append(lines, "")
		}
		switch e.kind {
		case "user":
			lines = append(lines, sAccent.Bold(true).Render("you"))
			lines = append(lines, wrap(e.text, w, sText)...)
		case "assistant":
			lines = append(lines, sCyan.Bold(true).Render("agent"))
			lines = append(lines, wrap(e.text, w, sText)...)
		case "call":
			lines = append(lines, wrap("→ "+e.text, w, sCyan)...)
		case "result":
			lines = append(lines, wrap("← "+e.text, w, sMuted)...)
		case "error":
			lines = append(lines, wrap("✗ "+e.text, w, sRed)...)
		case "decision":
			lines = append(lines, wrap("◆ decision "+e.text, w, sPurple)...)
		case "suspend":
			lines = append(lines, wrap("◆ "+e.text, w, sYellow)...)
		}
	}
	return lines
}
