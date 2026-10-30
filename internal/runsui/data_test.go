package runsui

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/wire"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// journalBuilder writes records the way the runtime does, with times the
// test controls.
type journalBuilder struct {
	t    *testing.T
	id   string
	at   time.Time
	recs []runtime.Record
}

func newBuilder(t *testing.T, id string) *journalBuilder {
	return &journalBuilder{t: t, id: id, at: t0}
}

func (b *journalBuilder) after(d time.Duration) *journalBuilder {
	b.at = b.at.Add(d)
	return b
}

func (b *journalBuilder) add(rec runtime.Record) *journalBuilder {
	rec.Seq = len(b.recs) + 1
	rec.RunID = b.id
	rec.Timestamp = b.at
	b.recs = append(b.recs, rec)
	return b
}

func (b *journalBuilder) payload(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		b.t.Fatal(err)
	}
	return raw
}

func (b *journalBuilder) state(s runtime.RunState) *journalBuilder {
	return b.add(runtime.Record{Kind: runtime.RecordState, State: s})
}

func (b *journalBuilder) turn(id, traceparent string) *journalBuilder {
	rec := runtime.Record{Kind: runtime.RecordTurn, Text: id}
	if traceparent != "" {
		rec.Payload = b.payload(map[string]any{"trace": map[string]string{"traceparent": traceparent}})
	}
	return b.add(rec)
}

func (b *journalBuilder) message(role string, parts ...kit.LLMMessagePart) *journalBuilder {
	msg := kit.LLMMessage{Role: kit.LLMMessageRole(role), Content: parts}
	text := ""
	for _, p := range parts {
		if t, ok := p.(kit.LLMTextPart); ok {
			text += t.Text
		}
	}
	return b.add(runtime.Record{Kind: runtime.RecordMessage, Role: role, Text: text, Payload: b.payload(msg)})
}

func (b *journalBuilder) intent(id, name, args string) *journalBuilder {
	return b.add(runtime.Record{Kind: kindToolIntent, Payload: b.payload(map[string]any{"id": id, "name": name, "args": args})})
}

func (b *journalBuilder) outcome(id, result string, isErr bool) *journalBuilder {
	return b.add(runtime.Record{Kind: kindToolOutcome, Payload: b.payload(map[string]any{"id": id, "result": result, "is_error": isErr})})
}

func (b *journalBuilder) step(n int, finish string) *journalBuilder {
	return b.add(runtime.Record{Kind: runtime.RecordStep, Text: finish, Payload: b.payload(wire.SuspendRequest{Kind: "step", Prompt: "step " + string(rune('0'+n))})})
}

