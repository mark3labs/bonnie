package bonnie

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/schedule"
)

// A saved host callback result must stay completed across an HTTP server restart,
// with no agent work and no repeated host effect on duplicate manual triggers.
func TestScheduleRunHTTPDurability(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	journal := filepath.Join(dir, "journal")
	effect := filepath.Join(dir, "host-effect")
	agent := channeltest.NewScriptAgent()
	var calls atomic.Int32
	def := schedule.Definition{
		Name: "host", Cron: "0 9 * * *", TimeZone: "UTC",
		Run: func(ctx context.Context, fire schedule.Fire) error {
			calls.Add(1)
			if err := ctx.Err(); err != nil {
				return err
			}
			return os.WriteFile(effect, []byte(fire.ID), 0o600)
		},
	}
	client := &http.Client{Timeout: 2 * time.Second}
	start := func() (string, func()) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		a := New(WithAgentFactory(agent.Factory()), WithJournal(journal), WithContextFiles(""),
			WithListener(ln), WithSchedule(def), WithScheduleClock(false),
			WithScheduleTriggerAuthorizer(func(r *http.Request) error {
				if r.Header.Get("Authorization") != "Bearer secret" {
					return fmt.Errorf("unauthorized")
				}
				return nil
			}), Quiet())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		stopped := false
		stop := func() {
			t.Helper()
			if stopped {
				return
			}
			stopped = true
			// Close idle connections so they do not delay HTTP shutdown.
			client.CloseIdleConnections()
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("server shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("server did not stop")
			}
		}
		t.Cleanup(stop)
		base := "http://" + ln.Addr().String()
		waitScheduleHTTP(t, client, base)
		return base, stop
	}
	request := func(base, method, path, body string, authorized bool, status int, result any) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if authorized {
			req.Header.Set("Authorization", "Bearer secret")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Errorf("close HTTP response: %v", err)
			}
		}()
		if resp.StatusCode != status {
			t.Fatalf("%s %s status = %d, want %d", method, path, resp.StatusCode, status)
		}
		if result != nil {
			if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
				t.Fatal(err)
			}
		}
	}
	const path = "/bonnie/v1/schedules/host"
	const body = `{"id":"once","scheduled_at":"2026-10-07T09:00:00Z"}`
	var saved schedule.Occurrence
	for phase := range 2 {
		base, stop := start()
		if phase == 0 {
			request(base, http.MethodPost, path+"/trigger", body, false, http.StatusUnauthorized, nil)
			request(base, http.MethodPost, path+"/trigger", body, true, http.StatusOK, nil)
		}
		history := func() schedule.Occurrence {
			t.Helper()
			var v struct {
				History []schedule.Occurrence `json:"history"`
			}
			request(base, http.MethodGet, path, "", false, http.StatusOK, &v)
			if len(v.History) != 1 {
				t.Fatalf("history = %+v, want one occurrence", v.History)
			}
			return v.History[0]
		}
		if phase == 0 {
			waitForSchedule(t, func() bool { return history().State == schedule.Completed })
			saved = history()
		}
		assertCompleted := func() {
			t.Helper()
			o := history()
			if o.Fire != saved.Fire || o.Fire.Kind != "manual" || o.State != schedule.Completed || !o.Prepared || len(o.Work) != 0 || o.Error != "" {
				t.Fatalf("history occurrence = %+v, want saved completed callback with no work", o)
			}
		}
		assertCompleted()
		request(base, http.MethodPost, path+"/trigger", body, true, http.StatusOK, nil)
		assertCompleted()
		stop()
		if calls.Load() != 1 || agent.Calls() != 0 {
			t.Fatalf("callback calls = %d, agent calls = %d, want 1 and 0", calls.Load(), agent.Calls())
		}
		data, err := os.ReadFile(effect)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != saved.Fire.ID {
			t.Fatalf("host effect = %q, want %q", data, saved.Fire.ID)
		}
	}
}
