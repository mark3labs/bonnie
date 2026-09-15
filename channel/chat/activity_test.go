package chat

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// event builds the runtime event the runner forwards for one Kit lifecycle
// event: the Kit type's own name, and the event itself as the payload.
func event(t *testing.T, e kit.Event) runtime.Event {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("encode %T: %v", e, err)
	}
	return runtime.Event{Type: string(e.EventType()), Data: raw, Time: time.Now()}
}

// recorder collects the statuses a transport would render.
type recorder struct{ statuses []string }

func (r *recorder) render(status string) { r.statuses = append(r.statuses, status) }

func (r *recorder) last() string {
	if len(r.statuses) == 0 {
		return ""
	}
	return r.statuses[len(r.statuses)-1]
}

// A turn's events become the statuses a person reads: the work starts, a
// tool is named with its subject, and the end of the tool falls back to the
// general status rather than leaving the last tool on screen.
func TestActivityFollowsATurn(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := &activity{render: rec.render}

	a.apply(event(t, kit.TurnStartEvent{Prompt: "ship it"}))
	a.apply(event(t, kit.ToolCallEvent{
		ToolCallID: "c1",
		ToolName:   "read_file",
		ParsedArgs: map[string]any{"path": "runtime/runner.go"},
	}))
	a.apply(event(t, kit.ToolResultEvent{ToolCallID: "c1", ToolName: "read_file"}))

	want := []string{StatusWorking, "read_file runtime/runner.go", StatusWorking}
	if len(rec.statuses) != len(want) {
		t.Fatalf("statuses = %q, want %q", rec.statuses, want)
	}
	for i, w := range want {
		if rec.statuses[i] != w {
			t.Errorf("status %d = %q, want %q", i, rec.statuses[i], w)
		}
	}
}

// Several tools in one breath read as the first one and a count. The count
// is what a person needs: how much is happening, not a list they cannot
// read while it changes.
func TestActivityCountsParallelCalls(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := &activity{render: rec.render}

	a.apply(event(t, kit.TurnStartEvent{}))
	a.apply(event(t, kit.ToolCallEvent{ToolCallID: "c1", ToolName: "grep",
		ParsedArgs: map[string]any{"pattern": "ErrRunNotFound"}}))
	a.apply(event(t, kit.ToolCallEvent{ToolCallID: "c2", ToolName: "grep",
		ParsedArgs: map[string]any{"pattern": "ErrNotWaiting"}}))
	a.apply(event(t, kit.ToolCallEvent{ToolCallID: "c3", ToolName: "ls",
		ParsedArgs: map[string]any{"path": "runtime"}}))

	if got, want := rec.last(), "grep ErrRunNotFound +2 more"; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}

	// The first call finishing promotes the next, and the count shrinks.
	a.apply(event(t, kit.ToolResultEvent{ToolCallID: "c1"}))
	if got, want := rec.last(), "grep ErrNotWaiting +1 more"; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
}

// The model's own words before a tool call say why it is calling it, which
// no label derived from arguments can. They win, once, for that call.
func TestActivityPrefersTheModelsNarration(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := &activity{render: rec.render}

	a.apply(event(t, kit.TurnStartEvent{}))
	a.apply(event(t, kit.MessageEndEvent{Content: "Checking the deploy logs.\nThen I will report."}))
	a.apply(event(t, kit.ToolCallEvent{ToolCallID: "c1", ToolName: "read_file",
		ParsedArgs: map[string]any{"path": "deploy.log"}}))
	if got, want := rec.last(), "Checking the deploy logs."; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}

	// It is consumed: the next call is named by its own arguments.
	a.apply(event(t, kit.ToolResultEvent{ToolCallID: "c1"}))
	a.apply(event(t, kit.ToolCallEvent{ToolCallID: "c2", ToolName: "read_file",
		ParsedArgs: map[string]any{"path": "build.log"}}))
	if got, want := rec.last(), "read_file build.log"; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
}

// Reasoning streams a token at a time. A status line must follow the thought
// without costing one API call per token: a line that is visibly growing
// refreshes at once, and a line that has stalled waits.
func TestActivityRateLimitsReasoning(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := &activity{render: rec.render}

	a.apply(event(t, kit.ReasoningDeltaEvent{Delta: "Reading"}))
	a.apply(event(t, kit.ReasoningDeltaEvent{Delta: " the"}))     // +4: grew
	a.apply(event(t, kit.ReasoningDeltaEvent{Delta: " journal"})) // +8: grew
	a.apply(event(t, kit.ReasoningDeltaEvent{Delta: "."}))

	if got, want := rec.last(), "Reading the journal"; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
	// The single character did not earn its own refresh.
	if len(rec.statuses) != 3 {
		t.Fatalf("statuses = %q, want three refreshes", rec.statuses)
	}
}

