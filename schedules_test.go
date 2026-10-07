package bonnie

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/channel/slack"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/schedule"
)

func TestScheduleHTTPDurabilityAndAuthorization(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	journal := filepath.Join(t.TempDir(), "journal")
	agent := channeltest.NewScriptAgent()
	def := schedule.Definition{Name: "daily", Cron: "0 9 * * *", TimeZone: "UTC", Prompt: "scheduled prompt"}
	start := func() (string, <-chan error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().String()
		a := New(WithAgentFactory(agent.Factory()), WithJournal(journal), WithContextFiles(""), WithListener(ln), WithSchedule(def), WithScheduleClock(false),
			WithScheduleTriggerAuthorizer(func(r *http.Request) error {
				if r.Header.Get("Authorization") != "Bearer secret" {
					return fmt.Errorf("unauthorized")
				}
				return nil
			}), Quiet())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		return "http://" + port, done
	}
	base, done := start()
	client := &http.Client{Timeout: 2 * time.Second}
	waitScheduleHTTP(t, client, base)
	get := func(path string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get("/bonnie/v1/schedules")
	if resp.StatusCode != 200 {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp = get("/bonnie/v1/schedules/daily")
	if resp.StatusCode != 200 {
		t.Fatalf("history status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	post := func(id string, auth bool) *http.Response {
		body := fmt.Sprintf(`{"id":%q,"scheduled_at":"2026-10-07T09:00:00Z"}`, id)
		req, _ := http.NewRequest(http.MethodPost, base+"/bonnie/v1/schedules/daily/trigger", strings.NewReader(body))
		if auth {
			req.Header.Set("Authorization", "Bearer secret")
		}
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	resp = post("denied", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized trigger = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	resp = post("once", true)
	if resp.StatusCode != 200 {
		b := make([]byte, 4096)
		n, _ := resp.Body.Read(b)
		t.Fatalf("trigger status %d: %s", resp.StatusCode, b[:n])
	}
	_ = resp.Body.Close()
	waitForSchedule(t, func() bool {
		r := get("/bonnie/v1/schedules/daily")
		defer func() { _ = r.Body.Close() }()
		var v struct {
			History []schedule.Occurrence `json:"history"`
		}
		return json.NewDecoder(r.Body).Decode(&v) == nil && len(v.History) > 0 && v.History[0].State == schedule.Completed
	})
	if agent.Calls() != 1 {
		t.Fatalf("calls = %d, want 1", agent.Calls())
	}
	r, err := runtime.OpenSQLiteJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := r.Runs(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var runID string
	for _, id := range ids {
		if strings.HasPrefix(id, "schedule-") {
			runID = id
		}
	}
	if runID == "" {
		t.Fatalf("scheduled run missing from %v", ids)
	}
	recs, err := r.Replay(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var userText bool
	for _, rec := range recs {
		if rec.Kind == runtime.RecordMessage && strings.Contains(rec.Text, "scheduled prompt") {
			userText = true
		}
	}
	if !userText {
		t.Fatal("scheduled prompt missing from runtime transcript")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// A newly idle keep-alive connection can delay net/http shutdown for
	// five seconds. Close the test client's connections before cancellation.
	client.CloseIdleConnections()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		client.CloseIdleConnections()
		t.Fatal("server did not stop")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	base, done = start()
	waitScheduleHTTP(t, client, base)
	// Retry the same external occurrence after restart; it must not run the model again.
	body := fmt.Sprintf(`{"id":"once","scheduled_at":%q}`, at.Format(time.RFC3339))
	req, _ := http.NewRequest(http.MethodPost, base+"/bonnie/v1/schedules/daily/trigger", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("duplicate trigger status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if agent.Calls() != 1 {
		t.Fatalf("duplicate trigger called model: %d", agent.Calls())
	}
	client.CloseIdleConnections()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		client.CloseIdleConnections()
		t.Fatal("restarted server did not stop")
	}
}

func TestScheduleStartupLock(t *testing.T) {
	t.Parallel()
	journal := filepath.Join(t.TempDir(), "journal")
	def := schedule.Definition{Name: "job", Cron: "0 9 * * *", TimeZone: "UTC", Prompt: "x"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := func() <-chan error {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		a := New(WithAgentFactory(channeltest.NewScriptAgent().Factory()), WithJournal(journal), WithContextFiles(""), WithListener(ln), WithSchedule(def), WithScheduleClock(false), Quiet())
		ch := make(chan error, 1)
		go func() { ch <- a.Run(ctx) }()
		return ch
	}
	first := start()
	waitForSchedule(t, func() bool { _, err := os.Stat(filepath.Join(journal, ".schedule.lock")); return err == nil })
	second := start()
	select {
	case err := <-second:
		if err == nil || !strings.Contains(err.Error(), "already owned") {
			t.Fatalf("competing process error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("competing process did not fail")
	}
	cancel()
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first process did not stop")
	}
}

func TestScheduleSlackDeliveryUsesThread(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var posts []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Channel string `json:"channel"`
			Text    string `json:"text"`
			Thread  string `json:"thread_ts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		posts = append(posts, b.Channel+"|"+b.Thread+"|"+b.Text)
		n := len(posts)
		mu.Unlock()
		ts := "1700000000.000900"
		if n > 1 {
			ts = "1700000000.001000"
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":%q}`, ts)
	}))
	t.Cleanup(func() { api.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	def := schedule.Definition{Name: "slack-job", Cron: "0 9 * * *", TimeZone: "UTC", Prompt: "scheduled prompt", Destination: schedule.Destination{Channel: "slack", Target: "C123"}}
	a := New(WithAgentFactory(channeltest.NewScriptAgent().Factory()), WithJournal(filepath.Join(t.TempDir(), "journal")), WithContextFiles(""), WithListener(ln), WithSchedule(def), WithScheduleClock(false), WithScheduleTriggerAuthorizer(func(*http.Request) error { return nil }), WithSlack(slack.Config{BotToken: "xoxb-test", SigningSecret: "secret", APIURL: api.URL}), Quiet())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}
	waitScheduleHTTP(t, client, base)
	body := `{"id":"slack-once","scheduled_at":"2026-10-07T09:00:00Z"}`
	req, _ := http.NewRequest(http.MethodPost, base+"/bonnie/v1/schedules/slack-job/trigger", strings.NewReader(body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("Slack trigger status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	waitForSchedule(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(posts) >= 2 })
	mu.Lock()
	got := append([]string(nil), posts...)
	mu.Unlock()
	if len(got) < 2 || got[0] != "C123||Scheduled task" || !strings.HasPrefix(got[1], "C123|1700000000.000900|") {
		t.Fatalf("Slack scheduled posts = %v, want neutral root and reply in its thread", got)
	}
	cancel()
	<-done
}

func waitScheduleHTTP(t *testing.T, c *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := c.Get(base + "/bonnie/v1/schedules")
		if err == nil {
			_ = r.Body.Close()
			if r.StatusCode == 200 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("schedule HTTP API did not become ready")
}
func waitForSchedule(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("schedule condition did not become true")
}