// sampleRun is a run with two turns: the first calls two tools, one of
// which fails, and suspends on an approval; the second resumes, answers,
// and completes.
func sampleRun(t *testing.T) *journalBuilder {
	b := newBuilder(t, "run-1")
	b.state(runtime.RunPending).
		add(runtime.Record{Kind: runtime.RecordExtensionData, ExtType: runtime.ExtTitle, Text: "Deploy the app"}).
		add(runtime.Record{Kind: runtime.RecordExtensionData, ExtType: runtime.ExtOrigin, Text: `{"channel":"slack","kind":"message"}`}).
		turn("T1", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01").
		state(runtime.RunRunning).
		add(runtime.Record{Kind: runtime.RecordSandbox, Text: "sandbox opened", Payload: b.payload(map[string]string{"backend": "docker", "sandbox_id": "bonnie-x"})}).
		message("user", kit.LLMTextPart{Text: "deploy it"}).
		after(2*time.Second).intent("c1", "shell", `{"command":"make"}`).
		after(time.Second).outcome("c1", "ok", false).
		intent("c2", "shell", `{"command":"deploy"}`).
		after(500*time.Millisecond).outcome("c2", "", false).
		message("assistant",
			kit.LLMReasoningPart{Text: "build then deploy"},
			kit.LLMToolCallPart{ToolCallID: "c1", ToolName: "shell", Input: `{"command":"make"}`},
			kit.LLMToolCallPart{ToolCallID: "c2", ToolName: "shell", Input: `{"command":"deploy"}`}).
		// The outcome hook did not mark c2, but the lossless message did.
		message("tool",
			kit.LLMToolResultPart{ToolCallID: "c1", Output: kit.LLMToolResultOutputContentText{Text: "ok"}},
			kit.LLMToolResultPart{ToolCallID: "c2", Output: kit.LLMToolResultOutputContentError{Error: errDeploy}}).
		step(0, "tool-calls").
		after(time.Second).
		add(runtime.Record{Kind: runtime.RecordSuspend, Text: "Approve prod?", Payload: b.payload(wire.SuspendRequest{TurnID: "T1", Kind: wire.SuspendApproval, Prompt: "Approve prod?"})}).
		state(runtime.RunWaiting)
	return b
}

type testErr string

func (e testErr) Error() string { return string(e) }

const errDeploy = testErr("deploy failed")

func TestSummarizeCountsAndOpenQuestion(t *testing.T) {
	t.Parallel()
	b := sampleRun(t)
	r := Summarize("run-1", runtime.RunWaiting, b.recs)

	if r.Title != "Deploy the app" || r.Origin != "slack/message" || r.Sandbox != "docker bonnie-x" {
		t.Fatalf("metadata: title %q origin %q sandbox %q", r.Title, r.Origin, r.Sandbox)
	}
	if r.Steps != 1 || r.ToolCalls != 2 || r.ToolUse["shell"] != 2 {
		t.Fatalf("counts: steps %d calls %d use %v", r.Steps, r.ToolCalls, r.ToolUse)
	}
	// One failure, seen only in the tool message.
	if r.ToolErrors != 1 {
		t.Fatalf("tool errors = %d, want 1", r.ToolErrors)
	}
	if r.Suspend == nil || r.Suspend.Kind != wire.SuspendApproval || r.Suspend.TurnID != "T1" {
		t.Fatalf("open question = %+v", r.Suspend)
	}
	if len(r.Turns) != 1 {
		t.Fatalf("turns = %d", len(r.Turns))
	}
	turn := r.Turns[0]
	if turn.TraceID != "0af7651916cd43dd8448eb211c80319c" || turn.SpanID != "b7ad6b7169203331" {
		t.Fatalf("trace = %q span = %q", turn.TraceID, turn.SpanID)
	}
	if turn.State != runtime.RunWaiting || turn.Open() || turn.Prompt != "deploy it" {
		t.Fatalf("turn = %+v", turn)
	}
	if got := turn.Duration(time.Time{}); got != 4500*time.Millisecond {
		t.Fatalf("turn duration = %v", got)
	}
}

// An outcome marked is_error and a tool message that carries the same
// failure are one failed call, not two.
func TestSummarizeCountsEachFailedCallOnce(t *testing.T) {
	t.Parallel()
	b := newBuilder(t, "run-e")
	b.turn("T", "").state(runtime.RunRunning).
		intent("c", "shell", "{}").outcome("c", "boom", true).
		message("tool", kit.LLMToolResultPart{ToolCallID: "c", Output: kit.LLMToolResultOutputContentError{Error: errDeploy}}).
		state(runtime.RunCompleted)
	r := Summarize("run-e", runtime.RunCompleted, b.recs)
	if r.ToolErrors != 1 || r.Turns[0].ToolErrors != 1 {
		t.Fatalf("errors = %d, turn errors = %d", r.ToolErrors, r.Turns[0].ToolErrors)
	}
}

// A resume answers the question: the run has no open question after it,
// even when the journal says waiting again for a later one.
func TestSummarizeResumeClosesTheQuestion(t *testing.T) {
	t.Parallel()
	b := sampleRun(t)
	b.after(time.Hour).add(runtime.Record{Kind: runtime.RecordResume, Text: "yes"}).
		turn("T2", "").state(runtime.RunRunning).
		message("assistant", kit.LLMTextPart{Text: "deployed"}).
		step(1, "stop").state(runtime.RunCompleted)
	r := Summarize("run-1", runtime.RunCompleted, b.recs)
	if r.Suspend != nil {
		t.Fatalf("open question after resume: %+v", r.Suspend)
	}
	if len(r.Turns) != 2 || r.Turns[1].State != runtime.RunCompleted || r.Last != "deployed" {
		t.Fatalf("turns %+v last %q", r.Turns, r.Last)
	}
	if r.Turns[0].Last != r.Turns[1].First-1 {
		t.Fatalf("turn ranges overlap or gap: %d, %d", r.Turns[0].Last, r.Turns[1].First)
	}
}

// A crash can leave the last turn without its closing state. The run's
// own state is the authority, so the turn is closed at the last record.
func TestSummarizeClosesACrashedTurn(t *testing.T) {
	t.Parallel()
	b := newBuilder(t, "run-c")
	b.turn("T", "").state(runtime.RunRunning).after(3*time.Second).intent("c", "shell", "{}")
	r := Summarize("run-c", runtime.RunFailed, b.recs)
	if r.Turns[0].Open() || r.Turns[0].State != runtime.RunFailed || !r.Turns[0].End.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("turn = %+v", r.Turns[0])
	}
}

