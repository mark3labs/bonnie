package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"

	httpchannel "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
)

// Only Markdown renderer output can reach templ.Raw. Raw HTML, event handlers,
// and executable URLs must not reach it, including encoded URL schemes.
func TestMarkdown(t *testing.T) {
	t.Parallel()
	text := "# Answer\n\n**Yes** and `code`.\n\n- one\n- two\n\n> note\n\n```go\nvar x = 1\n```\n\n[Guide](https://example.com)\n\n<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n[bad](javascript:alert%281%29) [bad](data:text/html;base64,PHNjcmlwdD4=) [encoded](jav&#x61;script:alert%281%29)"
	html, err := renderMarkdown(text)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h1>Answer</h1>", "<strong>Yes</strong>", "<code>code</code>", "<ul>", "<blockquote>", `class="language-go"`, `href="https://example.com"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s in %s", want, html)
		}
	}
	for _, bad := range []string{"<script", "<img", "onerror", "javascript:", "data:text/html"} {
		if strings.Contains(html, bad) {
			t.Errorf("unsafe %s in %s", bad, html)
		}
	}
}

// Tool result names come from earlier call payloads, also when a filter hides
// the call. Display must not write back to the record or use stale Text.
func TestTraceDisplay(t *testing.T) {
	t.Parallel()
	names := make(map[string]string)
	call := kit.LLMMessage{Role: "assistant", Content: []kit.LLMMessagePart{
		kit.LLMTextPart{Text: "Let me\n  read the file."},
		kit.LLMToolCallPart{ToolCallID: "call-1", ToolName: "read", Input: `{"path":"a.go"}`},
	}}
	result := kit.LLMMessage{Role: "tool", Content: []kit.LLMMessagePart{
		kit.LLMToolResultPart{ToolCallID: "call-1", Output: kit.LLMToolResultOutputContentText{Text: "File contents"}},
	}}
	for _, tc := range []struct {
		msg     kit.LLMMessage
		preview string
	}{{call, "Let me read the file."}, {result, "File contents"}} {
		payload, err := json.Marshal(tc.msg)
		if err != nil {
			t.Fatal(err)
		}
		original := bytes.Clone(payload)
		rec := runtime.Record{Kind: runtime.RecordMessage, Text: "stale", Payload: payload}
		preview, tools := traceDisplay(rec, names)
		if preview != tc.preview || len(tools) != 1 || tools[0] != "read" {
			t.Fatalf("display: %q %v", preview, tools)
		}
		if rec.Text != "stale" || !bytes.Equal(rec.Payload, original) {
			t.Fatal("display changed record")
		}
	}
	preview, _ := traceDisplay(runtime.Record{Text: strings.Repeat("界", 170)}, names)
	if len([]rune(preview)) != 161 || !strings.HasSuffix(preview, "…") {
		t.Fatal("preview is not bounded Unicode")
	}
	preview, _ = traceDisplay(runtime.Record{Kind: runtime.RecordMessage, Payload: json.RawMessage("broken"), Text: "stale"}, names)
	if preview != "Cannot decode message payload" {
		t.Fatal("invalid payload used stale text")
	}
}

