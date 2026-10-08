package inspect

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mark3labs/bonnie/runtime"
)

type testJournal struct {
	ids     []string
	records map[string][]runtime.Record
	states  map[string]runtime.RunState
}

func (j testJournal) Runs(context.Context, runtime.RunState) ([]string, error) { return j.ids, nil }
func (j testJournal) Replay(_ context.Context, id string) ([]runtime.Record, error) {
	return j.records[id], nil
}
func (j testJournal) State(_ context.Context, id string) (runtime.RunState, error) {
	return j.states[id], nil
}

func TestInspectorLoadsListAndSkipsReservedRuns(t *testing.T) {
	t.Parallel()
	j := testJournal{ids: []string{"bonnie.internal", "run-1"}, records: map[string][]runtime.Record{"run-1": {{Seq: 1, Kind: runtime.RecordStep}, {Seq: 2, Kind: runtime.RecordMessage, Text: "hello", Role: "user", Timestamp: time.Now()}}}, states: map[string]runtime.RunState{"run-1": runtime.RunWaiting}}
	m := newModel(context.Background(), j, "")
	m.loading = false
	msg := m.load(1, "")().(loadedMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if len(msg.items) != 1 || msg.selected != "run-1" || msg.state != runtime.RunWaiting {
		t.Fatalf("loaded = %#v", msg)
	}
	if msg.items[0].steps != 1 || msg.items[0].last != "hello" {
		t.Fatalf("summary = %#v", msg.items[0])
	}
	m.generation = 1
	next, _ := m.Update(msg)
	got := next.(model)
	if got.selected != "run-1" || len(got.records) != 2 {
		t.Fatalf("model did not load selected run: %#v", got)
	}
}

func TestInspectorRefreshAndHelpAreReadOnly(t *testing.T) {
	t.Parallel()
	j := testJournal{ids: []string{"run-1"}, records: map[string][]runtime.Record{"run-1": {}}, states: map[string]runtime.RunState{"run-1": runtime.RunCompleted}}
	m := newModel(context.Background(), j, "run-1")
	m.loading = false
	next, cmd := m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	m = next.(model)
	if !m.help || cmd != nil {
		t.Fatal("help key did not toggle help")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'r', Text: "r"}); cmd == nil {
		t.Fatal("refresh key did not request a journal load")
	}
}