// A tool in flight is the more specific thing to say, so reasoning does not
// overwrite it.
func TestActivityKeepsAToolOverReasoning(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := &activity{render: rec.render}

	a.apply(event(t, kit.ToolCallEvent{ToolCallID: "c1", ToolName: "bash",
		ParsedArgs: map[string]any{"command": "go test ./..."}}))
	a.apply(event(t, kit.ReasoningDeltaEvent{Delta: "Now I will check the tests"}))

	if got, want := rec.last(), "bash go test ./..."; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
}

// A turn that ends forgets everything, so the next turn on the same run does
// not inherit a stale tool.
func TestActivityResetsAtTheTurnBoundary(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := &activity{render: rec.render}

	a.apply(event(t, kit.TurnStartEvent{}))
	a.apply(event(t, kit.ToolCallEvent{ToolCallID: "c1", ToolName: "ls",
		ParsedArgs: map[string]any{"path": "runtime"}}))
	a.apply(runtime.Event{Type: runtime.EventState, State: runtime.RunCompleted, Time: time.Now()})

	if len(a.calls) != 0 {
		t.Fatalf("calls = %v, want none after the turn ended", a.calls)
	}
}

// The same status twice is one status: every transport pays an API call per
// line rendered.
func TestActivityRendersEachStatusOnce(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := &activity{render: rec.render}

	a.show(StatusWorking)
	a.show(StatusWorking)
	if len(rec.statuses) != 1 {
		t.Fatalf("statuses = %q, want one", rec.statuses)
	}
}

// emittingAgent is an agent that emits Kit lifecycle events the way a real
// Kit does. It satisfies the runner's optional event source by having the
// Subscribe method, which is how streaming reaches a transport at all.
type emittingAgent struct {
	mu        sync.Mutex
	listeners map[int]kit.EventListener
	next      int
	// seen closes when the tool status reached the renderer, so the turn
	// cannot finish — and unsubscribe the watcher — before the thing under
	// test happened.
	seen <-chan struct{}
	// burst is how many further tool calls the turn emits on its way out,
	// to leave events queued on the bus when the turn ends.
	burst int
}

func (a *emittingAgent) Subscribe(listener kit.EventListener) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listeners == nil {
		a.listeners = make(map[int]kit.EventListener)
	}
	a.next++
	id := a.next
	a.listeners[id] = listener
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.listeners, id)
	}
}

func (a *emittingAgent) emit(ev kit.Event) {
	a.mu.Lock()
	listeners := make([]kit.EventListener, 0, len(a.listeners))
	for _, l := range a.listeners {
		listeners = append(listeners, l)
	}
	a.mu.Unlock()
	for _, l := range listeners {
		l(ev)
	}
}

func (a *emittingAgent) PromptResult(context.Context, string) (*kit.TurnResult, error) {
	a.emit(kit.TurnStartEvent{Prompt: "ship it"})
	a.emit(kit.ToolCallEvent{ToolCallID: "c1", ToolName: "bash",
		ParsedArgs: map[string]any{"command": "go test ./..."}})
	select {
	case <-a.seen:
	case <-time.After(5 * time.Second):
	}
	// A burst on the way out: these are still queued on the bus when the
	// turn ends, which is the moment a clear can be overtaken.
	for i := range a.burst {
		a.emit(kit.ToolCallEvent{
			ToolCallID: "burst" + strconv.Itoa(i),
			ToolName:   "read_file",
			ParsedArgs: map[string]any{"path": "file" + strconv.Itoa(i) + ".go"},
		})
	}
	return &kit.TurnResult{Response: "green"}, nil
}

func (a *emittingAgent) InjectSteer(string) {}
func (a *emittingAgent) Close() error       { return nil }

