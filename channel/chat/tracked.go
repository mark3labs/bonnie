package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// DispatchReceipt returns a saved receipt for this channel and dispatch.
func (c *Core) DispatchReceipt(ctx context.Context, id string) (channel.DispatchReceipt, bool, error) {
	recs, err := c.runner.Journal().Replay(ctx, runtime.ReservedRunPrefix+"dispatch."+c.name)
	if errors.Is(err, runtime.ErrRunNotFound) {
		return channel.DispatchReceipt{}, false, nil
	}
	if err != nil {
		return channel.DispatchReceipt{}, false, err
	}
	for _, rec := range recs {
		var receipt channel.DispatchReceipt
		if err := json.Unmarshal(rec.Payload, &receipt); err != nil {
			return receipt, false, fmt.Errorf("bonnie: decode dispatch receipt: %w", err)
		}
		if receipt.DispatchID == id {
			return receipt, true, nil
		}
	}
	return channel.DispatchReceipt{}, false, nil
}

// SaveDispatchReceipt stores the channel-local address before execution.
// A platform can accept a new surface before this write. That crash window
// requires platform reconciliation; it does not have exactly-once semantics.
func (c *Core) SaveDispatchReceipt(ctx context.Context, receipt channel.DispatchReceipt) error {
	data, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("bonnie: encode dispatch receipt: %w", err)
	}
	_, err = c.runner.Journal().Append(ctx, runtime.Record{RunID: runtime.ReservedRunPrefix + "dispatch." + c.name, Kind: runtime.RecordExtensionData, Payload: data})
	return err
}

// PrepareDispatch reserves a stable address once per dispatch ID.
func (c *Core) PrepareDispatch(ctx context.Context, id, address string, target any) (channel.DispatchReceipt, error) {
	if id == "" || address == "" {
		return channel.DispatchReceipt{}, errors.New("bonnie: dispatch ID and address are required")
	}
	unlock := c.locks.Lock("dispatch/" + id)
	defer unlock()
	if receipt, ok, err := c.DispatchReceipt(ctx, id); err != nil || ok {
		return receipt, err
	}
	data, err := json.Marshal(target)
	if err != nil {
		return channel.DispatchReceipt{}, fmt.Errorf("bonnie: encode target: %w", err)
	}
	runID, err := c.From(address).RunID(ctx)
	if err != nil {
		return channel.DispatchReceipt{}, err
	}
	receipt := channel.DispatchReceipt{DispatchID: id, RunID: runID, Address: address, Target: data}
	return receipt, c.SaveDispatchReceipt(ctx, receipt)
}

// RunDispatch runs a queued turn on the reserved run, without delivery.
func (c *Core) RunDispatch(ctx context.Context, receipt channel.DispatchReceipt, text string, opts channel.SendOptions) (*runtime.Run, error) {
	if opts.TurnPolicy != "" && opts.TurnPolicy != channel.PolicyQueue {
		return nil, fmt.Errorf("%w: tracked dispatch requires queue", channel.ErrUnknownTurnPolicy)
	}
	unlock := c.locks.Lock(receipt.RunID)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Auth != nil {
		if err := c.addresses.NotePrincipal(ctx, receipt.RunID, opts.Auth); err != nil {
			return nil, err
		}
	}
	return c.runner.Start(ctx, receipt.RunID, runtime.Input{Text: text, Context: opts.Context, Title: opts.Title, Trigger: opts.Trigger, Origin: runtime.Origin{Channel: c.name, Kind: opts.Kind}})
}
