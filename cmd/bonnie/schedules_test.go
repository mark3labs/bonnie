package main

import (
	"encoding/json"
	"github.com/spf13/cobra"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Cobra reads os.Stdout even with an explicit writer. Other CLI tests
// replace that global, so command execution cannot run in parallel here.
func TestSchedulesCLI(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Method
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"history":[{"id":"x"}],"other":true}`))
	}))
	defer srv.Close()

	t.Run("manual default and auth", func(t *testing.T) {
		cmd := quietSchedulesCmd()
		cmd.SetArgs([]string{"trigger", "a/b", "--url", srv.URL, "--token", "secret"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if gotPath != "/bonnie/v1/schedules/a%2Fb/trigger" {
			t.Fatalf("path: %s", gotPath)
		}
		if gotAuth != "Bearer secret" {
			t.Fatalf("auth: %s", gotAuth)
		}
		if gotMethod != http.MethodPost || payload["kind"] != "manual" || payload["scheduled_at"] == nil {
			t.Fatalf("request: %s %#v", gotMethod, payload)
		}
		parsed, err := time.Parse(time.RFC3339Nano, payload["scheduled_at"].(string))
		if err != nil || time.Since(parsed) > time.Minute {
			t.Fatalf("scheduled_at: %#v %v", payload["scheduled_at"], err)
		}
	})
	t.Run("read arguments and history output", func(t *testing.T) {
		cmd := quietSchedulesCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"history", "a/b", "--url", srv.URL})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if gotPath != "/bonnie/v1/schedules/a%2Fb" {
			t.Fatalf("path: %s", gotPath)
		}
		if out.String() != "[{\"id\":\"x\"}]\n" {
			t.Fatalf("history output: %q", out.String())
		}
		cmd = quietSchedulesCmd()
		cmd.SetArgs([]string{"list", "unexpected"})
		if err := cmd.Execute(); err == nil {
			t.Fatal("list accepted arguments")
		}
		cmd = quietSchedulesCmd()
		cmd.SetArgs([]string{"show"})
		if err := cmd.Execute(); err == nil {
			t.Fatal("show accepted no name")
		}
	})
	t.Run("trigger validation", func(t *testing.T) {
		for _, args := range [][]string{{"trigger", "x", "--kind", "external"}, {"trigger", "x", "--kind", "external", "--scheduled-at", "bad", "--id", "id"}, {"trigger", "x", "--kind", "external", "--scheduled-at", "2026-10-07T00:00:00Z"}} {
			cmd := quietSchedulesCmd()
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil {
				t.Fatalf("accepted %v", args)
			}
		}
	})
}

func quietSchedulesCmd() *cobra.Command {
	cmd := newSchedulesCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd
}
