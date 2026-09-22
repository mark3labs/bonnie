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
//   - A run parked on an approval, or on a question with options, is
//     delivered with buttons; pressing one answers it. An approval answers
//     with a verdict rather than with the word on the button. A question
//     with no options, or with more than twenty-five, is answered in text.
//   - Attachments are ignored.
package discord

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	typeMessageComponent   = 3
	typeMessageWithSource  = 4
	typeDeferredWithSource = 5
	typeUpdateMessage      = 7
)

// Message component types, for the controls a parked run offers.
const (
	componentActionRow = 1
	componentButton    = 2
)

// Button styles. An approval reads better when the two verdicts do not look
// alike: pressing the wrong one is not recoverable by pressing the other.
const (
	buttonPrimary = 1
	buttonDanger  = 4
)

// buttonsPerRow is Discord's limit inside one action row, and maxRows is the
// limit on rows in one message. Together they cap a message at 25 controls;
// a suspension offering more is delivered as text, because a wall of
// buttons is not a choice a person can make.
const (
	buttonsPerRow = 5
	maxRows       = 5
)

// Config configures the Discord channel.
type Config struct {
	// BotToken is the bot token. Delivery needs it; receiving works without
	// it, which is what the conformance suite uses.
	BotToken string

	// PublicKey is the application's public key, hex-encoded, from the
	// Developer Portal. It is REQUIRED: [New] refuses an empty or
	// unparsable one with [channel.ErrUnverifiedWebhook], because without
	// verification anyone who can reach the webhook drives the agent under
	// any identity they care to claim.
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
	post chat.Delivery
}

var (
	_ channel.Channel = (*Channel)(nil)
	_ channel.Inbound = (*Channel)(nil)
)

// New returns a Discord channel over a runner.
//
// It refuses a config with no PublicKey, and one whose key does not parse:
// either leaves the webhook unable to tell Discord from anyone who found
// the URL. See [channel.ErrUnverifiedWebhook]. A BotToken stays optional —
// a channel without one receives and runs turns but cannot answer.
//
// Nothing is built before the key is proven, so a refused config never
// reaches the runner.
func New(r *runtime.Runner, cfg Config, opts ...chat.CoreOption) (*Channel, error) {
	if cfg.PublicKey == "" {
		return nil, fmt.Errorf("%w: discord needs PublicKey (DISCORD_PUBLIC_KEY) from the "+
			"application's General Information page, to prove an interaction came from Discord",
			channel.ErrUnverifiedWebhook)
	}
	key, err := hex.DecodeString(cfg.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bonnie: discord: the public key is not %d bytes of hex: %q", ed25519.PublicKeySize, cfg.PublicKey)
	}
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
		key:  ed25519.PublicKey(key),
		http: &http.Client{Timeout: 15 * time.Second},
	}
	c.post = chat.Delivery{Client: c.http, Prefix: "discord"}
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
	// CustomID identifies which control was pressed. It carries the token
	// [chat.Choices] minted, and only a message-component interaction has
	// one.
	CustomID string `json:"custom_id"`
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
	// Message is the message a pressed control belongs to. Its content is
	// reused when the press is acknowledged, so the question stays readable
	// after its buttons are taken away.
	Message *struct {
		Content string `json:"content"`
	} `json:"message"`
}