func TestSortAndFilter(t *testing.T) {
	t.Parallel()
	runs := []Run{
		{ID: "a", State: runtime.RunCompleted, Updated: t0.Add(3 * time.Hour), Created: t0},
		{ID: "b", State: runtime.RunWaiting, Updated: t0.Add(1 * time.Hour), Created: t0.Add(time.Hour)},
		{ID: "c", Title: "Alpha", State: runtime.RunFailed, Updated: t0.Add(2 * time.Hour), Created: t0.Add(2 * time.Hour)},
	}
	order := func(k SortKey) string {
		SortRuns(runs, k)
		s := ""
		for _, r := range runs {
			s += r.ID
		}
		return s
	}
	for k, want := range map[SortKey]string{SortUpdated: "acb", SortCreated: "cba", SortState: "bca", SortName: "acb"} {
		if got := order(k); got != want {
			t.Errorf("sort %s = %s, want %s", k, got, want)
		}
	}
	match := func(f StateFilter) string {
		s := ""
		for _, r := range runs {
			if f.Match(r) {
				s += r.ID
			}
		}
		return s
	}
	SortRuns(runs, SortName)
	for f, want := range map[StateFilter]string{FilterAll: "acb", FilterActive: "b", FilterWaiting: "b", FilterFailed: "c", FilterDone: "a"} {
		if got := match(f); got != want {
			t.Errorf("filter %s = %s, want %s", f, got, want)
		}
	}
	if !matchText(runs[1], "alp FAIL") || matchText(runs[1], "alpha waiting") {
		t.Error("text filter must match every word, in any case")
	}
}

// Each step names the model that served it. The run shows the latest one,
// the trace puts it on the step's chat span, and the journal line shows it.
// A step from before BONNIE recorded the model shows none.
func TestStepsNameTheirModel(t *testing.T) {
	t.Parallel()
	b := newBuilder(t, "run-m")
	stepWith := func(n int, model string) *journalBuilder {
		return b.add(runtime.Record{Kind: runtime.RecordStep, Text: "stop",
			Payload: b.payload(map[string]string{"kind": "step", "prompt": fmt.Sprintf("step %d", n), "model": model})})
	}
	b.state(runtime.RunPending).turn("T1", "").state(runtime.RunRunning).
		message("user", kit.LLMTextPart{Text: "hi"})
	stepWith(1, "anthropic/claude-sonnet-4-5")
	b.message("assistant", kit.LLMTextPart{Text: "hello"})
	stepWith(2, "openrouter/qwen/qwen3")
	b.state(runtime.RunCompleted)

	r := Summarize("run-m", runtime.RunCompleted, b.recs)
	if r.Model != "openrouter/qwen/qwen3" {
		t.Fatalf("run model = %q, want the latest step's", r.Model)
	}
	tr := BuildTrace(r, r.Turns[0], t0.Add(time.Hour))
	for name, want := range map[string]string{"chat · step 1": "anthropic/claude-sonnet-4-5", "chat · step 2": "openrouter/qwen/qwen3"} {
		if !hasAttr(spanNamed(t, tr, name), "model", want) {
			t.Errorf("%s has no model %q: %v", name, want, spanNamed(t, tr, name).Attrs)
		}
	}
	var line string
	for _, rec := range b.recs {
		if rec.Kind == runtime.RecordStep {
			line = recordDetail(rec)
			break
		}
	}
	if line != "step 1 · stop · anthropic/claude-sonnet-4-5" {
		t.Errorf("journal detail = %q", line)
	}

	old := sampleRun(t)
	if r := Summarize("run-1", runtime.RunWaiting, old.recs); r.Model != "" {
		t.Errorf("a run with no step model shows %q", r.Model)
	}
}
