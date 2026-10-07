package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

var _ channel.TrackedReceiver = (*Channel)(nil)

// PrepareDispatch reserves a durable Slack destination for one dispatch.
func (c *Channel) PrepareDispatch(ctx context.Context, dispatchID string, target any) (channel.DispatchReceipt, error) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	address, ok := target.(string)
	if !ok || address == "" {
		return channel.DispatchReceipt{}, errors.New("bonnie: slack: target must be a non-empty channel ID")
	}
	if receipt, found, err := c.core.DispatchReceipt(ctx, dispatchID); err != nil || found {
		return receipt, err
	}
	root, posted := c.postMessageTS(ctx, address, "", "Scheduled task")
	if !posted || root == "" {
		return channel.DispatchReceipt{}, errors.New("bonnie: slack: cannot open scheduled thread")
	}
	return c.core.PrepareDispatch(ctx, dispatchID, address+"/"+root, target)
}

// RunDispatch runs a queued dispatch in its reserved Slack conversation.
func (c *Channel) RunDispatch(ctx context.Context, receipt channel.DispatchReceipt, text string, opts channel.SendOptions) (*runtime.Run, error) {
	if opts.Kind == "" {
		opts.Kind = chat.KindThread
	}
	if opts.TurnPolicy == "" {
		opts.TurnPolicy = channel.PolicyQueue
	}
	return c.core.RunDispatch(ctx, receipt, text, opts)
}

// DeliverDispatch sends the result to the receipt's thread. Unlike inbound
// delivery, an outbox must receive posting failures so it can retry.
func (c *Channel) DeliverDispatch(ctx context.Context, receipt channel.DispatchReceipt, run *runtime.Run) error {
	text := chat.DeliveryText(run, nil, "(Reply in this thread to answer.)")
	if text == "" {
		return nil
	}
	channelID, threadTS, ok := strings.Cut(receipt.Address, "/")
	if !ok {
		return fmt.Errorf("bonnie: slack: invalid dispatch address %q", receipt.Address)
	}
	if threadTS == "dm" {
		threadTS = ""
	}
	for _, part := range chat.SplitText(text, messageLimit, maxParts) {
		if _, posted := c.postMessageTS(ctx, channelID, threadTS, part); !posted {
			return fmt.Errorf("bonnie: slack: failed to deliver dispatch %q", receipt.DispatchID)
		}
	}
	return nil
}