// handleInteraction implements the webhook.
func (c *Channel) handleInteraction(w http.ResponseWriter, r *http.Request, _ channel.Inbound, _ channel.Outbound) {
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

	// A pressed control answers a parked run. It arrives on this same
	// route, which is why buttons cost Discord no new endpoint and no
	// change in the Developer Portal.
	if in.Type == typeMessageComponent {
		c.handlePress(w, r, in)
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

	user := interactionUser(in)
	if user == nil {
		user = &discordUser{}
	}
	chat.Dispatch(r.Context(), c.core, chat.Turn{
		Address: in.ChannelID,
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

// handlePress answers a parked run from a control the person pressed.
//
// It acknowledges by REPLACING the message with the same text and no
// components, which takes the buttons away. That is the double-press guard:
// a question is asked once, and two people reading the same channel must not
// both answer it. The runtime refuses the second answer anyway —
// [runtime.ErrNotWaiting] — but a button that visibly stops being a button
// is a better explanation than an error arriving later.
func (c *Channel) handlePress(w http.ResponseWriter, r *http.Request, in discordInteraction) {
	if in.Data == nil || in.Data.CustomID == "" {
		respond(w, typeMessageWithSource, "That control carried nothing to act on.")
		return
	}

	// The run this channel is parked on decides what the token means. A
	// control from an answered question resolves against a suspension that
	// has moved on, and Answer refuses it.
	runID, bound, err := c.core.Lookup(r.Context(), in.ChannelID)
	if err != nil || !bound {
		respond(w, typeMessageWithSource, "There is no conversation here to answer.")
		return
	}
	run, err := c.core.Runner().Snapshot(r.Context(), runID)
	if err != nil {
		respond(w, typeMessageWithSource, "There is no conversation here to answer.")
		return
	}
	responses, ok := chat.Answer(run.Suspend, in.Data.CustomID)
	if !ok {
		// Either the question was already answered or this control belongs
		// to an older one. Say so, and leave the message alone: another
		// person's press may still be in flight against it.
		respond(w, typeMessageWithSource, "That question has already been answered.")
		return
	}

	// Take the buttons away inside Discord's three-second deadline, then
	// let the turn run for as long as it needs.
	prompt := ""
	if in.Message != nil {
		prompt = in.Message.Content
	}
	respondUpdate(w, prompt+"\n\n"+pressNote(in, run.Suspend, in.Data.CustomID))

	chat.DispatchAnswer(r.Context(), c.core, in.ChannelID, responses, c.deliver)
}

// pressNote records who answered and how, so the message the buttons left
// behind still says what happened.
func pressNote(in discordInteraction, sus *runtime.SuspendRequest, token string) string {
	var who string
	if u := interactionUser(in); u != nil {
		who = u.Username
	}
	choice := token
	for _, ch := range chat.Choices(&runtime.Run{State: runtime.RunWaiting, Suspend: sus}) {
		if ch.Token == token {
			choice = ch.Label
			break
		}
	}
	if who == "" {
		return "_Answered: " + choice + "_"
	}
	return "_" + choice + " — " + who + "_"
}

// interactionUser is the person behind an interaction: a guild interaction
// carries a member, a DM carries a bare user.
func interactionUser(in discordInteraction) *discordUser {
	if in.Member != nil && in.Member.User != nil {
		return in.Member.User
	}
	return in.User
}

// Receive implements [channel.Receiver]. The target is a channel or thread
// ID: the instruction is posted there, the address binds before the turn
// runs, and the reply lands in the same place. Discord's IDs are
// snowflakes with nothing in them to say which of the two it is, so the
// kind is the channel it is delivered to.
func (c *Channel) Receive(ctx context.Context, target any, text string, opts channel.SendOptions) error {
	channelID, ok := target.(string)
	if !ok || channelID == "" {
		return fmt.Errorf("bonnie: channel/discord: the target of a hand-off is the channel ID, not %T", target)
	}
	return c.core.Proactive(ctx, chat.Turn{
		Address:    channelID,
		Text:       text,
		Kind:       chat.KindChannel,
		Context:    opts.Context,
		Title:      opts.Title,
		TurnPolicy: opts.TurnPolicy,
		Auth:       opts.Auth,
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
		// Unreachable through [New], which refuses an empty public key.
		// False rather than true so the only failure mode is the safe one;
		// see the same guard in package slack.
		return false
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
//
// A run parked on a question the person can answer by pressing carries its
// controls on the LAST part of the reply, because a long prompt is split
// and buttons under the first part would sit above the rest of the
// question.
func (c *Channel) deliver(address string, run *runtime.Run, err error) {
	choices := chat.Choices(run)
	hint := "(Answer with /" + c.cfg.Command + " <your answer>.)"
	if len(choices) > 0 {
		// The controls say how to answer better than a sentence does, and
		// telling someone to type when a button is in front of them is
		// noise.
		hint = ""
	}
	text := chat.DeliveryText(run, err, hint)
	if text == "" {
		return
	}
	// The address is the channel ID itself; the framework's channel prefix
	// never reaches here.
	parts := chat.SplitText(text, messageLimit, maxParts)
	for i, p := range parts {
		var components []any
		if i == len(parts)-1 {
			components = buttonRows(choices)
		}
		c.postMessage(context.Background(), address, p, components)
	}
}

// buttonRows lays choices out as Discord action rows, or nil when there is
// nothing to draw or too much. Discord allows five buttons per row and five
// rows; a suspension offering more than that is delivered as text, because
// twenty-five buttons is not a choice a person can make.
func buttonRows(choices []chat.Choice) []any {
	if len(choices) == 0 || len(choices) > buttonsPerRow*maxRows {
		return nil
	}
	var rows []any
	for start := 0; start < len(choices); start += buttonsPerRow {
		end := min(start+buttonsPerRow, len(choices))
		var buttons []any
		for _, ch := range choices[start:end] {
			buttons = append(buttons, map[string]any{
				"type":      componentButton,
				"style":     buttonStyle(ch.Label),
				"label":     ch.Label,
				"custom_id": ch.Token,
			})
		}
		rows = append(rows, map[string]any{
			"type":       componentActionRow,
			"components": buttons,
		})
	}
	return rows
}

// buttonStyle makes a rejection look unlike an approval. Pressing the wrong
// one of those two is not recoverable by pressing the other.
func buttonStyle(label string) int {
	if label == "Reject" {
		return buttonDanger
	}
	return buttonPrimary
}

// postMessage posts one message, with optional controls beneath it.
// Fire-and-log: a delivery failure must not take the process down, and the
// journal keeps the truth.
func (c *Channel) postMessage(ctx context.Context, channelID, text string, components []any) {
	if c.cfg.BotToken == "" {
		return // nothing to send with; the conformance suite drives Inbound
	}
	payload := map[string]any{"content": text}
	if len(components) > 0 {
		payload["components"] = components
	}
	c.post.PostJSON(ctx, c.api+"/channels/"+channelID+"/messages",
		chat.BearerHeader("Bot", c.cfg.BotToken), payload)
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

// respondUpdate replaces the message a control belongs to, with no controls
// on it. The empty components array is what removes them: omitting the key
// would leave the buttons in place.
func respondUpdate(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": typeUpdateMessage,
		"data": map[string]any{
			"content":    content,
			"components": []any{},
		},
	})
}