// Page reads and filtered traces use the payload projection without changing
// the journal. User text stays plain, while assistant text uses Markdown.
func TestRenderedConversationAndFilteredTools(t *testing.T) {
	t.Parallel()
	h, j := setup(t)
	s := runtime.NewSession("markdown", j)
	if _, err := s.AppendMessage(kit.NewLLMUserMessage("**plain user text**")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(kit.LLMMessage{Role: "assistant", Content: []kit.LLMMessagePart{
		kit.LLMTextPart{Text: "**Rendered answer**"},
		kit.LLMToolCallPart{ToolCallID: "call", ToolName: "read", Input: "{}"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(kit.LLMMessage{Role: "tool", Content: []kit.LLMMessagePart{
		kit.LLMToolResultPart{ToolCallID: "call", Output: kit.LLMToolResultOutputContentText{Text: "unique result"}},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := j.Replay(t.Context(), "markdown")
	if err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/web/?run=markdown&filter=unique", nil, nil)
	html := w.Body.String()
	for _, want := range []string{"<strong>Rendered answer</strong>", "**plain user text**", `data-preserve-attr="open"`, `data-ignore-morph`, "connection-status"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(html, "<strong>plain user text</strong>") {
		t.Fatal("user text rendered as Markdown")
	}
	w = request(h, "GET", "/web/?view=trace&run=markdown&filter=unique", nil, nil)
	if !strings.Contains(w.Body.String(), `class="tool-name">read`) {
		t.Fatal("trace lost tool name")
	}
	after, err := j.Replay(t.Context(), "markdown")
	if err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("page read changed journal")
	}
}

// Tool rows keep arguments and results in closed disclosures. Mixed assistant
// text remains Markdown, and unknown result names never use another branch.
func TestChatTools(t *testing.T) {
	t.Parallel()
	names := map[string]string{}
	call := kit.LLMMessage{Role: "assistant", Content: []kit.LLMMessagePart{
		kit.LLMTextPart{Text: "**Reading**"},
		kit.LLMToolCallPart{ToolCallID: "a", ToolName: "read", Input: `{"path":"<a.go>","other":"secret"}`},
		kit.LLMToolCallPart{ToolCallID: "b", ToolName: "search", Input: `{"query":"` + strings.Repeat("界", 170) + `"}`},
	}}
	rows := chatTools(call, names)
	if len(rows) != 2 || rows[0].Name != "read" || rows[0].Summary != "<a.go>" || !strings.Contains(rows[0].Detail, "secret") || len([]rune(rows[1].Summary)) != 161 {
		t.Fatalf("calls: %+v", rows)
	}
	result := kit.LLMMessage{Role: "tool", Content: []kit.LLMMessagePart{
		kit.LLMToolResultPart{ToolCallID: "a", Output: kit.LLMToolResultOutputContentText{Text: "found\n file"}},
		kit.LLMToolResultPart{ToolCallID: "unknown", Output: kit.LLMToolResultOutputContentError{Error: fmt.Errorf("cannot read")}},
	}}
	rows = chatTools(result, names)
	if len(rows) != 2 || rows[0].Name != "read" || rows[0].Summary != "found file" || rows[1].Name != "Tool result" || rows[1].Summary != "cannot read" {
		t.Fatalf("results: %+v", rows)
	}
	for _, input := range []string{"broken", `{"other":"secret"}`, "{}"} {
		if toolInputSummary(input) != "Expand arguments" {
			t.Fatal("raw JSON exposed in summary")
		}
	}
	h, j := setup(t)
	s := runtime.NewSession("tool-chat", j)
	call.Content = call.Content[:2]
	for _, msg := range []kit.LLMMessage{call, result} {
		if _, err := s.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	html := request(h, "GET", "/web/?run=tool-chat", nil, nil).Body.String()
	for _, want := range []string{`class="tool-activity"`, `class="tool-name">read`, `class="tool-summary">&lt;a.go&gt;`, `<strong>Reading</strong>`, `data-preserve-attr="open"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s in %s", want, html)
		}
	}
	if strings.Contains(html, ` open`) {
		t.Fatal("tool details start open")
	}
}

// The composer exposes only durable state and controls that the channel supports.
func TestChatStructureAndState(t *testing.T) {
	t.Parallel()
	for _, state := range []runtime.RunState{runtime.RunRunning, runtime.RunPending, runtime.RunWaiting, runtime.RunCompleted, runtime.RunFailed, runtime.RunCancelled, runtime.RunRetired} {
		v := view{View: "chat", ID: "<run>", Token: "csrf", Snapshot: httpchannel.SnapshotResponse{RunResponse: httpchannel.RunResponse{State: state, TurnID: "turn"}}}
		var b bytes.Buffer
		if err := renderComponent(httptest.NewRequest("GET", "/web/", nil), panel(v), &b); err != nil {
			t.Fatal(err)
		}
		html := b.String()
		if !strings.Contains(html, `# </span>&lt;run&gt;`) || !strings.Contains(html, fmt.Sprintf(`Run: <span data-state="%s">%s</span>`, state, state)) || !strings.Contains(html, "<kbd>Ctrl</kbd> + <kbd>Enter</kbd> to send") {
			t.Fatalf("structure: %s", html)
		}
		if strings.Contains(html, `id="stop-form"`) != canStop(v) {
			t.Fatalf("stop state %s", state)
		}
		if regexp.MustCompile(`<button[^>]* disabled(?:[ =>])`).MatchString(html) == canSend(v) {
			t.Fatalf("send state %s: %s", state, html)
		}
		if canStop(v) && (!strings.Contains(html, `name="turn" value="turn"`) || !strings.Contains(html, `name="csrf" value="csrf"`)) {
			t.Fatal("stop lost protected fields")
		}
	}
	if chatTitle(view{}) != "New chat" {
		t.Fatal("new title")
	}
}
