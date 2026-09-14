// Package discord is BONNIE's Discord inbound transport (L3).
//
// # The shape
//
// Discord POSTs one interaction per invocation to your webhook (the
// Interactions Endpoint). This adapter mounts one route (default
// `POST /discord/interactions`), verifies Discord's Ed25519 signature over
// the timestamped body, answers the `PING` handshake, and acknowledges a
// command with a deferred response within the three-second deadline — the
// turn runs in a goroutine the handler does not outlive, and the reply goes
// back through `POST /channels/{id}/messages` with the bot token.
//
// Setup on the Discord side: create an application and a bot, register one
// slash command (default name `ask`, with a required string option named
// `message`), set this route as the Interactions Endpoint URL, and copy the
// public key, bot token, and application ID into configuration.
//
// # Which messages are for the bot
//
// A slash command is the only invocation in this transport: Discord's
// webhooks see interactions, not message traffic — a plain reply in a
// channel never reaches a webhook. One channel (or thread) is one
// conversation: `/ask` again continues it, and `/ask` while a turn runs
// steers the running turn per the turn policy.
//
// # Human-in-the-loop
//
// When a run parks on a question, the prompt is delivered as a channel
// message with a note on how to answer. The next `/ask <answer>` on the
// same channel is that answer: the dispatch rule in `channel/chat` routes
// it to the run's resume, not to a new turn.
//
// # Limits, stated plainly
//
//   - Webhook only. The gateway (websocket) sees message traffic and would
//     allow plain-reply conversations; it is not implemented, and it would
//     need a websocket dependency BONNIE does not carry.
//   - A reply longer than 2000 characters is split, with a cap of five
//     parts and a truncation notice on the last.
//   - Message components (buttons) are not implemented; a run parked on a
//     structured approval is answered in text.
//   - Attachments are ignored.
package discord

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

// DefaultPath is the webhook path the route mounts at.
const DefaultPath = "/discord/interactions"

// messageLimit is Discord's per-message character limit; maxParts caps a
// single reply's flood.
const (
	messageLimit = 2000
	maxParts     = 5
)

// Interaction types and response types, the few this adapter speaks.
const (
	typePing               = 1
	typeAppCommand         = 2
	typeMessageWithSource  = 4
	typeDeferredWithSource = 5
)

// Config configures the Discord channel.
type Config struct {
	// BotToken is the bot token. Delivery needs it; receiving works without
	// it, which is what the conformance suite uses.
	BotToken string

	// PublicKey is the application's public key, hex-encoded, from the
	// Developer Portal. Set it: without verification, anyone who can reach
	// the webhook drives the agent.
	PublicKey string

	// Command is the slash command this adapter answers. The default is
	// "ask".
	Command string

	// APIURL overrides the Discord API base URL. Tests point it at a fake;
	// leave it empty in production.
	APIURL string

	// Path overrides the webhook route. The default is [DefaultPath].
	Path string
}

// Channel is the Discord transport. It implements [channel.Channel] and
// [channel.Inbound].
type Channel struct {
	core *chat.Core
	cfg  Config
	api  string
	key  ed25519.PublicKey
	http *http.Client
}

var (
	_ channel.Channel = (*Channel)(nil)
	_ channel.Inbound = (*Channel)(nil)
)

// New returns a Discord channel over a runner. A public key that does not
// parse is a misconfiguration that would leave the webhook unverified —
// New refuses rather than run wide open.
func New(r *runtime.Runner, cfg Config, opts ...chat.CoreOption) (*Channel, error) {
	if cfg.Command == "" {
		cfg.Command = "ask"
	}
	api := cfg.APIURL
	if api == "" {
		api = "https://discord.com/api/v10"
	}
	c := &Channel{
		core: chat.NewCore(r, "discord", channel.PolicySteer, opts...),
		cfg:  cfg,
		api:  api,
		http: &http.Client{Timeout: 15 * time.Second},
	}
	if cfg.PublicKey != "" {
		key, err := hex.DecodeString(cfg.PublicKey)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("bonnie: discord: the public key is not %d bytes of hex: %q", ed25519.PublicKeySize, cfg.PublicKey)
		}
		c.key = ed25519.PublicKey(key)
	}
	return c, nil
}

// Name implements [channel.Channel].
func (c *Channel) Name() string { return "discord" }

// Routes implements [channel.Channel].
func (c *Channel) Routes() []channel.Route {
	path := c.cfg.Path
	if path == "" {
		path = DefaultPath
	}
	return []channel.Route{
		{Method: http.MethodPost, Path: path, Handler: c.handleInteraction},
	}
}

// From implements [channel.Inbound].
func (c *Channel) From(address string) channel.SessionRef { return c.core.From(address) }

