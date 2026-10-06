package bonnie

import (
	"encoding/json"
	"os"

	"github.com/charmbracelet/log"
	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie/runtime"
)

// ActivityLogger writes live run activity with charmbracelet/log. Use
// NewActivityLogger to build one and WithActivityLogger to enable it.
// It is safe for concurrent use when its underlying logger is not changed.
type ActivityLogger struct {
	logger *log.Logger
}

var _ runtime.ActivityLogger = (*ActivityLogger)(nil)

// NewActivityLogger uses logger to write activity. Nil selects a new text
// logger on stdout at Info level. Run states, tool calls, tool results, and
// final responses use Info level; failures use Error level. Warnings and
// retries use Warn level. Other lifecycle events and raw payloads use Debug level.
//
// Logs can contain sensitive data. Final response text is included at Info
// level. Debug output also includes prompts, tool arguments, and results.
func NewActivityLogger(logger *log.Logger) *ActivityLogger {
	if logger == nil {
		logger = log.NewWithOptions(os.Stdout, log.Options{ReportTimestamp: true})
	}
	return &ActivityLogger{logger: logger}
}

// LogActivity writes one live event. It does not change the event. A nil
// receiver or a zero ActivityLogger writes nothing.
func (l *ActivityLogger) LogActivity(ev runtime.Event) {
	if l == nil || l.logger == nil {
		return
	}
	fields := []any{"run_id", ev.RunID, "seq", ev.Seq}
	level := log.DebugLevel
	switch ev.Type {
	case runtime.EventState:
		level = log.InfoLevel
		fields = append(fields, "state", ev.State)
		if ev.State == runtime.RunFailed {
			level = log.ErrorLevel
		}
	case runtime.EventSuspend, runtime.EventResume:
		level = log.InfoLevel
	case runtime.EventResponse:
		level = log.InfoLevel
		fields = append(fields, "text", ev.Text)
	case string(kit.EventToolCall), string(kit.EventToolResult):
		level = log.InfoLevel
		var tool struct {
			ToolCallID string
			ToolName   string
			IsError    bool
		}
		if err := json.Unmarshal(ev.Data, &tool); err == nil {
			fields = append(fields, "tool", tool.ToolName, "tool_call_id", tool.ToolCallID)
			if tool.IsError {
				level = log.ErrorLevel
			}
		}
	case string(kit.EventError):
		level = log.ErrorLevel
	case string(kit.EventWarnings), string(kit.EventRetry):
		level = log.WarnLevel
	}
	l.logger.Log(level, ev.Type, fields...)
	if len(ev.Data) != 0 {
		l.logger.Debug("activity_payload", "run_id", ev.RunID, "event", ev.Type, "data", string(ev.Data))
	}
}

// WithActivityLogger enables live activity logging for all channels, including
// NATS. Nil disables logging. Any runtime.ActivityLogger can be used, including
// one built with NewActivityLogger. Replayed events are not logged again.
func WithActivityLogger(logger runtime.ActivityLogger) Option {
	return func(c *config) { c.activityLogger = logger }
}
