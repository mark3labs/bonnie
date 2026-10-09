// Package nats provides a JetStream KV presence store.
package nats

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/bonnie/presence"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrConflict reports ownership by another live worker instance.
// It is the transport-independent presence conflict sentinel.
var ErrConflict = presence.ErrConflict

// Config configures a JetStream key-value presence bucket.
type Config struct {
	// Bucket is the required bucket name and defines the discovery scope.
	Bucket string
	// TTL defaults to 30 seconds. It must be positive and match an existing bucket.
	TTL time.Duration
	// Create permits creation of a missing file-backed bucket with history 1.
	Create bool
}

// Store implements presence storage using a JetStream key-value bucket.
type Store struct {
	kv  nats.KeyValue
	ttl time.Duration
}

// New opens a JetStream KV presence bucket, or creates it when cfg.Create permits.
// JetStream must be enabled. The caller owns conn and must keep it available for
// store operations; Store does not close it. An existing bucket must match cfg.TTL.
func New(ctx context.Context, conn *nats.Conn, cfg Config) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, errors.New("presence nats: nil connection")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("presence nats: bucket is required")
	}
	if cfg.TTL == 0 {
		cfg.TTL = 30 * time.Second
	}
	if cfg.TTL < 0 {
		return nil, errors.New("presence nats: TTL must be positive")
	}
	js, err := conn.JetStream(nats.Context(ctx))
	if err != nil {
		return nil, fmt.Errorf("presence nats: JetStream: %w", err)
	}
	kv, err := js.KeyValue(cfg.Bucket)
	if errors.Is(err, nats.ErrBucketNotFound) && cfg.Create {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: cfg.Bucket, TTL: cfg.TTL, Storage: nats.FileStorage, History: 1})
		if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			kv, err = js.KeyValue(cfg.Bucket)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("presence nats: open bucket: %w", err)
	}
	info, err := kv.Status()
	if err != nil {
		return nil, fmt.Errorf("presence nats: bucket status: %w", err)
	}
	if info.TTL() != cfg.TTL {
		return nil, fmt.Errorf("presence nats: bucket TTL %s does not match configured TTL %s", info.TTL(), cfg.TTL)
	}
	return &Store{kv: kv, ttl: cfg.TTL}, nil
}
func hash(v string) string      { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func workerKey(w string) string { return "w_" + hash(w) }
func decode(e nats.KeyValueEntry) (presence.Record, error) {
	var r presence.Record
	err := json.Unmarshal(e.Value(), &r)
	return r, err
}

// Register claims a worker key with the complete record. Revision checks make
// ownership and refresh atomic; expired KV entries can be claimed by a new instance.
func (s *Store) Register(ctx context.Context, r presence.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	r.UpdatedAt = time.Now().UTC()
	r.ExpiresAt = r.UpdatedAt.Add(s.ttl)
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	k := workerKey(r.Identity.Worker)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		old, e := s.kv.Get(k)
		if errors.Is(e, nats.ErrKeyNotFound) {
			_, e = s.kv.Create(k, data)
		} else if e == nil {
			prior, de := decode(old)
			if de != nil {
				return fmt.Errorf("presence nats: decode existing record: %w", de)
			}
			if prior.Identity.Instance != r.Identity.Instance {
				return ErrConflict
			}
			_, e = s.kv.Update(k, data, old.Revision())
		}
		if e == nil {
			return nil
		}
		if errors.Is(e, nats.ErrKeyExists) || errors.Is(e, nats.ErrKeyNotFound) {
			continue
		}
		return fmt.Errorf("presence nats: write record: %w", e)
	}
}

// Unregister removes the record only if the current worker key still belongs to id.
func (s *Store) Unregister(ctx context.Context, id presence.Identity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k := workerKey(id.Worker)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, err := s.kv.Get(k)
		if errors.Is(err, nats.ErrKeyNotFound) {
			return presence.ErrNotFound
		}
		if err != nil {
			return err
		}
		r, err := decode(e)
		if err != nil {
			return err
		}
		if r.Identity != id {
			return presence.ErrNotFound
		}
		err = s.kv.Purge(k, nats.LastRevision(e.Revision()))
		if err == nil {
			return nil
		}
		if errors.Is(err, nats.ErrKeyNotFound) || errors.Is(err, nats.ErrKeyExists) {
			continue
		}
		return fmt.Errorf("presence nats: purge record: %w", err)
	}
}

// Discover returns live records in worker, then instance, order.
func (s *Store) Discover(ctx context.Context, f presence.Filter) ([]presence.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keys, err := s.kv.Keys(nats.Context(ctx))
	if errors.Is(err, nats.ErrNoKeysFound) {
		return []presence.Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]presence.Record, 0, len(keys))
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(k, "w_") {
			continue
		}
		e, err := s.kv.Get(k)
		if errors.Is(err, nats.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		r, err := decode(e)
		if err != nil {
			return nil, fmt.Errorf("presence nats: decode record: %w", err)
		}
		if matches(r, f) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Identity.Worker != out[j].Identity.Worker {
			return out[i].Identity.Worker < out[j].Identity.Worker
		}
		return out[i].Identity.Instance < out[j].Identity.Instance
	})
	return out, nil
}
func matches(r presence.Record, f presence.Filter) bool {
	if f.Worker != "" && r.Identity.Worker != f.Worker || f.Instance != "" && r.Identity.Instance != f.Instance || f.State != "" && r.State != f.State {
		return false
	}
	for k, v := range f.Labels {
		if r.Labels[k] != v {
			return false
		}
	}
	return true
}

// TTL returns the configured bucket lifetime.
func (s *Store) TTL() time.Duration { return s.ttl }
