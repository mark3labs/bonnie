package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

var _ channel.TrackedReceiver = (*Channel)(nil)

// PrepareDispatch reserves the Telegram destination for a durable dispatch.
func (c *Channel) PrepareDispatch(ctx context.Context, dispatchID string, target any) (channel.DispatchReceipt, error) {
	s, ok := target.(string)
	if !ok || s == "" {
		return channel.DispatchReceipt{}, fmt.Errorf("bonnie: channel/telegram: the target of a hand-off is the chat ID, or \"<chat_id>/<topic>\", not %T", target)
	}
	chatPart, topicPart, _ := strings.Cut(s, "/")
	_, err := strconv.ParseInt(chatPart, 10, 64)
	if err != nil {
		return channel.DispatchReceipt{}, fmt.Errorf("bonnie: channel/telegram: the target of a hand-off is the chat ID, or \"<chat_id>/<topic>\", not %q", s)
	}
	if topicPart != "" {
		if _, err := strconv.ParseInt(topicPart, 10, 64); err != nil {
			return channel.DispatchReceipt{}, fmt.Errorf("bonnie: channel/telegram: the topic of a hand-off is a number, not %q", topicPart)
		}
	}
	return c.core.PrepareDispatch(ctx, dispatchID, s, target)
}

// RunDispatch runs a dispatch on its reserved Telegram conversation.
func (c *Channel) RunDispatch(ctx context.Context, receipt channel.DispatchReceipt, text string, opts channel.SendOptions) (*runtime.Run, error) {
	if opts.Kind == "" {
		switch {
		case strings.Contains(receipt.Address, "/"):
			opts.Kind = chat.KindThread
		default:
			id, _ := strconv.ParseInt(receipt.Address, 10, 64)
			if id > 0 {
				opts.Kind = chat.KindDM
			} else {
				opts.Kind = chat.KindChannel
			}
		}
	}
	if opts.TurnPolicy == "" {
		opts.TurnPolicy = channel.PolicyQueue
	}
	return c.core.RunDispatch(ctx, receipt, text, opts)
}

// DeliverDispatch posts the completed result to its reserved Telegram destination.
func (c *Channel) DeliverDispatch(ctx context.Context, receipt channel.DispatchReceipt, run *runtime.Run) error {
	text := chat.DeliveryText(run, nil, "(Reply in this chat to answer.)")
	if text == "" {
		return nil
	}
	chatPart, topicPart, _ := strings.Cut(receipt.Address, "/")
	chatID, err := strconv.ParseInt(chatPart, 10, 64)
	if err != nil {
		return fmt.Errorf("bonnie: telegram: invalid dispatch address %q", receipt.Address)
	}
	var thread int64
	if topicPart != "" {
		thread, err = strconv.ParseInt(topicPart, 10, 64)
		if err != nil {
			return fmt.Errorf("bonnie: telegram: invalid dispatch address %q", receipt.Address)
		}
	}
	if c.cfg.Token == "" {
		return errors.New("bonnie: telegram: bot token is required to deliver a tracked dispatch")
	}
	for _, part := range chat.SplitText(text, messageLimit, maxParts) {
		payload := map[string]any{"chat_id": chatID, "text": part}
		if thread != 0 {
			payload["message_thread_id"] = thread
		}
		answer, ok := c.post.PostJSON(ctx, fmt.Sprintf("%s/bot%s/sendMessage", c.api, c.cfg.Token), nil, payload)
		if !ok {
			return fmt.Errorf("bonnie: telegram: failed to deliver dispatch %q", receipt.DispatchID)
		}
		var response struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(answer, &response); err != nil || !response.OK {
			return fmt.Errorf("bonnie: telegram: failed to deliver dispatch %q: Telegram rejected the message", receipt.DispatchID)
		}
	}
	return nil
}
