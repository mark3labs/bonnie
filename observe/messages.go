package observe

import (
	"encoding/json"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// genAIMessage is one message in the GenAI semantic conventions' JSON
// message format, the value of gen_ai.input.messages and
// gen_ai.output.messages.
type genAIMessage struct {
	Role         string      `json:"role"`
	Parts        []genAIPart `json:"parts"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type genAIPart struct {
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Response  string          `json:"response,omitempty"`
	MimeType  string          `json:"mime_type,omitempty"`
}

func textPart(text string) genAIPart { return genAIPart{Type: "text", Content: text} }

// toolCallPart keeps valid JSON arguments as JSON, so a backend shows an
// object and not a quoted string.
func toolCallPart(id, name, args string) genAIPart {
	p := genAIPart{Type: "tool_call", ID: id, Name: name}
	if json.Valid([]byte(args)) {
		p.Arguments = json.RawMessage(args)
	} else if args != "" {
		raw, _ := json.Marshal(args)
		p.Arguments = raw
	}
	return p
}

// encodeMessages renders messages as a JSON array. It returns "" when there
// is nothing to render.
func encodeMessages(msgs []genAIMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	data, err := json.Marshal(msgs)
	if err != nil {
		return ""
	}
	return string(data)
}

// fromKit converts the messages a model step receives. File data is never
// copied: a file part records its media type only.
func fromKit(msgs []kit.LLMMessage) []genAIMessage {
	out := make([]genAIMessage, 0, len(msgs))
	for _, m := range msgs {
		gm := genAIMessage{Role: string(m.Role)}
		for _, part := range m.Content {
			if p, ok := fromKitPart(part); ok {
				gm.Parts = append(gm.Parts, p)
			}
		}
		out = append(out, gm)
	}
	return out
}

func fromKitPart(part kit.LLMMessagePart) (genAIPart, bool) {
	switch p := part.(type) {
	case kit.LLMTextPart:
		return textPart(p.Text), true
	case *kit.LLMTextPart:
		return textPart(p.Text), true
	case kit.LLMReasoningPart:
		return genAIPart{Type: "reasoning", Content: p.Text}, true
	case *kit.LLMReasoningPart:
		return genAIPart{Type: "reasoning", Content: p.Text}, true
	case kit.LLMToolCallPart:
		return toolCallPart(p.ToolCallID, p.ToolName, p.Input), true
	case *kit.LLMToolCallPart:
		return toolCallPart(p.ToolCallID, p.ToolName, p.Input), true
	case kit.LLMToolResultPart:
		return genAIPart{Type: "tool_call_response", ID: p.ToolCallID, Response: toolResultText(p.Output)}, true
	case *kit.LLMToolResultPart:
		return genAIPart{Type: "tool_call_response", ID: p.ToolCallID, Response: toolResultText(p.Output)}, true
	case kit.LLMFilePart:
		return genAIPart{Type: "file", MimeType: p.MediaType}, true
	case *kit.LLMFilePart:
		return genAIPart{Type: "file", MimeType: p.MediaType}, true
	}
	return genAIPart{}, false
}

func toolResultText(out kit.LLMToolResultOutputContent) string {
	switch o := out.(type) {
	case kit.LLMToolResultOutputContentText:
		return o.Text
	case *kit.LLMToolResultOutputContentText:
		return o.Text
	case kit.LLMToolResultOutputContentError:
		if o.Error != nil {
			return o.Error.Error()
		}
	case *kit.LLMToolResultOutputContentError:
		if o.Error != nil {
			return o.Error.Error()
		}
	}
	return ""
}
