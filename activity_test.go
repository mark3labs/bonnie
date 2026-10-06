package bonnie

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/log"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestActivityLoggerJSON(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := log.NewWithOptions(&output, log.Options{ReportTimestamp: false, ReportCaller: false})
	logger.SetFormatter(log.JSONFormatter)
	logger.SetLevel(log.DebugLevel)
	activity := NewActivityLogger(logger)

	cases := []struct {
		name  string
		event runtime.Event
		level string
		check func(map[string]any) bool
	}{
		{
			name: "tool call",
			event: runtime.Event{RunID: "run-1", Seq: 7, Type: string(kit.EventToolCall),
				Data: json.RawMessage(`{"ToolName":"lookup","ToolCallID":"call-2"}`)},
			level: "info",
			check: func(entry map[string]any) bool {
				return entry["run_id"] == "run-1" && entry["seq"] == float64(7) &&
					entry["tool"] == "lookup" && entry["tool_call_id"] == "call-2"
			},
		},
		{
			name:  "response",
			event: runtime.Event{RunID: "run-1", Seq: 8, Type: runtime.EventResponse, Text: "hello"},
			level: "info",
			check: func(entry map[string]any) bool { return entry["text"] == "hello" },
		},
		{
			name:  "failure",
			event: runtime.Event{RunID: "run-1", Seq: 9, Type: runtime.EventState, State: runtime.RunFailed},
			level: "error",
			check: func(entry map[string]any) bool { return entry["state"] == string(runtime.RunFailed) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output.Reset()
			activity.LogActivity(tc.event)
			var entry map[string]any
			if err := json.Unmarshal(bytes.SplitN(bytes.TrimSpace(output.Bytes()), []byte("\n"), 2)[0], &entry); err != nil {
				t.Fatalf("invalid JSON log %q: %v", output.String(), err)
			}
			if entry["level"] != tc.level || !tc.check(entry) {
				t.Fatalf("entry = %#v, want level %q and expected fields", entry, tc.level)
			}
		})
	}
}

func TestActivityLoggerPayloadLevel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		level     log.Level
		wantDebug bool
	}{
		{name: "info suppresses payload", level: log.InfoLevel},
		{name: "debug includes payload", level: log.DebugLevel, wantDebug: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			logger := log.NewWithOptions(&output, log.Options{ReportTimestamp: false})
			logger.SetFormatter(log.JSONFormatter)
			logger.SetLevel(tc.level)
			NewActivityLogger(logger).LogActivity(runtime.Event{
				RunID: "run-1", Type: string(kit.EventToolCall),
				Data: json.RawMessage(`{"ToolName":"lookup","Arguments":{"token":"secret"}}`),
			})
			got := output.String()
			if strings.Contains(got, `"event":"`) != tc.wantDebug || strings.Contains(got, "secret") != tc.wantDebug {
				t.Fatalf("payload visibility at %s = %q", tc.level, got)
			}
		})
	}
}

func TestActivityLoggerNilAndZero(t *testing.T) {
	t.Parallel()
	var nilLogger *ActivityLogger
	var zero ActivityLogger
	event := runtime.Event{Type: runtime.EventResponse, Text: "ignored"}
	nilLogger.LogActivity(event)
	zero.LogActivity(event)
	if got := NewActivityLogger(nil); got == nil || got.logger == nil {
		t.Fatal("NewActivityLogger(nil) returned an unusable logger")
	}
}

func TestWithActivityLogger(t *testing.T) {
	t.Parallel()
	want := &ActivityLogger{}
	got := resolve(WithActivityLogger(want))
	if got.activityLogger != want {
		t.Fatal("WithActivityLogger did not store the configured logger")
	}
	if got := resolve(WithActivityLogger(nil)); got.activityLogger != nil {
		t.Fatal("WithActivityLogger(nil) did not disable activity logging")
	}
}
