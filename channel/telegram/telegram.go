// Package telegram is BONNIE's Telegram inbound transport (L3).
//
// # The shape
//
// Telegram POSTs one JSON update per message to your webhook. This adapter
// mounts one route (default `POST /telegram`), verifies the secret token in
// the `X-Telegram-Bot-Api-Secret-Token` header, and answers `200` at once —
// the turn runs in a goroutine the handler does not outlive, and the reply
// goes back through `sendMessage` with the bot token.
//
// Setup on the Telegram side: create a bot with BotFather, set the webhook
// with the same secret you configure here, and — for group chats — either
// keep Telegram's privacy mode on, in which case the bot receives only
// commands and mentions, or accept that it sees everything. Privacy mode on
// is the safe default and what this adapter assumes.
//
// # Which messages are for the bot
//
//   - A private chat: every text message.
//   - A group: `/ask <text>` (or `/ask@botname`), or a message that
//     mentions `@yourbot`.
//   - Replies inside a forum topic continue that topic's run.
//
// The DM case is one conversation per chat: the whole chat is the address.
// In groups, each topic is its own conversation.
//
// # Human-in-the-loop
//
// When a run parks on a question, the prompt is delivered as a message with
// a note to reply. The next text on the same address — the same chat, the
// same topic — is the answer: the dispatch rule in `channel/chat` routes it
// to the run's resume, not to a new turn.
//
// # Limits, stated plainly
//
//   - Webhook only. Long-polling `getUpdates` would let a laptop without a
//     public URL play too; it is not implemented. Telegram's own docs
//     describe `setWebhook`; the URL must be HTTPS.
//   - Messages arrive as plain text and replies are sent as plain text —
//     no `parse_mode`, so the model's markdown shows as written and cannot
//     inject Telegram's HTML formatting.
//   - A reply longer than 4096 characters is split, with a cap of five
//     parts and a truncation notice on the last.
//   - Cancel is not exposed: there is no Telegram UI gesture for it. A turn
//     that must be stopped ends through the HTTP channel or the journal.
//   - Attachments are ignored.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

// DefaultPath is the webhook path the route mounts at.
const DefaultPath = "/telegram"

// messageLimit is Telegram's per-message character limit; maxParts caps a
// single reply's flood.
const (
	messageLimit = 4096
	maxParts     = 5
)

// Config configures the Telegram channel.
type Config struct {
	// Token is the bot token from BotFather (starts `xoxb-`). Delivery
	// needs it; receiving works without it, which is what the conformance
	// suite uses. Configure it in production or replies go nowhere.
	Token string

	// Secret is the webhook secret shared with `setWebhook`. When set, every
	// request's `X-Telegram-Bot-Api-Secret-Token` header must match. Set it:
	// the header check is what stops a stranger from driving the agent by
	// POSTing to your webhook.
	Secret string

	// Username is the bot's username without the @. When set, group messages
	// count as for-the-bot when they mention it. Without it, only the
	// command reaches the bot in groups.
	Username string

	// Command is the group command, without the slash. The default is "ask".
	Command string

	// APIURL overrides the Telegram Bot API base URL. Tests point it at a
	// fake; leave it empty in production.
	APIURL string

	// Path overrides the webhook route. The default is [DefaultPath].
	Path string
}

// Channel is the Telegram transport. It implements [channel.Channel] and
// [channel.Inbound].
type Channel struct {
	core *chat.Core
	cfg  Config
	api  string
	http *http.Client
}

var (
	_ channel.Channel = (*Channel)(nil)
	_ channel.Inbound = (*Channel)(nil)
)

