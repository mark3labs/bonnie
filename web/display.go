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

// chatTools projects each tool part without changing the snapshot. Names for
// results come only from calls in this branch; unknown names stay explicit.
func chatTools(msg kit.LLMMessage, names map[string]string) []toolActivity {
	var rows []toolActivity
	for _, part := range msg.Content {
		switch p := part.(type) {
		case kit.LLMToolCallPart:
			names[p.ToolCallID] = p.ToolName
			rows = append(rows, toolActivity{Name: p.ToolName, Kind: "call", Summary: toolInputSummary(p.Input), Detail: pretty(json.RawMessage(p.Input))})
		case kit.LLMToolResultPart:
			name := names[p.ToolCallID]
			if name == "" {
				name = "Tool result"
			}
			payload, err := json.Marshal(kit.LLMMessage{Role: "tool", Content: []kit.LLMMessagePart{p}})
			if err != nil {
				rows = append(rows, toolActivity{Name: name, Kind: "result", Summary: "Cannot decode tool result"})
				continue
			}
			preview, _ := traceDisplay(runtime.Record{Kind: runtime.RecordMessage, Payload: payload}, names)
			rows = append(rows, toolActivity{Name: name, Kind: "result", Summary: preview, Detail: pretty(payload)})
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
