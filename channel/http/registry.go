package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/runtime"
)

// The address map lives in a reserved run so it inherits the journal's
// durability. A process-local map would lose every Slack thread on restart,
// which is the failure this whole layer exists to prevent.
const (
	addressRun       = runtime.ReservedRunPrefix + "addresses"
	addressExtType   = "channel.address"
	principalExtType = "channel.principal"
)

// registry maps a channel-local address to the run that serves it, and
// remembers who last spoke on a run.
type registry struct {
	journal runtime.Journal

	mu     sync.Mutex
	byAddr map[string]string
	loaded bool
}

func newRegistry(j runtime.Journal) *registry {
	return &registry{journal: j, byAddr: make(map[string]string)}
}

// addressBinding is the durable form of one address-to-run mapping.
type addressBinding struct {
	Address string `json:"address"`
	RunID   string `json:"run_id"`
}

// principalNote records who spoke on a run. BONNIE carries the identity; it
// does not verify it. Verification belongs to the host in v0.1.0.
type principalNote struct {
	RunID     string             `json:"run_id"`
	Principal *channel.Principal `json:"principal,omitempty"`
}

// loadLocked replays the reserved run once. The caller must hold r.mu.
func (r *registry) loadLocked(ctx context.Context) error {
	if r.loaded {
		return nil
	}
	recs, err := r.journal.Replay(ctx, addressRun)
	switch {
	case errors.Is(err, runtime.ErrRunNotFound):
		r.loaded = true
		return nil
	case err != nil:
		return fmt.Errorf("bonnie: load address map: %w", err)
	}

	for _, rec := range recs {
		if rec.ExtType != addressExtType || len(rec.Payload) == 0 {
			continue
		}
		var b addressBinding
		if err := json.Unmarshal(rec.Payload, &b); err != nil {
			continue
		}
		// Last binding wins, so re-keying an address is just another append.
		r.byAddr[b.Address] = b.RunID
	}
	r.loaded = true
	return nil
}

// resolve returns the run that serves an address, creating and binding one on
// first sight.
func (r *registry) resolve(ctx context.Context, address string, newID func() string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.loadLocked(ctx); err != nil {
		return "", err
	}
	if runID, ok := r.byAddr[address]; ok {
		return runID, nil
	}

	runID := newID()
	if err := r.bindLocked(ctx, address, runID); err != nil {
		return "", err
	}
	return runID, nil
}

// Bind points an address at a run, replacing any earlier binding. Use it to
// re-key an address — to start a fresh conversation in the same Slack thread,
// for example.
func (r *registry) bind(ctx context.Context, address, runID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.loadLocked(ctx); err != nil {
		return err
	}
	return r.bindLocked(ctx, address, runID)
}

func (r *registry) bindLocked(ctx context.Context, address, runID string) error {
	payload, err := json.Marshal(addressBinding{Address: address, RunID: runID})
	if err != nil {
		return fmt.Errorf("bonnie: encode address binding: %w", err)
	}
	if _, err := r.journal.Append(ctx, runtime.Record{
		RunID:   addressRun,
		Kind:    runtime.RecordExtensionData,
		ExtType: addressExtType,
		Text:    address,
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("bonnie: bind address: %w", err)
	}
	r.byAddr[address] = runID
	return nil
}

// notePrincipal records the identity that sent a message.
func (r *registry) notePrincipal(ctx context.Context, runID string, p *channel.Principal) error {
	payload, err := json.Marshal(principalNote{RunID: runID, Principal: p})
	if err != nil {
		return fmt.Errorf("bonnie: encode principal: %w", err)
	}
	if _, err := r.journal.Append(ctx, runtime.Record{
		RunID:   addressRun,
		Kind:    runtime.RecordExtensionData,
		ExtType: principalExtType,
		Text:    runID,
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("bonnie: record principal: %w", err)
	}
	return nil
}
