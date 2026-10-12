package web

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// JSON colors never introduce model HTML. Plain output keeps line breaks.
func TestToolSectionFormatting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text string
		want []string
	}{
		{`{"path":"<script>alert(1)</script>","count":2,"ok":true,"none":null}`, []string{`syntax-key`, `syntax-string`, `syntax-number`, `syntax-literal`, "\n", "&lt;script&gt;"}},
		{"<img src=x onerror=alert(1)>\nnext", []string{"&lt;img", "\nnext"}},
		{`{"broken":`, []string{`{&#34;broken&#34;:`}},
	} {
		section := newToolSection("Output", tc.text)
		for _, want := range tc.want {
			if !strings.Contains(section.HTML, want) {
				t.Errorf("missing %q in %s", want, section.HTML)
			}
		}
		if strings.Contains(section.HTML, "<script") || strings.Contains(section.HTML, "<img") {
			t.Fatal("unescaped tool output")
		}
	}
}

// Shell streams are readable text, not escaped JSON inside a message envelope.
// The raw disclosure retains that envelope, with independent stable state.
func TestToolResultSections(t *testing.T) {
	t.Parallel()
	text := `{"stdout":"first\nsecond\n","stderr":"warning\n","exit_code":0,"completed":true}`
	msg := kit.LLMMessage{Role: "tool", Content: []kit.LLMMessagePart{
		kit.LLMToolResultPart{ToolCallID: "a", Output: kit.LLMToolResultOutputContentText{Text: text}},
	}}
	rows := chatTools(msg, map[string]string{"a": "shell"})
	if len(rows) != 1 || rows[0].Summary != "first second" || len(rows[0].Sections) != 3 {
		t.Fatalf("projection: %+v", rows)
	}
	if rows[0].Sections[0].Text != "first\nsecond\n" || rows[0].Sections[1].Label != "stderr" || strings.Contains(rows[0].Sections[2].Text, "stdout") {
		t.Fatalf("sections: %+v", rows[0].Sections)
	}
	var b bytes.Buffer
	if err := renderComponent(httptest.NewRequest("GET", "/web/", nil), toolActivityView("message-test", 0, rows[0]), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Raw payload", "first\nsecond\n", `id="message-test-tool-0-section-0"`, `id="message-test-tool-0-raw"`, `data-preserve-attr="open"`, "syntax-key"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %s", want, b.String())
		}
	}
	for _, text := range []string{"plain output", `{"stdout":"","stderr":"","exit_code":0}`, `[1,true,"value"]`} {
		if sections := toolOutputSections(text); len(sections) != 1 || sections[0].Label != "Output" {
			t.Fatalf("fallback: %+v", sections)
		}
	}
}
