package slack

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

func (c *Channel) cancelPath(path string) string {
	if c.cfg.CancelPath != "" {
		return c.cfg.CancelPath
	}
	return path + "/cancel"
}

// handleCancelCommand verifies the signed slash-command body. A public channel
// needs an explicit thread target: guessing the latest thread is not safe.
func (c *Channel) handleCancelCommand(w http.ResponseWriter, r *http.Request, _ channel.Inbound, _ channel.Outbound) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !c.verify(r.Header.Get("X-Slack-Signature"), r.Header.Get("X-Slack-Request-Timestamp"), body) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	channelID := form.Get("channel_id")
	target := strings.TrimSpace(form.Get("text"))
	if channelID == "" || form.Get("user_id") == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if target == "" {
		if !strings.HasPrefix(channelID, "D") {
			http.Error(w, "Supply the conversation thread timestamp.", http.StatusBadRequest)
			return
		}
		target = "dm"
	} else {
		parts := strings.Split(target, ".")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Trim(target, "0123456789.") != "" {
			http.Error(w, "Invalid thread timestamp.", http.StatusBadRequest)
			return
		}
	}
	// Duplicate Slack deliveries cannot stop a later turn.
	key := form.Get("trigger_id")
	if key == "" {
		http.Error(w, "missing trigger", http.StatusBadRequest)
		return
	}
	if !c.claim("cancel:" + key) {
		_, _ = w.Write([]byte("Cancellation already requested."))
		return
	}
	result, err := c.core.From(channelID+"/"+target).RequestCancel(r.Context(), "")
	if err != nil {
		http.Error(w, "Cancellation could not be recorded.", http.StatusInternalServerError)
		return
	}
	text := "No active turn."
	if result.Status == runtime.CancelRequested {
		text = "Cancellation requested."
	}
	_, _ = w.Write([]byte(text))
}
