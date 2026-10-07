package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

var _ channel.TrackedReceiver = (*Channel)(nil)

// PrepareDispatch reserves the issue or pull-request conversation for a dispatch.
func (c *Channel) PrepareDispatch(ctx context.Context, dispatchID string, target any) (channel.DispatchReceipt, error) {
	t, ok := target.(Target)
	if !ok {
		data, err := json.Marshal(target)
		if err != nil {
			return channel.DispatchReceipt{}, err
		}
		if err := json.Unmarshal(data, &t); err != nil {
			return channel.DispatchReceipt{}, err
		}
	}
	if t.Owner == "" || t.Repo == "" || t.Number == 0 {
		return channel.DispatchReceipt{}, fmt.Errorf("bonnie: channel/github: dispatch target must be a valid github.Target, not %T", target)
	}
	if c.cfg.InstallationID == 0 {
		return channel.DispatchReceipt{}, errors.New("bonnie: channel/github: tracked dispatches need GITHUB_INSTALLATION_ID")
	}
	address := AddressIssue(t.Owner, t.Repo, t.Number)
	if t.PullRequest {
		address = AddressPullRequest(t.Owner, t.Repo, t.Number)
	}
	// Persist typed destination data in the receipt for delivery after restart.
	stored := struct {
		Target
		InstallationID int64 `json:"installation_id"`
	}{Target: t, InstallationID: c.cfg.InstallationID}
	return c.core.PrepareDispatch(ctx, dispatchID, address, stored)
}

// RunDispatch runs a dispatch on its reserved GitHub conversation.
func (c *Channel) RunDispatch(ctx context.Context, receipt channel.DispatchReceipt, text string, opts channel.SendOptions) (*runtime.Run, error) {
	if opts.Kind == "" {
		opts.Kind = chat.KindIssue
		var target Target
		if json.Unmarshal(receipt.Target, &target) == nil && target.PullRequest {
			opts.Kind = chat.KindPullRequest
		}
	}
	if opts.TurnPolicy == "" {
		opts.TurnPolicy = channel.PolicyQueue
	}
	return c.core.RunDispatch(ctx, receipt, text, opts)
}

// DeliverDispatch posts the dispatch result and returns errors so an outbox can retry.
func (c *Channel) DeliverDispatch(ctx context.Context, receipt channel.DispatchReceipt, run *runtime.Run) error {
	var destination struct {
		Target
		InstallationID int64 `json:"installation_id"`
	}
	if err := json.Unmarshal(receipt.Target, &destination); err != nil {
		return fmt.Errorf("bonnie: channel/github: decode dispatch target: %w", err)
	}
	t := destination.Target
	if t.Owner == "" || t.Repo == "" || t.Number == 0 || destination.InstallationID == 0 {
		return errors.New("bonnie: channel/github: invalid tracked dispatch destination")
	}
	text := chat.DeliveryText(run, nil, "(Reply in this thread to answer.)")
	if text == "" {
		return nil
	}
	address := AddressIssue(t.Owner, t.Repo, t.Number)
	if t.PullRequest {
		address = AddressPullRequest(t.Owner, t.Repo, t.Number)
	}
	path, ok := deliveryPath(receipt.Address, t.Owner, t.Repo)
	if !ok || receipt.Address != address {
		return fmt.Errorf("bonnie: channel/github: invalid dispatch address %q", receipt.Address)
	}
	env := &envelope{Repository: repo{Owner: actor{Login: t.Owner}, Name: t.Repo}, Installation: &installation{ID: destination.InstallationID}}
	for _, part := range chat.SplitText(text, issueLimit, maxParts) {
		token, err := c.installToken(ctx, env)
		if err != nil {
			return fmt.Errorf("bonnie: channel/github: create delivery token: %w", err)
		}
		_, posted := c.post.PostJSON(ctx, c.api+"/repos/"+t.Owner+"/"+t.Repo+path,
			http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/vnd.github+json"}}, map[string]string{"body": part})
		if !posted {
			return fmt.Errorf("bonnie: channel/github: failed to deliver dispatch %q", receipt.DispatchID)
		}
	}
	return nil
}
