// Package slack is BONNIE's Slack inbound transport (L3).
//
// # The shape
//
// Slack POSTs one JSON event per message to your webhook (the Events API).
// This adapter mounts one route (default `POST /slack/events`), verifies
// Slack's v0 signature over the raw body, answers the `url_verification`
// handshake, and acknowledges everything else with `200` at once — the turn
// runs in a goroutine the handler does not outlive, and the reply goes back
// through `chat.postMessage` with the bot token.
//
// Setup on the Slack side: create an app, enable Event Subscriptions with
// this route as the Request URL, subscribe to `app_mention` and — for
// direct messages — `message.im`, install the app to the workspace, and set
// the bot token and signing secret.
//
// # Which messages are for the bot
//
//   - An `app_mention`: always — the mention is the invocation, and the
//     mention's thread (or the message itself, when it starts one) is the
//     conversation.
//   - A direct message: always. One conversation per DM channel.
//   - A threaded reply: only when this channel bound that thread — a busy
//     channel's other threads are not the agent's business.
//
// Slack redelivers on timeout, so events are deduplicated by `event_id`
// before they reach the runner: a retried delivery must not steer or queue
// a second copy of the same message into the turn.
//
// # Human-in-the-loop
//
// When a run parks on a question, the prompt is delivered into the thread
// with a note to reply. The next text on the same thread is the answer: the
// dispatch rule in `channel/chat` routes it to the run's resume, not to a
// new turn.
//
// # Limits, stated plainly
//
//   - Webhook only — Socket Mode (no public URL) is not implemented.
//   - Replies are plain text: no Block Kit, no mrkdwn parsing, so the
//     model's markdown shows as written.
//   - A reply longer than Slack's per-message limit is split, with a cap
//     of five parts and a truncation notice on the last.
//   - Button actions (Block Kit interactivity) are not implemented; a run
//     parked on a structured approval is answered in text.
//   - Attachments and files are ignored.
package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

// DefaultPath is the webhook path the route mounts at.
const DefaultPath = "/slack/events"

// messageLimit is Slack's per-message character limit; maxParts caps a
// single reply's flood.
const (
	messageLimit = 39000
	maxParts     = 5
)

// replayWindow is how far a request's timestamp may drift before it is
// stale. Slack signs the timestamp into the signature; a window rejects
// captures that are replayed later.
const replayWindow = 5 * time.Minute

// Config configures the Slack channel.
type Config struct {
	// BotToken is the OAuth bot token (starts `xoxb-`). Delivery needs it;
	// receiving works without it, which is what the conformance suite uses.
	BotToken string

	// SigningSecret verifies that Slack sent this request. Set it: without
	// verification, anyone who can reach the webhook drives the agent.
	SigningSecret string

	// APIURL overrides the Slack API base URL. Tests point it at a fake;
	// leave it empty in production.
	APIURL string

	// Path overrides the webhook route. The default is [DefaultPath].
	Path string
}

// Channel is the Slack transport. It implements [channel.Channel] and
// [channel.Inbound].
type Channel struct {
	core   *chat.Core
	cfg    Config
	api    string
	http   *http.Client
	seenMu sync.Mutex
	seen   map[string]bool
}

var (
	_ channel.Channel = (*Channel)(nil)
	_ channel.Inbound = (*Channel)(nil)
)

// New returns a Slack channel over a runner.
func New(r *runtime.Runner, cfg Config, opts ...chat.CoreOption) *Channel {
	api := cfg.APIURL
	if api == "" {
		api = "https://slack.com/api"
	}
	return &Channel{
		core: chat.NewCore(r, "slack", channel.PolicySteer, opts...),
		cfg:  cfg,
		api:  api,
		http: &http.Client{Timeout: 15 * time.Second},
		seen: make(map[string]bool),
	}
}

// Name implements [channel.Channel].
func (c *Channel) Name() string { return "slack" }

// Routes implements [channel.Channel].
func (c *Channel) Routes() []channel.Route {
	path := c.cfg.Path
	if path == "" {
		path = DefaultPath
	}
	return []channel.Route{
		{Method: http.MethodPost, Path: path, Handler: c.handleEvent},
	}
}

// From implements [channel.Inbound].
func (c *Channel) From(address string) channel.SessionRef { return c.core.From(address) }

// Attach implements [channel.Inbound].
func (c *Channel) Attach(runID string) channel.SessionRef { return c.core.Attach(runID) }

// The wire types, the parts of a Slack event this adapter reads.
type slackEvent struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	EventID   string `json:"event_id"`
	Event     *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		TS          string `json:"ts"`
		ThreadTS    string `json:"thread_ts"`
		Channel     string `json:"channel"`
		ChannelType string `json:"channel_type"` // "im" for a direct message
		BotID       string `json:"bot_id"`
		User        string `json:"user"`
		Subtype     string `json:"subtype"`
	} `json:"event"`
}

