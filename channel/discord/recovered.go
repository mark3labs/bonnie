package discord

import (
	"context"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

var _ channel.Recoverer = (*Channel)(nil)

// DeliverRecovered implements [channel.Recoverer]. A run that the host
// continued after a restart answers in the conversation that started it,
// exactly as a turn the webhook started would have.
func (c *Channel) DeliverRecovered(ctx context.Context, runID string, run *runtime.Run, err error) error {
	return c.core.DeliverRecovered(ctx, runID, run, err, c.deliver)
}
