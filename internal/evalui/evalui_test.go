package evalui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/eval"
	"github.com/mark3labs/bonnie/runtime"
)

// The CLI copies the names and record shapes of package eval so its binary
// does not link package testing. The copies must stay equal.
func TestNamesMatchEval(t *testing.T) {
	t.Parallel()
	for got, want := range map[string]string{
		ReportPrefix: eval.ReportPrefix, RunPrefix: eval.RunPrefix, EnvJudge: eval.EnvJudge,
		EnvLLMJudge: eval.EnvLLMJudge, EnvStrict: eval.EnvStrict, EnvDir: eval.EnvDir, RunID: eval.RunID,
	} {
		if got != want {
			t.Errorf("copied name %q, want %q", got, want)
		}
	}
	for _, pair := range [][2]reflect.Type{
		{reflect.TypeFor[Report](), reflect.TypeFor[eval.Report]()},
		{reflect.TypeFor[runInfo](), reflect.TypeFor[eval.RunInfo]()},
	} {
		a, b := pair[0], pair[1]
		if a.NumField() != b.NumField() {
			t.Fatalf("%s has %d fields, %s %d", a, a.NumField(), b, b.NumField())
		}
		for i := range a.NumField() {
			fa, fb := a.Field(i), b.Field(i)
			if fa.Name != fb.Name || fa.Type != fb.Type || fa.Tag != fb.Tag {
				t.Errorf("%s field %d: %s %s %s, want %s %s %s", a, i, fa.Name, fa.Type, fa.Tag, fb.Name, fb.Type, fb.Tag)
			}
		}
	}
}

