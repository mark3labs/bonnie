package bonnie

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/presence"
)

// PresenceConfig configures optional worker registration during [Agent.Run].
// Presence is advisory; it does not assign work or coordinate execution.
type PresenceConfig struct {
	// Registry is required. The host does not close it.
	Registry presence.Registry
	// WorkerID is the required stable worker name. It must match each mounted
	// channel's non-empty [channel.WorkerIdentity].
	WorkerID string
	// InstanceID distinguishes process instances. When empty, the host generates
	// a random ID for each Run. Do not reuse it across simultaneous instances.
	InstanceID string
	// Labels holds discovery metadata, not authorization claims.
	Labels map[string]string
	// RefreshInterval defaults to 10 seconds. Negative values are invalid.
	// It must be less than the registry TTL when it implements [presence.TTLStore].
	RefreshInterval time.Duration
	// Endpoints replaces the full provider endpoint list when entries are supplied.
	// When omitted, the host collects endpoints from [channel.PresenceProvider]
	// implementations, including HTTP. Supply public URLs explicitly; the host
	// does not infer them from bind addresses. Override readiness follows host state.
	Endpoints []presence.Endpoint
}

// WithPresence enables worker presence registration after channel startup.
// WorkerID and Registry are required; InstanceID is generated when omitted.
// It copies labels and endpoints. Endpoint overrides replace provider endpoints.
// Registration and refresh failures stop [Agent.Run]. Shutdown attempts to publish
// draining and unregister with a context bounded by [WithShutdownTimeout].
func WithPresence(cfg PresenceConfig) Option {
	cfg.Labels = maps.Clone(cfg.Labels)
	cfg.Endpoints = append([]presence.Endpoint(nil), cfg.Endpoints...)
	return func(c *config) { c.presence = &cfg }
}

func (p PresenceConfig) identity() (presence.Identity, error) {
	if p.Registry == nil || p.WorkerID == "" {
		return presence.Identity{}, errors.New("bonnie: presence requires Registry and WorkerID")
	}
	if p.RefreshInterval == 0 {
		p.RefreshInterval = 10 * time.Second
	}
	if p.RefreshInterval < 0 {
		return presence.Identity{}, errors.New("bonnie: presence refresh interval must be positive")
	}
	if p.InstanceID == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return presence.Identity{}, fmt.Errorf("bonnie: presence instance ID: %w", err)
		}
		p.InstanceID = hex.EncodeToString(b)
	}
	return presence.Identity{Worker: p.WorkerID, Instance: p.InstanceID}, nil
}

type presenceLifecycle struct {
	cfg      PresenceConfig
	id       presence.Identity
	channels []Channel
}

func newPresenceLifecycle(cfg PresenceConfig, channels []Channel) (*presenceLifecycle, error) {
	id, err := cfg.identity()
	if err != nil {
		return nil, err
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 10 * time.Second
	}
	if ttl, ok := cfg.Registry.(presence.TTLStore); ok && cfg.RefreshInterval >= ttl.TTL() {
		return nil, fmt.Errorf("bonnie: presence refresh interval %s must be less than registry TTL %s", cfg.RefreshInterval, ttl.TTL())
	}
	for _, ch := range channels {
		if worker, ok := ch.(channel.WorkerIdentity); ok && worker.WorkerIdentity() != "" && worker.WorkerIdentity() != cfg.WorkerID {
			return nil, fmt.Errorf("bonnie: channel %q worker identity differs from presence WorkerID", ch.Name())
		}
	}
	return &presenceLifecycle{cfg: cfg, id: id, channels: channels}, nil
}
func (p *presenceLifecycle) record(state presence.State) presence.Record {
	eps := append([]presence.Endpoint(nil), p.cfg.Endpoints...)
	if p.cfg.Endpoints == nil {
		for _, ch := range p.channels {
			if provider, ok := ch.(channel.PresenceProvider); ok {
				eps = append(eps, provider.PresenceEndpoints()...)
			}
		}
	}
	for i := range eps {
		if p.cfg.Endpoints != nil {
			eps[i].Ready = state == presence.Ready
		}
		if state != presence.Ready {
			eps[i].Ready = false
		}
	}
	return presence.Record{Identity: p.id, Endpoints: eps, Labels: maps.Clone(p.cfg.Labels), State: state}
}
func (p *presenceLifecycle) register(ctx context.Context, state presence.State) error {
	return p.cfg.Registry.Register(ctx, p.record(state))
}
func (p *presenceLifecycle) run(ctx context.Context) error {
	ticker := time.NewTicker(p.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.register(ctx, "ready"); err != nil {
				return fmt.Errorf("bonnie: refresh presence: %w", err)
			}
		}
	}
}
