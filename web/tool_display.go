package web

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
)

var jsonToken = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|\b(?:true|false|null)\b`)

// toolOutputSections separates shell streams from JSON metadata. This is a
// display projection only; Detail retains the complete message payload.
func toolOutputSections(text string) []toolSection {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &fields) == nil && fields != nil {
		var streams []toolSection
		for _, key := range []string{"stdout", "stderr"} {
			var value string
			if raw, ok := fields[key]; ok && json.Unmarshal(raw, &value) == nil {
				delete(fields, key)
				if value != "" {
					streams = append(streams, newToolSection(key, value))
				}
			}
		}
		if len(streams) > 0 {
			// RawMessage values were decoded above and can always be encoded.
			metadata, err := json.Marshal(fields)
			if err == nil && len(fields) > 0 {
				streams = append(streams, newToolSection("Metadata", string(metadata)))
			}
			return streams
		}
	}
	return []toolSection{newToolSection("Output", text)}
}

// newToolSection formats valid JSON and colors its tokens. Plain output stays
// literal, not Markdown or HTML. All source text is escaped before templ.Raw.
func newToolSection(label, text string) toolSection {
	s := toolSection{Label: label, Text: text}
	if !json.Valid([]byte(text)) {
		s.HTML = html.EscapeString(text)
		return s
	}
	s.Text = pretty(json.RawMessage(text))
	var b strings.Builder
	end := 0
	for _, match := range jsonToken.FindAllStringIndex(s.Text, -1) {
		b.WriteString(html.EscapeString(s.Text[end:match[0]]))
		token := s.Text[match[0]:match[1]]
		class := "number"
		if token[0] == '"' {
			class = "string"
			if strings.HasPrefix(strings.TrimSpace(s.Text[match[1]:]), ":") {
				class = "key"
			}
		} else if token == "true" || token == "false" || token == "null" {
			class = "literal"
		}
		b.WriteString(`<span class="syntax-` + class + `">`)
		b.WriteString(html.EscapeString(token))
		b.WriteString("</span>")
		end = match[1]
	}
	b.WriteString(html.EscapeString(s.Text[end:]))
	s.HTML = b.String()
	return s
}