// Attach implements [channel.Inbound].
func (c *Channel) Attach(runID string) channel.SessionRef { return c.core.Attach(runID) }

// The wire types, the parts of an interaction this adapter reads.
type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type discordOption struct {
	Name  string `json:"name"`
	Type  int    `json:"type"`
	Value any    `json:"value"`
}

type discordData struct {
	Name    string          `json:"name"`
	Options []discordOption `json:"options"`
}

type discordInteraction struct {
	ID        string       `json:"id"`
	Type      int          `json:"type"`
	ChannelID string       `json:"channel_id"`
	Data      *discordData `json:"data"`
	Member    *struct {
		User *discordUser `json:"user"`
	} `json:"member"`
	User *discordUser `json:"user"`
}

// handleInteraction implements the webhook.
func (c *Channel) handleInteraction(w http.ResponseWriter, r *http.Request, _ channel.Inbound) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !c.verify(r.Header.Get("X-Signature-Ed25519"), r.Header.Get("X-Signature-Timestamp"), body) {
		// Not Discord. A probe learns only that the door did not open.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var in discordInteraction
	if err := json.Unmarshal(body, &in); err != nil {
		respond(w, typeMessageWithSource, "malformed interaction")
		return
	}

	// The handshake: Discord pings once, when an operator saves the
	// Interactions Endpoint URL, and expects the ping echoed.
	if in.Type == typePing {
		respond(w, typePing, "")
		return
	}

	switch {
	case in.Type != typeAppCommand || in.Data == nil || in.Data.Name != c.cfg.Command:
		respond(w, typeMessageWithSource, fmt.Sprintf("Use /%s to talk to the agent.", c.cfg.Command))
		return
	case commandText(in.Data) == "":
		respond(w, typeMessageWithSource, fmt.Sprintf("Tell me what to do: /%s <message>", c.cfg.Command))
		return
	}

	// Acknowledge inside the deadline, then work. The deferred response is
	// edited by nobody — delivery is a plain channel message, which also
	// survives turns that outlive the interaction token's 15 minutes.
	respond(w, typeDeferredWithSource, "")

	user := in.Member.User
	if user == nil {
		user = in.User
	}
	chat.Dispatch(r.Context(), c.core, chat.Turn{
		Address: "discord/" + in.ChannelID,
		Text:    commandText(in.Data),
		// Discord's webhook does not say whether the channel is a thread;
		// the interaction carries only its ID. One channel is one
		// conversation either way.
		Kind:    chat.KindChannel,
		Context: []string{"Discord user " + user.Username + " used /" + c.cfg.Command + " in channel " + in.ChannelID + "."},
		Auth: &channel.Principal{
			Authenticator: "discord",
			Kind:          "user",
			ID:            user.ID,
			Attributes: map[string]any{
				"username":   user.Username,
				"channel_id": in.ChannelID,
			},
		},
	}, c.deliver)
}

// commandText extracts the command's `message` option.
func commandText(d *discordData) string {
	for _, o := range d.Options {
		if o.Name == "message" {
			s, _ := o.Value.(string)
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// verify checks Discord's Ed25519 signature over the timestamp and the raw
// body, hex-encoded on the wire, compared with the constant-time curve
// check inside ed25519.Verify.
func (c *Channel) verify(signature, timestamp string, body []byte) bool {
	if c.key == nil {
		return true // unverified: the host accepts the risk by configuring so
	}
	sig, err := hex.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(c.key, []byte(timestamp+string(body)), sig)
}

// deliver posts a turn's outcome back to the channel the command came from.
// A failed post is logged, never retried in a loop — the run's result is in
// the journal, and `bonnie runs show` reads it back.
func (c *Channel) deliver(address string, run *runtime.Run, err error) {
	text := chat.DeliveryText(run, err, "(Answer with /"+c.cfg.Command+" <your answer>.)")
	if text == "" {
		return
	}
	channelID := strings.TrimPrefix(address, "discord/")
	for _, p := range chat.SplitText(text, messageLimit, maxParts) {
		c.postMessage(context.Background(), channelID, p)
	}
}

// postMessage posts one message. Fire-and-log: a delivery failure must not
// take the process down, and the journal keeps the truth.
func (c *Channel) postMessage(ctx context.Context, channelID, text string) {
	if c.cfg.BotToken == "" {
		return // nothing to send with; the conformance suite drives Inbound
	}
	body, _ := json.Marshal(map[string]any{"content": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/channels/"+channelID+"/messages", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+c.cfg.BotToken)
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bonnie: discord: deliver: %v\n", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "bonnie: discord: deliver: %s\n", resp.Status)
	}
}

// respond writes an interaction response.
func respond(w http.ResponseWriter, respType int, content string) {
	out := map[string]any{"type": respType}
	if content != "" {
		out["data"] = map[string]string{"content": content}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
