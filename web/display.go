package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/yuin/goldmark"

	"github.com/mark3labs/bonnie/runtime"
)

// Goldmark's default renderer omits raw HTML and blocks dangerous link schemes.
// Do not enable html.WithUnsafe: this output goes directly into the template.
func renderMarkdown(text string) (string, error) {
	var b bytes.Buffer
	if err := goldmark.New().Convert([]byte(text), &b); err != nil {
		return "", fmt.Errorf("bonnie: web: render markdown: %w", err)
	}
	return b.String(), nil
}

// traceDisplay is a display-only projection. The record and its payload remain
// unchanged. A message payload is authoritative, even if Text is stale.
func traceDisplay(rec runtime.Record, toolNames map[string]string) (string, []string) {
	text := rec.Text
	var tools []string
	if rec.Kind == runtime.RecordMessage && len(rec.Payload) > 0 {
		var msg kit.LLMMessage
		if err := json.Unmarshal(rec.Payload, &msg); err != nil {
			return "Cannot decode message payload", nil
		}
		var parts []string
		for _, part := range msg.Content {
			switch p := part.(type) {
			case kit.LLMTextPart:
				parts = append(parts, p.Text)
			case kit.LLMToolCallPart:
				tools = append(tools, p.ToolName)
				toolNames[p.ToolCallID] = p.ToolName
			case kit.LLMToolResultPart:
				if name := toolNames[p.ToolCallID]; name != "" {
					tools = append(tools, name)
				}
				switch output := p.Output.(type) {
				case kit.LLMToolResultOutputContentText:
					parts = append(parts, output.Text)
				case kit.LLMToolResultOutputContentError:
					if output.Error != nil {
						parts = append(parts, output.Error.Error())
					}
				}
			}
		}
		text = strings.Join(parts, " ")
	}
	preview := strings.Join(strings.Fields(text), " ")
	runes := []rune(preview)
	if len(runes) > 160 {
		preview = string(runes[:160]) + "…"
	}
	return preview, tools
}

// A content key prevents an old branch disclosure from moving to a new message.
func messageDetailID(index int, msg message) string {
	return fmt.Sprintf("message-%d-%x", index, sha256.Sum256([]byte(msg.Payload)))
}

// projectMessage preserves the order of text and tool parts for chat display.
func projectMessage(msg kit.LLMMessage, names map[string]string) (message, error) {
	m := message{Role: string(msg.Role)}
	for _, part := range msg.Content {
		switch p := part.(type) {
		case kit.LLMTextPart:
			if n := len(m.Parts); n > 0 && m.Parts[n-1].Tool == nil {
				m.Parts[n-1].Text += p.Text
			} else {
				m.Parts = append(m.Parts, messagePart{Text: p.Text})
			}
		case kit.LLMToolCallPart:
			names[p.ToolCallID] = p.ToolName
			m.Parts = append(m.Parts, messagePart{Tool: &toolActivity{Name: p.ToolName, Kind: "call", Summary: toolInputSummary(p.Input), Detail: pretty(json.RawMessage(p.Input)), Sections: []toolSection{newToolSection("Arguments", p.Input)}}})
		case kit.LLMToolResultPart:
			name := names[p.ToolCallID]
			if name == "" {
				name = "Tool result"
			}
			payload, err := json.Marshal(kit.LLMMessage{Role: "tool", Content: []kit.LLMMessagePart{p}})
			if err != nil {
				return message{}, fmt.Errorf("bonnie: web: encode tool result: %w", err)
			}
			preview, _ := traceDisplay(runtime.Record{Kind: runtime.RecordMessage, Payload: payload}, names)
			sections := []toolSection{newToolSection("Output", pretty(payload))}
			switch output := p.Output.(type) {
			case kit.LLMToolResultOutputContentText:
				sections = toolOutputSections(output.Text)
				// A shell result summary should show output, not its JSON wrapper.
				for _, section := range sections {
					if section.Label == "stdout" || section.Label == "stderr" {
						preview = compactPreview(section.Text)
						break
					}
				}
			case kit.LLMToolResultOutputContentError:
				if output.Error != nil {
					sections = []toolSection{newToolSection("Error", output.Error.Error())}
				}
			}
			m.Parts = append(m.Parts, messagePart{Tool: &toolActivity{Name: name, Kind: "result", Summary: preview, Detail: pretty(payload), Sections: sections}})
		}
	}
	if m.Role == "assistant" {
		for i := range m.Parts {
			if m.Parts[i].Tool != nil {
				continue
			}
			html, err := renderMarkdown(m.Parts[i].Text)
			if err != nil {
				return message{}, err
			}
			m.Parts[i].HTML = html
		}
	}
	return m, nil
}

// chatTools projects tool parts for callers that need only the activity list.
func chatTools(msg kit.LLMMessage, names map[string]string) []toolActivity {
	projected, _ := projectMessage(msg, names)
	var rows []toolActivity
	for _, part := range projected.Parts {
		if part.Tool != nil {
			rows = append(rows, *part.Tool)
		}
	}
	return rows
}

func compactPreview(text string) string {
	preview, _ := traceDisplay(runtime.Record{Text: text}, nil)
	return preview
}

// The snapshot API has no title metadata. Do not infer a title from messages.
func chatTitle(v view) string {
	if v.ID != "" {
		return v.ID
	}
	return "New chat"
}

func canSend(v view) bool {
	return v.ID == "" || (v.Snapshot.State != runtime.RunRunning && v.Snapshot.State != runtime.RunPending && v.Snapshot.State != runtime.RunWaiting && v.Snapshot.State != runtime.RunRetired)
}
func canStop(v view) bool {
	return v.ID != "" && (v.Snapshot.State == runtime.RunRunning || v.Snapshot.State == runtime.RunPending)
}

// Show a useful argument, not raw argument JSON, in the collapsed row.
func toolInputSummary(input string) string {
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &args) == nil {
		for _, key := range []string{"command", "path", "query", "action", "url"} {
			var value string
			if json.Unmarshal(args[key], &value) == nil && value != "" {
				return compactPreview(value)
			}
		}
	}
	return "Expand arguments"
}
