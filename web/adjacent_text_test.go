package web

import (
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Adjacent text fragments can split Markdown syntax, but a tool is a boundary.
func TestAdjacentTextMarkdown(t *testing.T) {
	t.Parallel()
	msg, err := projectMessage(kit.LLMMessage{Role: "assistant", Content: []kit.LLMMessagePart{
		kit.LLMTextPart{Text: "**be"},
		kit.LLMTextPart{Text: "fore**"},
		kit.LLMToolCallPart{ToolCallID: "call", ToolName: "read", Input: "{}"},
		kit.LLMTextPart{Text: "after"},
	}}, make(map[string]string))
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Parts) != 3 || !strings.Contains(msg.Parts[0].HTML, "<strong>before</strong>") || msg.Parts[1].Tool == nil || msg.Parts[2].Text != "after" {
		t.Fatalf("incorrect text boundaries: %+v", msg.Parts)
	}
}