// The whole wire, end to end: the agent's own lifecycle events reach the
// transport as status lines while the turn runs, and the indicator is
// cleared before the reply is delivered. Everything else in this file tests
// one link; this tests that they are joined.
func TestActivityReachesTheTransport(t *testing.T) {
	t.Parallel()

	seen := make(chan struct{})
	agent := &emittingAgent{seen: seen}
	runner := runtime.NewRunner(runtime.NewMemoryJournal(),
		func(context.Context, *runtime.Session) (runtime.Agent, error) { return agent, nil })

	var mu sync.Mutex
	var statuses []string
	var closeOnce sync.Once
	core := NewCore(runner, "test", channel.PolicySteer, WithActivity(func(_, status string) {
		mu.Lock()
		statuses = append(statuses, status)
		mu.Unlock()
		if status == "bash go test ./..." {
			closeOnce.Do(func() { close(seen) })
		}
	}))

	delivered := make(chan *runtime.Run, 1)
	Dispatch(context.Background(), core, Turn{Address: "c1", Text: "run the tests"},
		func(_ string, run *runtime.Run, err error) {
			if err != nil {
				t.Errorf("deliver: %v", err)
			}
			delivered <- run
		})

	select {
	case run := <-delivered:
		if run.Response != "green" {
			t.Fatalf("response = %q, want the turn's answer", run.Response)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never delivered")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{StatusThinking, StatusWorking, "bash go test ./...", ""}
	if len(statuses) != len(want) {
		t.Fatalf("statuses = %q, want %q", statuses, want)
	}
	for i := range want {
		if statuses[i] != want[i] {
			t.Errorf("status %d = %q, want %q", i, statuses[i], want[i])
		}
	}
}

// The clear is always the last word. A status still in flight when the turn
// ends must not land after it: on a transport that posted a placeholder,
// the thread would keep "Working…" above the answer for ever.
func TestActivityClearIsAlwaysLast(t *testing.T) {
	t.Parallel()

	seen := make(chan struct{})
	agent := &emittingAgent{seen: seen, burst: 200}
	runner := runtime.NewRunner(runtime.NewMemoryJournal(),
		func(context.Context, *runtime.Session) (runtime.Agent, error) { return agent, nil })

	var mu sync.Mutex
	var statuses []string
	var closeOnce sync.Once
	core := NewCore(runner, "test", channel.PolicySteer, WithActivity(func(_, status string) {
		// Recorded on entry, so the order is the order the renders were
		// started in — which is what "the clear is last" has to mean.
		mu.Lock()
		statuses = append(statuses, status)
		mu.Unlock()
		if status == "" {
			// A slow clear widens the window a racing status would use.
			time.Sleep(50 * time.Millisecond)
		}
		if status == "bash go test ./..." {
			closeOnce.Do(func() { close(seen) })
		}
	}))

	delivered := make(chan struct{})
	Dispatch(context.Background(), core, Turn{Address: "c1", Text: "run the tests"},
		func(string, *runtime.Run, error) { close(delivered) })

	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never delivered")
	}

	// The runner publishes the turn's closing events while the clear is in
	// flight; none of them may be rendered after it.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(statuses) == 0 {
		t.Fatal("no status was rendered")
	}
	if last := statuses[len(statuses)-1]; last != "" {
		// The burst is long; the tail is the part that shows the fault.
		tail := statuses
		if len(tail) > 4 {
			tail = tail[len(tail)-4:]
		}
		t.Fatalf("a status was rendered after the clear: …%q, want the clear last", tail)
	}
}

func TestToolLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		tool   string
		parsed map[string]any
		raw    string
		want   string
	}{
		{"a tool's own key wins", "grep", map[string]any{"path": "runtime", "pattern": "Halt"}, "", "grep Halt"},
		{"an unknown tool is probed generically", "jira_issue", map[string]any{"id": "PROJ-42"}, "", "jira_issue PROJ-42"},
		{"arguments come from the raw JSON when Kit did not parse them", "read_file", nil, `{"path":"go.mod"}`, "read_file go.mod"},
		{"no readable argument leaves the name", "weather", map[string]any{"lat": true}, "", "weather"},
		{"unparseable arguments leave the name", "weather", nil, "{not json", "weather"},
		{"a number reads as a person writes it", "issue", map[string]any{"id": float64(42)}, "", "issue 42"},
		{"a long path keeps its telling end", "read_file", map[string]any{"path": "agent/lib/triage/incident/response/cards.go"}, "", "read_file response/cards.go"},
		{"a long command is cut, not folded", "bash", map[string]any{"command": "go test -race ./... -run TestTheWholeThingEndToEnd"}, "", "bash go test -race ./... -run TestTheWholeTh…"},
		{"a command is one line", "bash", map[string]any{"command": "cd /tmp\nls"}, "", "bash cd /tmp"},
		{"an empty name is no label", "", map[string]any{"path": "x"}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ToolLabel(c.tool, c.parsed, c.raw); got != c.want {
				t.Errorf("ToolLabel(%q) = %q, want %q", c.tool, got, c.want)
			}
		})
	}
}

// A status is read at a glance and crosses a platform's length limit, so it
// is one line, single-spaced, and capped by runes rather than bytes.
func TestTruncateStatus(t *testing.T) {
	t.Parallel()
	if got, want := TruncateStatus("  Reading\n  the   journal  "), "Reading the journal"; got != want {
		t.Errorf("TruncateStatus = %q, want %q", got, want)
	}
	// Multi-byte runes: a byte cut would split one and the platform would
	// render the wreckage.
	long := TruncateStatus(strings.Repeat("ü", 80))
	if runes := []rune(long); len(runes) != StatusLimit {
		t.Errorf("TruncateStatus kept %d runes, want %d", len(runes), StatusLimit)
	}
	if !strings.HasSuffix(long, "…") {
		t.Errorf("TruncateStatus = %q, want a marked cut", long)
	}
}
