package bonnie

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/internal/fakemodel"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	"github.com/mark3labs/bonnie/schedule"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// TestScheduleKitApprovalSurvivesRestart checks a real Kit approval suspension
// through schedule dispatch, HTTP resume, and a second process opening SQLite.
func TestScheduleKitApprovalSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	journal := t.TempDir()
	model := fakemodel.New(
		fakemodel.Call("request_approval", `{"action":"publish report"}`),
		fakemodel.Say("Approved. Report published."),
	)
	definition := schedule.Definition{Name: "report", Cron: "0 9 * * *", TimeZone: "UTC", Prompt: "Ask for approval before publishing the report."}
	start := func() (string, <-chan error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		agent := New(
			WithJournal(journal), WithWorkspace(""), WithListener(listener), WithSchedule(definition), WithScheduleClock(false),
			WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))), WithSystemPrompt("test system prompt"),
			WithKit(model.Option(), func(o *kit.Options) {
				o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.Quiet = true, true, true, true, true, true
			}),
			WithScheduleTriggerAuthorizer(func(r *http.Request) error {
				if r.Header.Get("Authorization") != "Bearer secret" {
					return fmt.Errorf("unauthorized")
				}
				return nil
			}), Quiet(),
		)
		done := make(chan error, 1)
		go func() { done <- agent.Run(ctx) }()
		t.Cleanup(func() {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("agent run: %v", err)
				}
			default:
			}
		})
		return "http://" + listener.Addr().String(), done
	}
	client := &http.Client{Timeout: 3 * time.Second}
	base, done := start()
	waitScheduleHTTP(t, client, base)

	body := `{"id":"approval-run","scheduled_at":"2026-10-07T09:00:00Z"}`
	request, err := http.NewRequest(http.MethodPost, base+"/bonnie/v1/schedules/report/trigger", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("trigger status = %d, want 200", response.StatusCode)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	var runID string
	waitForSchedule(t, func() bool {
		response, err := client.Get(base + "/bonnie/v1/schedules/report")
		if err != nil {
			return false
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Errorf("close schedule response: %v", err)
			}
		}()
		var result struct {
			History []schedule.Occurrence `json:"history"`
		}
		if json.NewDecoder(response.Body).Decode(&result) != nil || len(result.History) == 0 {
			return false
		}
		occurrence := result.History[0]
		if occurrence.State != schedule.Waiting || len(occurrence.Work) == 0 || occurrence.Work[0].Result == nil {
			return false
		}
		runID = occurrence.Work[0].Receipt.RunID
		return runID != ""
	})
	if calls := len(model.Requests()); calls != 1 {
		t.Fatalf("initial model calls = %d, want exactly 1", calls)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first server did not stop")
	}
	ctx, cancel = context.WithCancel(context.Background())
	base, done = start()
	waitScheduleHTTP(t, client, base)

	responseToApproval, err := http.NewRequest(http.MethodPost, base+"/bonnie/v1/runs/"+runID+"/respond", strings.NewReader(`{"responses":[{"approved":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	responseToApproval.Header.Set("Content-Type", "application/json")
	response, err = client.Do(responseToApproval)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		data := make([]byte, 4096)
		n, _ := response.Body.Read(data)
		_ = response.Body.Close()
		t.Fatalf("approval response status = %d: %s", response.StatusCode, data[:n])
	}
	_ = response.Body.Close()
	waitForSchedule(t, func() bool {
		response, err := client.Get(base + "/bonnie/v1/runs/" + runID)
		if err != nil {
			return false
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Errorf("close run response: %v", err)
			}
		}()
		var run runtime.Run
		return json.NewDecoder(response.Body).Decode(&run) == nil && run.State == runtime.RunCompleted && strings.Contains(run.Response, "Approved. Report published.")
	})
	if calls := len(model.Requests()); calls != 2 {
		t.Fatalf("total model calls = %d, want 2 (one after approval)", calls)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("restarted server did not stop")
	}
}
