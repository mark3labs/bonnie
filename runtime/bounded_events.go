package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// ErrEventOverflow terminates an observer, never the run being observed.
var ErrEventOverflow = errors.New("bonnie: event observer exceeded its buffer budget")

// SubscribeBounded is the opt-in bounded counterpart of Subscribe. Errors are
// reported before output closes; stop is idempotent. Budgets include the event
// currently waiting for delivery, not only the pending queue.
func (b *EventBus) SubscribeBounded(runID string, after, maxEvents, maxBytes int) (<-chan Event, <-chan error, func()) {
	b.mu.Lock()
	s := newBoundedSubscriber(maxEvents, maxBytes)
	for _, ev := range b.backlog[runID] {
		if ev.Seq > after {
			s.push(ev)
		}
	}
	b.nextSub++
	id := b.nextSub
	if b.subs[runID] == nil {
		b.subs[runID] = make(map[int]*subscriber)
	}
	b.subs[runID][id] = s
	b.mu.Unlock()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs[runID], id)
			if len(b.subs[runID]) == 0 {
				delete(b.subs, runID)
			}
			b.mu.Unlock()
			s.close()
		})
	}
	go func() { <-s.done; stop() }()
	return s.out, s.failure, stop
}
func newBoundedSubscriber(maxEvents, maxBytes int) *subscriber {
	s := &subscriber{out: make(chan Event), wake: make(chan struct{}, 1), done: make(chan struct{}), failure: make(chan error, 1), maxEvents: maxEvents, maxBytes: maxBytes}
	go s.boundedPump()
	return s
}
func (s *subscriber) boundedPump() {
	defer close(s.out)
	for {
		s.mu.Lock()
		if s.closed {
			s.queue = nil
			s.mu.Unlock()
			return
		}
		if len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-s.done:
				return
			}
		}
		ev := s.queue[0]
		s.mu.Unlock()
		select {
		case s.out <- ev:
		case <-s.done:
			return
		}
		encoded, _ := json.Marshal(ev)
		s.mu.Lock()
		s.queue[0] = Event{}
		s.queue = s.queue[1:]
		s.queuedBytes -= len(encoded)
		s.mu.Unlock()
	}
}

// StreamEventsBounded provides the same journal/live handoff as StreamEvents,
// but bounds live buffering and surfaces replay/overflow errors. Cancellation
// reaches journal reads as well as forwarding. The journal Replay interface
// returns a full record slice; this does not claim bounded journal storage.
func (r *Runner) StreamEventsBounded(ctx context.Context, runID string, after, maxEvents, maxBytes int) (<-chan Event, <-chan error, func()) {
	ctx, cancel := context.WithCancel(ctx)
	out := make(chan Event)
	failures := make(chan error, 1)
	live, liveErrors, unsubscribe := r.bus.SubscribeBounded(runID, after, maxEvents, maxBytes)
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); unsubscribe() }) }
	position, oldest := r.durableSeq(runID), r.bus.Oldest(runID)
	needReplay := position > after && (oldest == 0 || after+1 < oldest)
	go func() {
		defer close(out)
		defer stop()
		report := func(err error) {
			if err != nil && !errors.Is(err, errStreamClosed) && !errors.Is(err, context.Canceled) {
				select {
				case failures <- err:
				default:
				}
			}
		}
		emitted := after
		if needReplay {
			replay := make(chan Event)
			replayDone := make(chan error, 1)
			replayCtx, replayCancel := context.WithCancel(ctx)
			go func() {
				replayDone <- r.replayEventsContext(replayCtx, runID, after, replay, replayCtx.Done(), &emitted)
				close(replay)
			}()
			var err error
		replayLoop:
			for {
				select {
				case <-ctx.Done():
					break replayLoop
				case err = <-liveErrors:
					break replayLoop
				case ev, ok := <-replay:
					if !ok {
						break replayLoop
					}
					raw, _ := json.Marshal(ev)
					if len(raw) > maxBytes {
						err = ErrEventOverflow
						break replayLoop
					}
					select {
					case out <- ev:
					case err = <-liveErrors:
						break replayLoop
					case <-ctx.Done():
						break replayLoop
					}
				}
			}
			replayCancel()
			replayErr := <-replayDone
			if err != nil {
				report(err)
				return
			}
			if replayErr != nil {
				report(replayErr)
				return
			}
			if ctx.Err() != nil {
				return
			}
		}
		watermark := emitted
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-liveErrors:
				report(err)
				return
			case ev, ok := <-live:
				if !ok {
					select {
					case err := <-liveErrors:
						report(err)
					default:
					}
					return
				}
				if needReplay && ev.Seq <= watermark {
					continue
				}
				select {
				case out <- ev:
				case err := <-liveErrors:
					report(err)
					return
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, failures, stop
}
