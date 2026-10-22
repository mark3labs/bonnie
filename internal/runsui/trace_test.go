package runsui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

func spanNamed(t *testing.T, tr Trace, name string) Span {
	t.Helper()
	for _, s := range tr.Spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no span %q in %v", name, names(tr))
	return Span{}
}

func names(tr Trace) []string {
	var out []string
	for _, s := range tr.Spans {
		out = append(out, s.Name)
	}
	return out
}

// The spans follow the shape package observe exports: one invoke_agent per
// turn, a chat span per step that ends where its first tool starts, and an
// execute_tool span from each intent to its outcome.
func TestBuildTraceSpans(t *testing.T) {
	t.Parallel()
	b := sampleRun(t)
	r := Summarize("run-1", runtime.RunWaiting, b.recs)
	tr := BuildTrace(r, r.Turns[0], t0.Add(time.Hour))

	want := []string{"invoke_agent", "sandbox", "chat · step 1", "execute_tool shell", "execute_tool shell", "suspended · approval"}
	if got := names(tr); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("spans = %v, want %v", got, want)
	}
	if tr.TraceID != "0af7651916cd43dd8448eb211c80319c" || tr.Duration() != 4500*time.Millisecond {
		t.Fatalf("trace %q duration %v", tr.TraceID, tr.Duration())
	}
	chat := spanNamed(t, tr, "chat · step 1")
	if chat.Duration() != 2*time.Second || chat.Status != StatusOK {
		t.Fatalf("chat = %v %s", chat.Duration(), chat.Status)
	}
	if !hasAttr(chat, "finish_reason", "tool-calls") || !hasAttr(chat, "reasoning", "build then deploy") {
		t.Fatalf("chat attrs = %v", chat.Attrs)
	}
	tools := tr.Spans[3:5]
	if tools[0].Duration() != time.Second || tools[0].Status != StatusOK || tools[0].Depth != 2 {
		t.Fatalf("first tool = %+v", tools[0])
	}
	// The second call failed in the lossless message, not its outcome.
	if tools[1].Status != StatusError || !hasAttr(tools[1], "tool.arguments", `{"command":"deploy"}`) {
		t.Fatalf("second tool = %+v", tools[1])
	}
	suspend := tr.Spans[5]
	if suspend.Kind != SpanSuspend || !hasAttr(suspend, "prompt", "Approve prod?") {
		t.Fatalf("suspend = %+v", suspend)
	}
}

func hasAttr(s Span, k, v string) bool {
	for _, a := range s.Attrs {
		if a.Key == k && a.Value == v {
			return true
		}
	}
	return false
}

// A tool with an intent and no outcome was interrupted mid-call: its
// effect is unknown, and the trace says so instead of calling it done.
func TestBuildTraceUnknownTool(t *testing.T) {
	t.Parallel()
	b := newBuilder(t, "run-u")
	b.turn("T", "").state(runtime.RunRunning).
		after(time.Second).intent("c", "charge_card", "{}").
		after(5 * time.Second).state(runtime.RunInterrupted)
	r := Summarize("run-u", runtime.RunInterrupted, b.recs)
	tr := BuildTrace(r, r.Turns[0], t0.Add(time.Hour))
	tool := spanNamed(t, tr, "execute_tool charge_card")
	if tool.Status != StatusUnknown || tool.Duration() != 5*time.Second {
		t.Fatalf("tool = %+v", tool)
	}
	if spanNamed(t, tr, "invoke_agent").Status != StatusError {
		t.Fatal("an interrupted turn must read as an error")
	}
}

// After the last checkpoint of a turn that failed, the time went somewhere
// the journal did not see. The trace shows the gap as an unfinished step.
func TestBuildTraceShowsTheUnjournalledTail(t *testing.T) {
	t.Parallel()
	b := newBuilder(t, "run-g")
	b.turn("T", "").state(runtime.RunRunning).
		message("assistant", kit.LLMTextPart{Text: "thinking"}).
		after(time.Second).step(0, "stop").
		after(10 * time.Second).state(runtime.RunFailed)
	r := Summarize("run-g", runtime.RunFailed, b.recs)
	tr := BuildTrace(r, r.Turns[0], t0.Add(time.Hour))
	tail := tr.Spans[len(tr.Spans)-1]
	if tail.Name != "chat · step 2" || tail.Status != StatusUnknown || tail.Duration() != 10*time.Second {
		t.Fatalf("tail = %+v", tail)
	}
}

// An open turn ends at now, and its spans stay open.
func TestBuildTraceOpenTurn(t *testing.T) {
	t.Parallel()
	b := newBuilder(t, "run-o")
	b.turn("T", "").state(runtime.RunRunning).after(time.Second).intent("c", "shell", "{}")
	r := Summarize("run-o", runtime.RunRunning, b.recs)
	now := t0.Add(9 * time.Second)
	tr := BuildTrace(r, r.Turns[0], now)
	if !tr.End.Equal(now) || tr.Spans[0].Status != StatusOpen {
		t.Fatalf("trace end %v root %s", tr.End, tr.Spans[0].Status)
	}
	if tool := spanNamed(t, tr, "execute_tool shell"); tool.Status != StatusOpen || tool.Duration() != 8*time.Second {
		t.Fatalf("tool = %+v", tool)
	}
}

// Every waterfall line has the width asked for, at any width, so the
// panel border never moves.
func TestWaterfallFitsWidth(t *testing.T) {
	t.Parallel()
	b := sampleRun(t)
	r := Summarize("run-1", runtime.RunWaiting, b.recs)
	tr := BuildTrace(r, r.Turns[0], t0.Add(time.Hour))
	for _, w := range []int{40, 77, 120, 200} {
		header, lines := waterfall(tr, w, 2, true)
		for i, l := range append([]string{header}, lines...) {
			if got := ansi.StringWidth(l); got > w {
				t.Fatalf("width %d: line %d is %d cells: %q", w, i, got, ansi.Strip(l))
			}
		}
	}
	if out := ansi.Strip(RenderTrace(tr, 100)); !strings.Contains(out, "turn 1") || !strings.Contains(out, "⚙ shell") {
		t.Fatalf("RenderTrace:\n%s", out)
	}
}