// handleEvent implements the webhook.
func (c *Channel) handleEvent(w http.ResponseWriter, r *http.Request, _ channel.Inbound) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !c.verify(r.Header.Get("X-Slack-Signature"), r.Header.Get("X-Slack-Request-Timestamp"), body) {
		// Not Slack. A probe learns only that the door did not open.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var ev slackEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeOK(w)
		return
	}

	// The URL verification handshake: Slack asks once, when an operator
	// saves the Request URL, and expects the challenge echoed.
	if ev.Type == "url_verification" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": ev.Challenge})
		return
	}
	writeOK(w)

	if ev.Type != "event_callback" || ev.Event == nil {
		return
	}
	// Slack retries when an endpoint is slow to acknowledge, so the same
	// event can arrive twice. A retried delivery must not send the same
	// message into the turn again.
	if !c.claim(ev.EventID) {
		return
	}

	turn, ok := c.forUs(ev.Event)
	if !ok {
		return
	}
	turn.Auth = &channel.Principal{
		Authenticator: "slack",
		Kind:          "user",
		ID:            ev.Event.User,
		Attributes: map[string]any{
			"channel": ev.Event.Channel,
		},
	}
	turn.Context = []string{"Slack user " + ev.Event.User + " wrote in channel " + ev.Event.Channel + "."}
	chat.Dispatch(r.Context(), c.core, turn, c.deliver)
}

// verify checks Slack's v0 signature: HMAC-SHA256 over "v0:<timestamp>:<body>"
// keyed by the signing secret, hex-encoded, compared in constant time. The
// timestamp inside the signature bounds replays.
func (c *Channel) verify(signature, timestamp string, body []byte) bool {
	if c.cfg.SigningSecret == "" {
		return true // unverified: the host accepts the risk by configuring so
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)) > replayWindow {
		return false
	}
	mac := hmac.New(sha256.New, []byte(c.cfg.SigningSecret))
	_, _ = fmt.Fprintf(mac, "v0:%s:", timestamp)
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}

// claim records an event ID and reports whether this call is the first.
func (c *Channel) claim(eventID string) bool {
	if eventID == "" {
		return true
	}
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	if c.seen[eventID] {
		return false
	}
	// The map is bounded: a restart forgets it (the runner's own
	// bookkeeping makes a duplicate harmless-ish), and a long-running
	// server must not grow it without limit.
	if len(c.seen) >= 4096 {
		c.seen = make(map[string]bool)
	}
	c.seen[eventID] = true
	return true
}

// forUs decides whether an event reaches the agent, and normalises it: the
// address is the mention's thread, the DM channel, or a thread this channel
// bound; the text has the mention stripped; the kind says which.
func (c *Channel) forUs(e *struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	BotID       string `json:"bot_id"`
	User        string `json:"user"`
	Subtype     string `json:"subtype"`
}) (chat.Turn, bool) {
	if e.BotID != "" || e.Subtype != "" {
		return chat.Turn{}, false // bots and edits: never
	}
	switch {
	case e.Type == "app_mention":
		// The mention starts (or continues) a conversation in its thread.
		thread := e.ThreadTS
		if thread == "" {
			thread = e.TS
		}
		return chat.Turn{
			Address: fmt.Sprintf("slack/%s/%s", e.Channel, thread),
			Text:    stripMention(e.Text),
			Kind:    chat.KindThread,
		}, true

	case e.Type == "message" && e.ChannelType == "im":
		return chat.Turn{
			Address: fmt.Sprintf("slack/%s/dm", e.Channel),
			Text:    strings.TrimSpace(e.Text),
			Kind:    chat.KindDM,
		}, true

	case e.Type == "message" && e.ThreadTS != "":
		// A reply in a thread: for the agent only when it bound that
		// thread. Everything else in a busy channel is not its business.
		address := fmt.Sprintf("slack/%s/%s", e.Channel, e.ThreadTS)
		if _, bound := c.core.Addresses().Lookup(address); bound {
			return chat.Turn{Address: address, Text: strings.TrimSpace(e.Text), Kind: chat.KindThread}, true
		}
		return chat.Turn{}, false

	default:
		return chat.Turn{}, false
	}
}

// stripMention removes Slack's `<@U…>` mention tokens from the text.
func stripMention(text string) string {
	var b strings.Builder
	for {
		start := strings.Index(text, "<@")
		if start < 0 {
			break
		}
		end := strings.IndexByte(text[start:], '>')
		if end < 0 {
			break
		}
		b.WriteString(text[:start])
		text = text[start+end+1:]
	}
	b.WriteString(text)
	return strings.TrimSpace(b.String())
}

// deliver posts a turn's outcome back to the thread the message came from.
// A failed post is logged, never retried in a loop — the run's result is in
// the journal, and `bonnie runs show` reads it back.
func (c *Channel) deliver(address string, run *runtime.Run, err error) {
	text := chat.DeliveryText(run, err, "(Reply in this thread to answer.)")
	if text == "" {
		return
	}
	parts := strings.SplitN(address, "/", 3)
	var thread string
	if len(parts) > 2 && parts[2] != "dm" {
		thread = parts[2]
	}
	for _, p := range chat.SplitText(text, messageLimit, maxParts) {
		c.postMessage(context.Background(), parts[1], thread, p)
	}
}

// postMessage posts one message. Fire-and-log: a delivery failure must not
// take the process down, and the journal keeps the truth.
func (c *Channel) postMessage(ctx context.Context, channelID, threadTS, text string) {
	if c.cfg.BotToken == "" {
		return // nothing to send with; the conformance suite drives Inbound
	}
	payload := map[string]any{"channel": channelID, "text": text}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.BotToken)
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bonnie: slack: deliver: %v\n", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "bonnie: slack: deliver: %s\n", resp.Status)
	}
}

// writeOK answers Slack's webhook with the ack it expects.
func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}
