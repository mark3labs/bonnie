package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/runtime"
)

// An explicit run ID bypasses address resolution for history, streams, and turns.
func TestSelectedRunHistoryAndTurns(t *testing.T) {
	t.Parallel()
	for _, state := range []runtime.RunState{runtime.RunCompleted, runtime.RunWaiting} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			f := &fakeClient{lookupRun: "wrong-run", snapshot: &client.Snapshot{
				RunID: "selected-run", Cursor: 12, State: state,
				Messages: []kit.LLMMessage{kit.NewLLMUserMessage("saved message")},
			}}
			m := NewRun(f, context.Background(), "selected-run")
			next, _ := m.Update(m.lookup()())
			m = next.(Model)
			if m.runID != "selected-run" || f.snapshotRun != "selected-run" || f.lookups != 0 {
				t.Fatalf("wrong run: model=%q snapshot=%q lookups=%d", m.runID, f.snapshotRun, f.lookups)
			}
			if len(m.entries) != 1 || m.entries[0].text != "saved message" {
				t.Fatalf("history = %+v", m.entries)
			}
			_ = m.openStream()()
			if f.streamRun != "selected-run" || f.streamAfter[0] != 12 {
				t.Fatalf("stream = %q at %v", f.streamRun, f.streamAfter)
			}
			_, cmd := m.send("next message")
			for _, step := range cmd().(tea.BatchMsg) {
				_ = step()
			}
			if state == runtime.RunWaiting {
				if f.respondRun != "selected-run" {
					t.Fatalf("response run = %q", f.respondRun)
				}
			} else if f.sentRun != "selected-run" {
				t.Fatalf("send run = %q", f.sentRun)
			}
			if f.started || f.ensured != 0 {
				t.Fatal("selected run created an address conversation")
			}
			m.spin = false
			next, _ = m.slash("/new")
			if next.(Model).selectedRun != "" {
				t.Fatal("/new kept the selected run")
			}
		})
	}
}

// A missing ID is not a new address. Input must not create a replacement run.
func TestSelectedRunNotFound(t *testing.T) {
	t.Parallel()
	f := &fakeClient{snapshotErr: ErrNotFound}
	m := NewRun(f, context.Background(), "missing-run")
	next, cmd := m.Update(m.lookup()())
	m = next.(Model)
	if cmd != nil || m.state != statusError || m.runID != "" {
		t.Fatalf("missing run: state=%v run=%q", m.state, m.runID)
	}
	_, cmd = m.send("hello")
	if cmd != nil || f.started || f.ensured != 0 || f.lookups != 0 {
		t.Fatal("missing run used address resolution")
	}
}
