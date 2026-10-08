package inspect

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mark3labs/bonnie/runtime"
)

func keyMsg(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Text: s}
}

func TestInspectorFilteringRoutesPrintableKeysToList(t *testing.T) {
	t.Parallel()
	j := testJournal{ids: []string{"run-r", "other"}, records: map[string][]runtime.Record{}, states: map[string]runtime.RunState{}}
	m := newModel(context.Background(), j, "")
	m.generation = 1
	next, _ := m.Update(m.load(1, "")())
	m = next.(model)
	m.list.SetFilterState(list.Filtering)
	for _, s := range []string{"r", "?", "q"} {
		next, _ = m.Update(keyMsg(s))
		m = next.(model)
	}
	if m.list.FilterValue() != "r?q" {
		t.Fatalf("filter input = %q, want %q", m.list.FilterValue(), "r?q")
	}
	// Bubbles filters asynchronously. Apply a matching query before accepting it.
	m.list.SetFilterText("run")
	m.list.SetFilterState(list.Filtering)
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)
	if m.list.FilterState() != list.FilterApplied {
		t.Fatalf("filter state = %v, want applied", m.list.FilterState())
	}
}

func TestInspectorQuitAndTabFocus(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), testJournal{}, "")
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if _, ok := next.(model); !ok || cmd == nil {
		t.Fatal("ctrl+c did not request quit")
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = next.(model)
	if m.focus != 1 {
		t.Fatalf("focus = %d, want detail panel", m.focus)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if got := next.(model).focus; got != 0 {
		t.Fatalf("focus after second tab = %d, want list", got)
	}
}

func TestInspectorIgnoresStaleLoad(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), testJournal{}, "current")
	m.generation = 2
	m.loading = true
	next, cmd := m.Update(loadedMsg{generation: 1, selected: "stale", err: errors.New("old error")})
	got := next.(model)
	if cmd != nil || got.selected != "current" || got.err != nil || !got.loading {
		t.Fatalf("stale result changed model: %#v", got)
	}
}

func TestInspectorSelectsExplicitRunInList(t *testing.T) {
	t.Parallel()
	j := testJournal{ids: []string{"first", "chosen", "last"}, records: map[string][]runtime.Record{}, states: map[string]runtime.RunState{}}
	m := newModel(context.Background(), j, "chosen")
	m.generation = 1
	next, _ := m.Update(m.load(1, "chosen")())
	m = next.(model)
	if m.selected != "chosen" || m.list.Index() != 1 {
		t.Fatalf("selected=%q, list index=%d; want chosen at index 1", m.selected, m.list.Index())
	}
}

type errorJournal struct {
	testJournal
	runsErr, replayErr, stateErr error
}

func (j errorJournal) Runs(ctx context.Context, state runtime.RunState) ([]string, error) {
	if j.runsErr != nil {
		return nil, j.runsErr
	}
	return j.testJournal.Runs(ctx, state)
}
func (j errorJournal) Replay(ctx context.Context, id string) ([]runtime.Record, error) {
	if j.replayErr != nil {
		return nil, j.replayErr
	}
	return j.testJournal.Replay(ctx, id)
}
func (j errorJournal) State(ctx context.Context, id string) (runtime.RunState, error) {
	if j.stateErr != nil {
		return "", j.stateErr
	}
	return j.testJournal.State(ctx, id)
}

func TestInspectorReportsMissingRunAndJournalErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		journal  Journal
		selected string
		want     string
	}{
		{name: "missing run", journal: testJournal{}, selected: "absent", want: `run "absent" not found`},
		{name: "runs error", journal: errorJournal{runsErr: errors.New("runs failed")}, want: "runs failed"},
		{name: "replay error", journal: errorJournal{testJournal: testJournal{ids: []string{"run"}}, replayErr: errors.New("replay failed")}, want: "replay failed"},
		{name: "state error", journal: errorJournal{testJournal: testJournal{ids: []string{"run"}}, stateErr: errors.New("state failed")}, want: "state failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := newModel(context.Background(), tt.journal, tt.selected)
			msg := m.load(1, tt.selected)().(loadedMsg)
			if msg.err == nil || !strings.Contains(msg.err.Error(), tt.want) {
				t.Fatalf("load error = %v, want containing %q", msg.err, tt.want)
			}
		})
	}
}

func TestInspectorViewFitsNormalTerminalSizes(t *testing.T) {
	t.Parallel()
	for _, size := range []struct{ width, height int }{{60, 20}, {120, 30}} {
		t.Run(strings.Join([]string{string(rune(size.width)), string(rune(size.height))}, "x"), func(t *testing.T) {
			t.Parallel()
			m := newModel(context.Background(), testJournal{}, "")
			next, _ := m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			view := next.(model).View().Content
			w, h := lipgloss.Size(view)
			if w > size.width || h > size.height {
				t.Fatalf("view size %dx%d exceeds terminal %dx%d", w, h, size.width, size.height)
			}
		})
	}
}
