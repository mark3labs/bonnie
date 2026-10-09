package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/bonnie/presence"
)

// Watch returns a snapshot and polls Discover at min(TTL/3, one second).
// KV expiry does not emit a deletion here: polling detects disappearance and
// emits the last matching record. Discovery errors are terminal stream events.
func (s *Store) Watch(ctx context.Context, f presence.Filter) ([]presence.Record, <-chan presence.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	snapshot, err := s.Discover(ctx, f)
	if err != nil {
		return nil, nil, fmt.Errorf("presence nats: watch snapshot: %w", err)
	}
	interval := s.ttl / 3
	if interval <= 0 || interval > time.Second {
		interval = time.Second
	}
	ch := make(chan presence.Event)
	go func() {
		defer close(ch)
		known := make(map[presence.Identity]presence.Record, len(snapshot))
		for _, r := range snapshot {
			known[r.Identity] = r
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, e := s.Discover(ctx, f)
				if e != nil {
					select {
					case ch <- presence.Event{Err: e}:
					case <-ctx.Done():
					}
					return
				}
				next := make(map[presence.Identity]presence.Record, len(current))
				for _, r := range current {
					next[r.Identity] = r
					old, ok := known[r.Identity]
					if !ok || old.UpdatedAt != r.UpdatedAt {
						select {
						case ch <- presence.Event{Record: r}:
						case <-ctx.Done():
							return
						}
					}
				}
				for id, r := range known {
					if _, ok := next[id]; !ok {
						select {
						case ch <- presence.Event{Record: r, Deleted: true}:
						case <-ctx.Done():
							return
						}
					}
				}
				known = next
			}
		}
	}()
	return snapshot, ch, nil
}
