package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/mark3labs/bonnie/channel/chat"
)

// ActivityMode selects how a channel shows that the agent is working.
type ActivityMode string

// The activity modes, from "say nothing" to Slack's own assistant status.
const (
	// ActivityDefault is the empty value: [ActivityMessage].
	ActivityDefault ActivityMode = ""

	// ActivityOff shows nothing until the reply. A thread then holds only
	// the conversation, which is what an audited channel may want.
	ActivityOff ActivityMode = "off"

	// ActivityMessage posts one placeholder message into the thread and
	// edits it in place — `chat.postMessage`, then `chat.update` per
	// status, then `chat.delete` when the reply is ready. It needs the
	// `chat:write` scope the channel already needs to answer at all, and
	// it works in every conversation: a channel, a thread, a DM.
	ActivityMessage ActivityMode = "message"

	// ActivityStatus drives Slack's own assistant typing indicator with
	// `assistant.threads.setStatus`. It is the tidiest surface — no
	// message is posted, so nothing can be left behind — but Slack accepts
	// it only for an app with the AI assistant surface enabled, in an
	// assistant thread, and it needs the `assistant:write` scope. A Slack
	// app without that surface answers `invalid_thread` and the indicator
	// is silently absent; use [ActivityMessage] there.
	ActivityStatus ActivityMode = "status"
)

// activityPrefix marks the placeholder message as machinery rather than
// speech. A person scanning a busy thread must be able to tell the agent's
// answer from its progress at a glance.
const activityPrefix = "_… "

const activitySuffix = "_"

// activity is the per-address state of the placeholder message: the
// timestamp to edit, and the text already on it.
type activity struct {
	ts   string
	text string
}

// renderActivity is the [chat.ActivityFunc] the core calls while a turn
// runs. The empty status ends the turn: the placeholder is deleted so the
// reply, which posts next, is the last word in the thread.
//
// Every call is best-effort and logged, never retried: an indicator that
// fails must not fail the turn it describes. A delete that Slack refuses
// therefore leaves the last status standing in the thread, which is the
// cost of never letting a status line break a run. Slack reports an
// application failure as HTTP 200 with `"ok": false`, so [Channel.call]
// reads the body rather than the status line.
func (c *Channel) renderActivity(address, status string) {
	if c.cfg.BotToken == "" {
		return
	}
	channelID, thread, _ := strings.Cut(address, "/")
	if thread == "dm" {
		thread = ""
	}
	if c.cfg.Activity == ActivityStatus {
		c.setAssistantStatus(channelID, thread, status)
		return
	}

	c.activeMu.Lock()
	current := c.active[address]
	switch {
	case status == "":
		delete(c.active, address)
	case current == nil:
		// Posting is a round trip, and the timestamp it answers with is
		// what the next edit needs. The entry is written below.
	default:
		if current.text == status {
			c.activeMu.Unlock()
			return // already on screen; an edit would buy nothing
		}
		current.text = status
	}
	c.activeMu.Unlock()

	switch {
	case status == "":
		if current == nil {
			return
		}
		c.call(context.Background(), "chat.delete", map[string]any{
			"channel": channelID,
			"ts":      current.ts,
		})

	case current == nil:
		ts, ok := c.postMessageTS(context.Background(), channelID, thread, activityText(status))
		if !ok || ts == "" {
			return
		}
		c.activeMu.Lock()
		c.active[address] = &activity{ts: ts, text: status}
		c.activeMu.Unlock()

	default:
		c.call(context.Background(), "chat.update", map[string]any{
			"channel": channelID,
			"ts":      current.ts,
			"text":    activityText(status),
		})
	}
}

// activityText renders a status as the placeholder's body: italic, and
// prefixed, so it reads as a note about the work rather than as an answer.
func activityText(status string) string {
	return activityPrefix + status + activitySuffix
}

// setAssistantStatus drives Slack's assistant typing indicator. The empty
// status clears it, which is exactly what the end of a turn means.
func (c *Channel) setAssistantStatus(channelID, thread, status string) {
	if thread == "" {
		return // the assistant status is a property of a thread
	}
	body := map[string]any{
		"channel_id": channelID,
		"thread_ts":  thread,
		"status":     status,
	}
	if status != "" {
		body["loading_messages"] = []string{status}
	}
	c.call(context.Background(), "assistant.threads.setStatus", body)
}

// call posts one JSON request to a Slack API method and reports the
// outcome to stderr. It is the fire-and-log path every activity write
// takes: the journal holds the run, and a lost indicator costs a status
// line.
func (c *Channel) call(ctx context.Context, method string, payload map[string]any) {
	if c.cfg.BotToken == "" {
		return
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/"+method, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.BotToken)
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bonnie: slack: %s: %v\n", method, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.OK {
		reason := out.Error
		if reason == "" {
			reason = resp.Status
		}
		fmt.Fprintf(os.Stderr, "bonnie: slack: %s: %s\n", method, reason)
	}
}

// activityOption returns the core option that wires the indicator, or nil
// when the channel shows none.
//
// An unknown mode shows nothing and says so. A misspelling must not mean
// "the default": a host that asked for a surface and got another one would
// have no way to tell, and the constructor has no error to return.
func (c *Channel) activityOption() chat.CoreOption {
	switch c.cfg.Activity {
	case ActivityOff:
		return nil
	case ActivityDefault, ActivityMessage, ActivityStatus:
	default:
		fmt.Fprintf(os.Stderr,
			"bonnie: slack: unknown activity mode %q, so no indicator is shown\n", c.cfg.Activity)
		return nil
	}
	if c.cfg.BotToken == "" {
		return nil // nothing to write with
	}
	return chat.WithActivity(c.renderActivity)
}
