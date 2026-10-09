package presence

import (
	"context"
	"time"
)

// Watch returns a matching snapshot followed by polled changes. The stream closes
// when ctx is canceled; deletion events carry the last matching record.
func (s *MemoryStore) Watch(ctx context.Context, filter Filter) ([]Record, <-chan Event, error) {
	snapshot, err := s.Discover(ctx, filter)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan Event)
	go func() {
		defer close(ch)
		known := make(map[Identity]Record, len(snapshot))
		for _, r := range snapshot {
			known[r.Identity] = r
		}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, e := s.Discover(ctx, filter)
				if e != nil {
					return
				}
				next := make(map[Identity]Record, len(current))
				for _, r := range current {
					next[r.Identity] = r
					old, ok := known[r.Identity]
					if !ok || old.UpdatedAt != r.UpdatedAt {
						select {
						case ch <- Event{Record: r}:
						case <-ctx.Done():
							return
						}
					}
				}
				for id, r := range known {
					if _, ok := next[id]; !ok {
						select {
						case ch <- Event{Record: r, Deleted: true}:
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