// events encodes go test -json events, one for each line.
func events(t *testing.T, evs ...testEvent) [][]byte {
	t.Helper()
	var out [][]byte
	for _, ev := range evs {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

const (
	reportPolite  = `{"test":"TestA","label":"polite","kind":"judge","value":0.9,"min":0.8,"status":"passed"}`
	reportClarity = `{"test":"TestB/case","label":"The response is clear and names the policy that applies.","kind":"llm","value":0.2,"min":0.5,"status":"scored","reason":"vague"}`
)

// sampleSession is two evals: TestA passes, TestB/case fails.
func sampleSession(t *testing.T, journal string) *Session {
	t.Helper()
	s := NewSession("20261029-120000", t.TempDir(), "/trees/support")
	for _, line := range events(t,
		testEvent{Action: "run", Package: "p", Test: "TestA"},
		testEvent{Action: "output", Package: "p", Test: "TestA", Output: "    eval.go:1: " + ReportPrefix + reportPolite + "\n"},
		testEvent{Action: "output", Package: "p", Test: "TestA", Output: "    eval.go:1: " + RunPrefix + `{"test":"TestA","journal":"` + journal + `","turns":1}` + "\n"},
		testEvent{Action: "output", Package: "p", Test: "TestA", Output: "    noise that a passing test hides\n"},
		testEvent{Action: "pass", Package: "p", Test: "TestA", Elapsed: 1.5},
		testEvent{Action: "run", Package: "p", Test: "TestB"},
		testEvent{Action: "run", Package: "p", Test: "TestB/case"},
		testEvent{Action: "output", Package: "p", Test: "TestB/case", Output: "=== RUN   TestB/case\n"},
		testEvent{Action: "output", Package: "p", Test: "TestB/case", Output: "    a.go:3: bonnie: eval: check failed: why\n"},
		testEvent{Action: "output", Package: "p", Test: "TestB/case", Output: "    eval.go:1: " + ReportPrefix + reportClarity + "\n"},
		testEvent{Action: "fail", Package: "p", Test: "TestB/case"},
		testEvent{Action: "fail", Package: "p", Test: "TestB"},
		testEvent{Action: "fail", Package: "p"},
	) {
		s.Feed(line)
	}
	s.Finish(nil)
	return s
}

// Feed attaches reports and the kept journal to their eval, counts only
// leaf tests, and keeps the output of each eval.
func TestFeed(t *testing.T) {
	t.Parallel()
	snap := sampleSession(t, "/j/TestA").Snapshot()
	if len(snap.Evals) != 2 || snap.Evals[0].Name != "TestA" || snap.Evals[1].Name != "TestB/case" {
		t.Fatalf("evals = %+v", snap.Evals)
	}
	a, b := snap.Evals[0], snap.Evals[1]
	if a.Journal != "/j/TestA" || a.Turns != 1 || len(a.Reports) != 1 || a.Status != StatusPass {
		t.Fatalf("TestA = %+v", a)
	}
	if b.Status != StatusFail || len(b.Reports) != 1 || b.Reports[0].Reason != "vague" {
		t.Fatalf("TestB/case = %+v", b)
	}
	if got := b.Failure(); len(got) != 1 || !strings.Contains(got[0], "check failed: why") {
		t.Fatalf("failure = %q", got)
	}
	c := snap.Counts()
	if c.Evals != 2 || c.Passed != 1 || c.Failed != 1 || c.Scores["passed"] != 1 || c.Scores["scored"] != 1 {
		t.Fatalf("counts = %+v", c)
	}
	if snap.Err != "" {
		t.Fatalf("err = %q", snap.Err)
	}
}

// The plain table cuts a long label and shows it in full below, and shows
// the output of a failed eval only.
func TestWritePlain(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := WritePlain(&out, sampleSession(t, "").Snapshot()); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"PASS  TestA  1.5s", "judge    polite", ">= 0.800", "FAIL  TestB/case", "check failed: why",
		"reason: vague", "label: The response is clear and names the policy that applies.",
		"evals: 1 passed, 1 failed, 0 skipped; scores: 1 passed, 1 scored, 0 failed, 0 recorded", "saved: "} {
		if !strings.Contains(text, want) {
			t.Errorf("output does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "noise") || strings.Contains(text, "FAIL  TestB  ") || strings.Contains(text, "=== RUN") {
		t.Errorf("output shows a passing test's log, a parent test, or framing:\n%s", text)
	}
	// The columns stay aligned: the value starts at the same cell in each row.
	var cols []int
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, "      judge") || strings.HasPrefix(line, "      llm") {
			cols = append(cols, utf8.RuneCountInString(line[:strings.Index(line, "0.")]))
		}
	}
	if len(cols) != 2 || cols[0] != cols[1] {
		t.Errorf("value columns = %v:\n%s", cols, text)
	}

	out.Reset()
	if err := WriteJSON(&out, sampleSession(t, "").Snapshot()); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 2 || !strings.Contains(lines[1], `"kind":"llm"`) {
		t.Fatalf("json = %q", out.String())
	}
}

// A build failure reaches the output: go test -json sends it as build
// events with no package, then fails the package with no test. go test's
// error is kept only when no eval failed.
func TestFeedBuildFailure(t *testing.T) {
	t.Parallel()
	s := NewSession("id", t.TempDir(), "")
	for _, line := range []string{
		`{"ImportPath":"example/agent [example/agent.test]","Action":"build-output","Output":"./echo_eval_test.go:9:2: undefined: newAgnt\n"}`,
		`{"ImportPath":"example/agent [example/agent.test]","Action":"build-fail"}`,
		`{"Action":"start","Package":"example/agent"}`,
		`{"Action":"output","Package":"example/agent","Output":"FAIL\texample/agent [build failed]\n"}`,
		`{"Action":"fail","Package":"example/agent","FailedBuild":"example/agent [example/agent.test]"}`,
		`not json`,
	} {
		s.Feed([]byte(line))
	}
	s.Finish(os.ErrInvalid)
	snap := s.Snapshot()
	build := strings.Join(snap.Build, "\n")
	for _, want := range []string{"undefined: newAgnt", "FAIL example/agent", "[build failed]", "not json"} {
		if !strings.Contains(build, want) {
			t.Errorf("build output does not contain %q:\n%s", want, build)
		}
	}
	if snap.Err == "" || len(snap.Evals) != 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

// A session saves and loads, and Prune keeps the newest runs and the
// current one.
func TestStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.Local)
	var ids []string
	for i := range 4 {
		id, dir, err := NewRun(root, start.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		s := NewSession(id, dir, "/tree")
		s.Feed(events(t, testEvent{Action: "run", Package: "p", Test: "TestA"}, testEvent{Action: "pass", Package: "p", Test: "TestA"})[0])
		s.Finish(nil)
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Two runs in the same second get different IDs.
	twin, _, err := NewRun(root, start)
	if err != nil || twin != ids[0]+"-2" {
		t.Fatalf("twin = %q, %v", twin, err)
	}
	loaded, err := Load(filepath.Join(root, ids[2]))
	if err != nil || loaded.Snapshot().ID != ids[2] || len(loaded.Snapshot().Evals) != 1 {
		t.Fatalf("load: %v %+v", err, loaded)
	}
	if err := Prune(root, ids[0], 2); err != nil {
		t.Fatal(err)
	}
	got, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{ids[3], ids[2], ids[0]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after prune = %v, want %v", got, want)
	}
	// The twin has no results and is new: it can be the active run of
	// another bonnie eval, so it stays.
	if _, err := os.Stat(filepath.Join(root, twin)); err != nil {
		t.Fatalf("a new run with no results was removed: %v", err)
	}
}

// Prune removes a run directory with no results only when it is old
// enough to be what a crash left. A newer one can belong to a concurrent
// bonnie eval that has not saved yet.
func TestPruneKeepsActiveRuns(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now()
	_, active, err := NewRun(root, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(active, "journals", "TestA"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, crashed, err := NewRun(root, now.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * unsavedGrace)
	if err := os.Chtimes(crashed, old, old); err != nil {
		t.Fatal(err)
	}
	id, dir, err := NewRun(root, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(id, dir, "")
	s.Finish(nil)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if err := Prune(root, id, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(active, "journals", "TestA")); err != nil {
		t.Fatalf("the active run of another process was removed: %v", err)
	}
	if _, err := os.Stat(crashed); !os.IsNotExist(err) {
		t.Fatalf("an old run with no results was kept: %v", err)
	}
}

// The conversation shows text, tool calls, results and decisions from the
// journal.
func TestConversation(t *testing.T) {
	t.Parallel()
	msg := func(m kit.LLMMessage) runtime.Record {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return runtime.Record{Kind: runtime.RecordMessage, Role: string(m.Role), Payload: b}
	}
	decision, err := json.Marshal(runtime.DecisionRecord{Answers: []kit.DecisionAnswer{{ID: "destructive", Type: kit.DecisionBoolean, Probability: 0.12}}})
	if err != nil {
		t.Fatal(err)
	}
	recs := []runtime.Record{
		msg(kit.LLMMessage{Role: kit.LLMRoleUser, Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "Refund order 7."}}}),
		msg(kit.LLMMessage{Role: kit.LLMRoleAssistant, Content: []kit.LLMMessagePart{kit.LLMToolCallPart{ToolCallID: "c1", ToolName: "lookup", Input: `{"id":7}`}}}),
		msg(kit.LLMMessage{Role: kit.LLMRoleTool, Content: []kit.LLMMessagePart{kit.LLMToolResultPart{ToolCallID: "c1", Output: kit.LLMToolResultOutputContentText{Text: "paid"}}}}),
		{Kind: runtime.RecordDecision, Payload: decision},
		msg(kit.LLMMessage{Role: kit.LLMRoleAssistant, Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "Done."}}}),
	}
	text := ansi.Strip(strings.Join(renderConversation(conversation(recs), 60), "\n"))
	for _, want := range []string{"you", "Refund order 7.", `→ lookup {"id":7}`, "← lookup: paid", "◆ decision destructive P=0.12", "agent", "Done."} {
		if !strings.Contains(text, want) {
			t.Errorf("conversation does not contain %q:\n%s", want, text)
		}
	}
}

// keyMsg is a key press for the model.
func keyMsg(s string) tea.KeyPressMsg {
	switch s {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

func press(t *testing.T, m model, keys ...string) (model, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, k := range keys {
		var next tea.Model
		next, cmd = m.Update(keyMsg(k))
		m = next.(model)
	}
	return m, cmd
}

func screen(m model) string {
	return ansi.Strip(m.View().Content)
}

// The screen lists the evals with their status, shows the scores of the
// selected one, filters to failed evals, and opens a kept journal in the
// run console.
func TestModel(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), sampleSession(t, "/j/TestA"), "")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m = next.(model)
	view := screen(m)
	for _, want := range []string{"BONNIE eval · support · 20261029-120000", "FAIL", "✓ 1", "✗ 1", "✓ TestA", "✗ TestB/case", "1 Scores", "polite", "0.900", ">= 0.800", "journal /j/TestA", "o open run console"} {
		if !strings.Contains(view, want) {
			t.Errorf("screen does not contain %q:\n%s", want, view)
		}
	}

	m, _ = press(t, m, "down")
	view = screen(m)
	for _, want := range []string{"The response is clear", "↳ vague", "Failures", "check failed: why"} {
		if !strings.Contains(view, want) {
			t.Errorf("screen of TestB/case does not contain %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "o open run console") {
		t.Error("an eval with no journal offers the run console")
	}

	m, _ = press(t, m, "f")
	if len(m.items) != 1 || m.snap.Evals[m.items[0].eval].Name != "TestB/case" {
		t.Fatalf("failed-only items = %+v", m.items)
	}
	m, _ = press(t, m, "f", "g")
	m, cmd := press(t, m, "o")
	if m.out.open != "/j/TestA" || m.out.selected != "TestA" || cmd == nil {
		t.Fatalf("open = %+v", m.out)
	}

	// Coming back from the console selects the same eval.
	back := newModel(context.Background(), sampleSession(t, "/j/TestA"), "TestB/case")
	if back.selName != "TestB/case" || back.sel != 1 {
		t.Fatalf("selection after return = %d %q", back.sel, back.selName)
	}
}

// While evals run, q asks first, and a second q stops them.
func TestModelStopWhileRunning(t *testing.T) {
	t.Parallel()
	s := NewSession("id", t.TempDir(), "/tree")
	s.Feed(events(t, testEvent{Action: "run", Package: "p", Test: "TestSlow"})[0])
	m := newModel(context.Background(), s, "")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	m = next.(model)
	if view := screen(m); !strings.Contains(view, "1 running") || !strings.Contains(view, "TestSlow") {
		t.Fatalf("running screen:\n%s", view)
	}
	m, cmd := press(t, m, "q")
	if cmd != nil || !m.confirmStop || !strings.Contains(screen(m), "Press q again to stop them") {
		t.Fatal("the first q did not ask")
	}
	m, _ = press(t, m, "q")
	if !m.out.stopped {
		t.Fatal("the second q did not stop the evals")
	}

	// A change of the session reaches the screen.
	s.Feed(events(t, testEvent{Action: "pass", Package: "p", Test: "TestSlow", Elapsed: 2})[0])
	s.Finish(nil)
	next, _ = m.Update(changedMsg{})
	if view := screen(next.(model)); !strings.Contains(view, "PASS") || !strings.Contains(view, "2.0s") {
		t.Fatalf("finished screen:\n%s", view)
	}
}

// r reruns the selected eval and R each failed eval, once the run has
// ended. The screen stays: the rerun's session replaces the old one in
// place, with the rerun evals running from the start.
func TestModelRerun(t *testing.T) {
	t.Parallel()
	prev := sampleSession(t, "")
	var asked []string
	rerun := func(p *Session, evals []Result) (*Session, error) {
		asked = asked[:0]
		for _, r := range evals {
			asked = append(asked, r.Name)
		}
		s := NewSession("20261029-121000", t.TempDir(), p.Tree)
		s.Seed(p, evals)
		return s, nil
	}
	m := newModel(context.Background(), prev, "")
	m.rerun = rerun
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m = next.(model)
	if view := screen(m); !strings.Contains(view, "r rerun") || !strings.Contains(view, "R rerun failed") {
		t.Fatalf("the footer does not offer a rerun:\n%s", view)
	}

	failed, cmd := press(t, m, "R")
	if cmd == nil || !failed.starting || !strings.Contains(screen(failed), "starting the rerun") {
		t.Fatal("R did not start a rerun")
	}
	// q waits while the rerun starts.
	if waiting, qcmd := press(t, failed, "q"); qcmd != nil || !strings.Contains(screen(waiting), "quit in a moment") {
		t.Fatal("q quit while the rerun started")
	}
	msg := cmd()
	if len(asked) != 1 || asked[0] != "TestB/case" {
		t.Fatalf("R asked for %q", asked)
	}
	next, _ = failed.Update(msg)
	m = next.(model)
	view := screen(m)
	for _, want := range []string{"rerun of 20261029-120000", "1 running", "✓ TestA", "TestB/case"} {
		if !strings.Contains(view, want) {
			t.Errorf("the rerun screen does not contain %q:\n%s", want, view)
		}
	}
	if m.session == prev || m.snap.Evals[1].Status != StatusRunning || m.snap.Evals[0].Status != StatusPass {
		t.Fatalf("the screen did not switch to the rerun: %+v", m.snap.Evals)
	}
	// No rerun while one runs.
	if got, c := press(t, m, "r"); c != nil || got.starting {
		t.Fatal("r reran while evals run")
	}

	// r reruns the selected eval.
	one, cmd := press(t, newModelWith(prev, rerun), "r")
	if cmd == nil || !one.starting {
		t.Fatal("r did not start a rerun")
	}
	cmd()
	if len(asked) != 1 || asked[0] != "TestA" {
		t.Fatalf("r asked for %q", asked)
	}

	// A rerun that cannot start says why, and the old session stays.
	broken := newModelWith(prev, func(*Session, []Result) (*Session, error) { return nil, os.ErrPermission })
	broken, cmd = press(t, broken, "r")
	next, _ = broken.Update(cmd())
	if got := next.(model); got.session != prev || got.starting || !strings.Contains(screen(got), "permission denied") {
		t.Fatalf("failed rerun screen:\n%s", screen(got))
	}
}

func newModelWith(s *Session, rerun RerunFunc) model {
	m := newModel(context.Background(), s, "")
	m.rerun = rerun
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	return next.(model)
}

// A rerun eval that go test never reports, for example after a build
// failure, ends as failed with a reason, not as running.
func TestSeedQueuedNotRun(t *testing.T) {
	t.Parallel()
	prev := sampleSession(t, "")
	s := NewSession("20261029-121000", t.TempDir(), "")
	s.Seed(prev, []Result{{Package: "p", Name: "TestA"}})
	if a := s.Snapshot().Evals[0]; a.Status != StatusRunning || a.Carried() || len(a.Reports) != 0 {
		t.Fatalf("queued TestA = %+v", a)
	}
	s.Finish(os.ErrInvalid)
	a := s.Snapshot().Evals[0]
	if a.Status != StatusFail || !strings.Contains(strings.Join(a.Output, ""), "did not run this eval") {
		t.Fatalf("TestA after a run without it = %+v", a)
	}
}

// A rerun session starts with the results of the run before it. A test
// that runs again replaces its result in place; the others keep theirs,
// with the run that made them. The counts are those of the whole suite.
func TestSeedRerun(t *testing.T) {
	t.Parallel()
	prev := sampleSession(t, "/j/TestA")
	s := NewSession("20261029-121000", t.TempDir(), "/trees/support")
	s.Seed(prev, nil)
	if snap := s.Snapshot(); snap.From != prev.ID || len(snap.Evals) != 2 || !snap.Evals[1].Carried() {
		t.Fatalf("seeded = %+v", snap)
	}
	for _, line := range events(t,
		testEvent{Action: "run", Package: "p", Test: "TestB"},
		testEvent{Action: "run", Package: "p", Test: "TestB/case"},
	) {
		s.Feed(line)
	}
	if b := s.Snapshot().Evals[1]; b.Name != "TestB/case" || b.Status != StatusRunning || b.Carried() || len(b.Reports) != 0 {
		t.Fatalf("running rerun = %+v", b)
	}
	for _, line := range events(t,
		testEvent{Action: "pass", Package: "p", Test: "TestB/case", Elapsed: 0.5},
		testEvent{Action: "pass", Package: "p", Test: "TestB"},
		testEvent{Action: "pass", Package: "p"},
	) {
		s.Feed(line)
	}
	s.Finish(nil)
	snap := s.Snapshot()
	if len(snap.Evals) != 2 || snap.Evals[0].Name != "TestA" || snap.Evals[1].Name != "TestB/case" {
		t.Fatalf("evals = %+v", snap.Evals)
	}
	a, b := snap.Evals[0], snap.Evals[1]
	if a.Run != prev.ID || a.Journal != "/j/TestA" || len(a.Reports) != 1 {
		t.Fatalf("carried TestA = %+v", a)
	}
	if b.Carried() || b.Status != StatusPass {
		t.Fatalf("rerun TestB/case = %+v", b)
	}
	if c := snap.Counts(); c.Passed != 2 || c.Failed != 0 {
		t.Fatalf("counts = %+v", c)
	}

	// The next rerun keeps the run of each result, not the last run.
	next := NewSession("20261029-122000", t.TempDir(), "")
	next.Seed(s, nil)
	if got := next.Snapshot().Evals; got[0].Run != prev.ID || got[1].Run != s.ID {
		t.Fatalf("runs = %q, %q", got[0].Run, got[1].Run)
	}

	// A build failure in a rerun shows, though the package has carried
	// results.
	broken := NewSession("20261029-123000", t.TempDir(), "")
	broken.Seed(prev, nil)
	broken.Feed([]byte(`{"Action":"output","Package":"p","Output":"FAIL\tp [build failed]\n"}`))
	broken.Feed([]byte(`{"Action":"fail","Package":"p"}`))
	if build := strings.Join(broken.Snapshot().Build, "\n"); !strings.Contains(build, "FAIL p") {
		t.Fatalf("build = %q", build)
	}
}

// Prune keeps a run whose results a kept rerun carries, so their journals
// stay.
func TestPruneKeepsCarriedRuns(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.Local)
	var sessions []*Session
	for i := range 3 {
		id, dir, err := NewRun(root, start.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		s := NewSession(id, dir, "")
		if i == 0 {
			s.Feed(events(t, testEvent{Action: "run", Package: "p", Test: "TestA"})[0])
			s.Feed(events(t, testEvent{Action: "pass", Package: "p", Test: "TestA"})[0])
		}
		if i == 2 {
			s.Seed(sessions[0], nil)
		}
		s.Finish(nil)
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	if err := Prune(root, sessions[2].ID, 1); err != nil {
		t.Fatal(err)
	}
	got, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{sessions[2].ID, sessions[0].ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after prune = %v, want %v", got, want)
	}
}

// The screen of a rerun marks a carried result with the time of its run.
func TestModelCarried(t *testing.T) {
	t.Parallel()
	s := NewSession("20261029-121000", t.TempDir(), "/trees/support")
	s.Seed(sampleSession(t, "/j/TestA"), nil)
	s.Finish(nil)
	m := newModel(context.Background(), s, "TestA")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	view := screen(next.(model))
	for _, want := range []string{"rerun of 20261029-120000", "12:00:00 1.5s", "From run 20261029-120000"} {
		if !strings.Contains(view, want) {
			t.Errorf("screen does not contain %q:\n%s", want, view)
		}
	}
	if runTime("20261029-200000-2") != "20:00:00" || runTime("other") != "other" {
		t.Error("runTime")
	}
}

// When go test fails although each eval passed, the plain table says why.
func TestWritePlainShowsGoTestError(t *testing.T) {
	t.Parallel()
	s := NewSession("id", t.TempDir(), "")
	for _, line := range events(t,
		testEvent{Action: "run", Package: "p", Test: "TestParent"},
		testEvent{Action: "run", Package: "p", Test: "TestParent/case"},
		testEvent{Action: "pass", Package: "p", Test: "TestParent/case"},
		testEvent{Action: "fail", Package: "p", Test: "TestParent"},
		testEvent{Action: "fail", Package: "p"},
	) {
		s.Feed(line)
	}
	s.Finish(errors.New("exit status 1"))
	var out bytes.Buffer
	if err := WritePlain(&out, s.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "go test failed: exit status 1") || !strings.Contains(out.String(), "evals: 1 passed, 0 failed") {
		t.Fatalf("output:\n%s", out.String())
	}
}
