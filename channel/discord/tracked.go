package discord

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

var _ channel.TrackedReceiver = (*Channel)(nil)

// PrepareDispatch reserves the Discord channel or thread for a durable dispatch.
func (c *Channel) PrepareDispatch(ctx context.Context, dispatchID string, target any) (channel.DispatchReceipt, error) {
	address, ok := target.(string)
	if !ok || address == "" {
		return channel.DispatchReceipt{}, fmt.Errorf("bonnie: channel/discord: the target of a hand-off is the channel ID, not %T", target)
	}
	return c.core.PrepareDispatch(ctx, dispatchID, address, target)
}

// RunDispatch runs a dispatch on its reserved Discord conversation.
func (c *Channel) RunDispatch(ctx context.Context, receipt channel.DispatchReceipt, text string, opts channel.SendOptions) (*runtime.Run, error) {
	if opts.Kind == "" {
		opts.Kind = chat.KindChannel
	}
	if opts.TurnPolicy == "" {
		opts.TurnPolicy = channel.PolicyQueue
	}
	return c.core.RunDispatch(ctx, receipt, text, opts)
}

// DeliverDispatch posts the completed result to its reserved Discord destination.
func (c *Channel) DeliverDispatch(ctx context.Context, receipt channel.DispatchReceipt, run *runtime.Run) error {
	choices := chat.Choices(run)
	hint := "(Answer with /" + c.cfg.Command + " <your answer>.)"
	if len(choices) > 0 {
		hint = ""
	}
	text := chat.DeliveryText(run, nil, hint)
	if text == "" {
		return nil
	}
	if receipt.Address == "" {
		return errors.New("bonnie: discord: invalid dispatch address")
	}
	if c.cfg.BotToken == "" {
		return errors.New("bonnie: discord: bot token is required to deliver a tracked dispatch")
	}
	parts := chat.SplitText(text, messageLimit, maxParts)
	for i, part := range parts {
		var components []any
		if i == len(parts)-1 {
			components = buttonRows(choices)
		}
		payload := map[string]any{"content": part}
		if len(components) > 0 {
			payload["components"] = components
		}
		_, ok := c.post.PostJSON(ctx, c.api+"/channels/"+receipt.Address+"/messages", chat.BearerHeader("Bot", c.cfg.BotToken), payload)
		if !ok {
			return fmt.Errorf("bonnie: discord: failed to deliver dispatch %q", receipt.DispatchID)
		}
	}
	return nil
}