// New returns a Telegram channel over a runner.
func New(r *runtime.Runner, cfg Config, opts ...chat.CoreOption) *Channel {
	if cfg.Command == "" {
		cfg.Command = "ask"
	}
	api := cfg.APIURL
	if api == "" {
		api = "https://api.telegram.org"
	}
	return &Channel{
		core: chat.NewCore(r, channel.PolicySteer, opts...),
		cfg:  cfg,
		api:  api,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// Name implements [channel.Channel].
func (c *Channel) Name() string { return "telegram" }

// Routes implements [channel.Channel].
func (c *Channel) Routes() []channel.Route {
	path := c.cfg.Path
	if path == "" {
		path = DefaultPath
	}
	return []channel.Route{
		{Method: http.MethodPost, Path: path, Handler: c.handleUpdate},
	}
}

// From implements [channel.Inbound].
func (c *Channel) From(address string) channel.SessionRef { return c.core.From(address) }

// Attach implements [channel.Inbound].
func (c *Channel) Attach(runID string) channel.SessionRef { return c.core.Attach(runID) }

// The wire types, the parts of a Telegram update this adapter reads.
type tgFrom struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsBot    bool   `json:"is_bot"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private", "group", "supergroup", "channel"
}

type tgEntity struct {
	Type   string `json:"type"` // "mention", "bot_command"
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type tgMessage struct {
	Text            string     `json:"text"`
	From            *tgFrom    `json:"from"`
	Chat            tgChat     `json:"chat"`
	MessageThreadID int64      `json:"message_thread_id"` // a forum topic
	Entities        []tgEntity `json:"entities"`
}

type tgUpdate struct {
	Message *tgMessage `json:"message"`
}

// handleUpdate implements the webhook.
func (c *Channel) handleUpdate(w http.ResponseWriter, r *http.Request, _ channel.Inbound) {
	if c.cfg.Secret != "" && r.Header.Get("X-Telegram-Bot-Api-Secret-Token") != c.cfg.Secret {
		// Not Telegram. Say nothing about why: a probe learns only that the
		// door did not open.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var u tgUpdate
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(body).Decode(&u); err != nil {
		writeOK(w)
		return
	}
	writeOK(w)

	if u.Message == nil || u.Message.Text == "" || u.Message.From == nil || u.Message.From.IsBot {
		return
	}
	text, ok := c.forUs(u.Message)
	if !ok {
		return
	}

	opts := channel.SendOptions{Auth: &channel.Principal{
		Authenticator: "telegram",
		Kind:          "user",
		ID:            strconv.FormatInt(u.Message.From.ID, 10),
		Attributes: map[string]any{
			"username": u.Message.From.Username,
			"chat_id":  u.Message.Chat.ID,
		},
	}}
	addr := c.address(u.Message)
	chat.Dispatch(r.Context(), c.core, addr, text, opts, c.deliver)
}

// forUs reports whether a message reaches the agent, and returns the text
// with the invocation stripped.
func (c *Channel) forUs(m *tgMessage) (string, bool) {
	switch m.Chat.Type {
	case "private":
		// A DM is always for the bot; a command token may still prefix the
		// text, so strip it, but its presence does not decide.
		text, _ := stripCommand(m.Text, c.cfg.Command, c.cfg.Username)
		return text, true
	case "group", "supergroup":
		mentioned := false
		for _, e := range m.Entities {
			if e.Type != "mention" || e.Offset+e.Length > len(m.Text) {
				continue
			}
			if m.Text[e.Offset:e.Offset+e.Length] == "@"+c.cfg.Username {
				mentioned = true
			}
		}
		text, isCmd := stripCommand(m.Text, c.cfg.Command, c.cfg.Username)
		if mentioned || isCmd {
			return text, true
		}
		return "", false
	default:
		return "", false // channels and anything new: not for the agent
	}
}

// stripCommand removes a leading "/cmd" or "/cmd@botname" token. It reports
// whether a matching command was present.
func stripCommand(text, command, username string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return text, false
	}
	token, rest, _ := strings.Cut(text[1:], " ")
	name, target, hasTarget := strings.Cut(token, "@")
	if name != command {
		return text, false
	}
	if hasTarget && target != username {
		return text, false // the command targets another bot
	}
	return strings.TrimSpace(rest), true
}

// address is the channel-local address of a message: one conversation per
// chat, or per forum topic when the chat has them.
func (c *Channel) address(m *tgMessage) string {
	a := fmt.Sprintf("telegram/%d", m.Chat.ID)
	if m.MessageThreadID != 0 {
		a += fmt.Sprintf("/%d", m.MessageThreadID)
	}
	return a
}

// deliver posts a turn's outcome back to the chat the message came from.
// The address encodes the target; a failed post is logged, never retried in
// a loop — the run's result is in the journal, and `bonnie runs show` reads
// it back.
func (c *Channel) deliver(address string, run *runtime.Run, err error) {
	text := chat.DeliveryText(run, err, "(Reply in this chat to answer.)")
	if text == "" {
		return
	}
	parts := strings.SplitN(address, "/", 3)
	chatID, _ := strconv.ParseInt(parts[1], 10, 64)
	var thread int64
	if len(parts) > 2 {
		thread, _ = strconv.ParseInt(parts[2], 10, 64)
	}
	for _, p := range chat.SplitText(text, messageLimit, maxParts) {
		c.sendMessage(context.Background(), chatID, thread, p)
	}
}

// sendMessage posts one message. Fire-and-log: a delivery failure must not
// take the process down, and the journal keeps the truth.
func (c *Channel) sendMessage(ctx context.Context, chatID, thread int64, text string) {
	if c.cfg.Token == "" {
		return // nothing to send with; the conformance suite drives Inbound
	}
	payload := map[string]any{"chat_id": chatID, "text": text}
	if thread != 0 {
		payload["message_thread_id"] = thread
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/bot%s/sendMessage", c.api, c.cfg.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bonnie: telegram: deliver: %v\n", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "bonnie: telegram: deliver: %s\n", resp.Status)
	}
}

// writeOK answers Telegram's webhook with the body it expects.
func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}
