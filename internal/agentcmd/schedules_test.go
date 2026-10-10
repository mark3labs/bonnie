package agentcmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Reject incomplete external identities before contacting the server. Valid
// external triggers keep their explicit identity and scheduled time.
func TestScheduleTriggerIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		flags []string
		valid bool
	}{
		{name: "missing time", flags: []string{"--kind", "external", "--id", "fire"}},
		{name: "missing identity", flags: []string{"--kind", "external", "--scheduled-at", "2026-10-07T00:00:00Z"}},
		{name: "blank identity", flags: []string{"--kind", "external", "--id", " ", "--scheduled-at", "2026-10-07T00:00:00Z"}},
		{name: "external", flags: []string{"--kind", "external", "--id", "fire", "--scheduled-at", "2026-10-07T00:00:00Z"}, valid: true},
		{name: "manual", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls int
			var payload map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			cmd := NewSchedulesCommand(server.URL)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"trigger", "test"}, tc.flags...))
			err := cmd.Execute()
			if tc.valid {
				if err != nil || calls != 1 {
					t.Fatalf("error = %v, calls = %d", err, calls)
				}
				if tc.name == "external" && (payload["id"] != "fire" || payload["scheduled_at"] != "2026-10-07T00:00:00Z") {
					t.Fatalf("payload = %#v", payload)
				}
			} else if err == nil || !strings.Contains(err.Error(), "require --scheduled-at and --id") || calls != 0 {
				t.Fatalf("error = %v, calls = %d", err, calls)
			}
		})
	}
}
