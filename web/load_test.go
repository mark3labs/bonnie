package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	httpchannel "github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/runtime"
)

type loadJournal struct {
	runtime.Journal
	runs, states, replays atomic.Int64
}

func (j *loadJournal) Runs(ctx context.Context, state runtime.RunState) ([]string, error) {
	j.runs.Add(1)
	return j.Journal.Runs(ctx, state)
}
func (j *loadJournal) State(ctx context.Context, id string) (runtime.RunState, error) {
	j.states.Add(1)
	return j.Journal.State(ctx, id)
}
func (j *loadJournal) Replay(ctx context.Context, id string) ([]runtime.Record, error) {
	j.replays.Add(1)
	return j.Journal.Replay(ctx, id)
}

func TestLoadReadsOnlyWhatViewNeeds(t *testing.T) {
	t.Parallel()
	base := runtime.NewMemoryJournal()
	defer func() { _ = base.Close() }()
	if err := base.Checkpoint(t.Context(), "selected", runtime.RunPending); err != nil {
		t.Fatal(err)
	}
	j := &loadJournal{Journal: base}
	runner := runtime.NewRunner(j, nil)
	s := &server{journal: j, runner: runner, api: httpchannel.New(runner).Handler()}
	for _, tc := range []struct {
		view                             string
		wantRuns, wantStates, wantReplay int64
	}{
		{view: "runs", wantRuns: 1, wantStates: 2, wantReplay: 1},
		{view: "chat", wantStates: 1, wantReplay: 1},
		{view: "trace", wantReplay: 1},
	} {
		j.runs.Store(0)
		j.states.Store(0)
		j.replays.Store(0)
		r := httptest.NewRequest("GET", "/web/?view="+url.QueryEscape(tc.view)+"&run=selected", nil)
		_, err := s.load(r)
		if err != nil {
			t.Fatalf("load %s: %v", tc.view, err)
		}
		if got := j.runs.Load(); got != tc.wantRuns {
			t.Errorf("%s Runs calls = %d, want %d", tc.view, got, tc.wantRuns)
		}
		if got := j.states.Load(); got != tc.wantStates {
			t.Errorf("%s State calls = %d, want %d", tc.view, got, tc.wantStates)
		}
		if got := j.replays.Load(); got != tc.wantReplay {
			t.Errorf("%s Replay calls = %d, want %d", tc.view, got, tc.wantReplay)
		}
	}
}
