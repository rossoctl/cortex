package session

import (
	"sync"
	"sync/atomic"

	"github.com/rossoctl/cortex/core/pipeline"
)

// subscriberChanBuf caps each subscriber's channel depth. 64 absorbs short
// bursts without unbounded memory; slow consumers drop rather than blocking
// Append.
const subscriberChanBuf = 64

// subscriber wires one live consumer of session events. Writes on ch are
// non-blocking — a full channel increments drops and discards the event.
type subscriber struct {
	ch        chan pipeline.SessionEvent
	drops     uint64 // atomic; reads via LoadDrops
	closeOnce sync.Once
}

// closeCh closes ch at most once. Called by the cancel func returned from
// Subscribe and by Store.Close during shutdown; whichever runs first wins.
func (s *subscriber) closeCh() {
	s.closeOnce.Do(func() { close(s.ch) })
}

// LoadDrops returns the total events dropped for this subscriber since Subscribe.
func (s *subscriber) LoadDrops() uint64 {
	return atomic.LoadUint64(&s.drops)
}

// Subscribe registers a listener for session events written after the call.
// The returned channel is buffered (64). Events are sent non-blockingly; if
// the consumer falls behind, events are dropped — the caller can observe the
// drop count via the returned Subscription's Drops method. Always call the
// cancel func to remove the subscriber and close the channel.
func (s *Store) Subscribe() (*Subscription, func()) {
	sub := &subscriber{ch: make(chan pipeline.SessionEvent, subscriberChanBuf)}

	s.mu.Lock()
	s.subscribers = append(s.subscribers, sub)
	s.mu.Unlock()

	cancel := func() {
		s.mu.Lock()
		for i, existing := range s.subscribers {
			if existing == sub {
				s.subscribers = append(s.subscribers[:i], s.subscribers[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		sub.closeCh()
	}
	return &Subscription{sub: sub}, cancel
}

// Subscription is the consumer-facing handle returned by Store.Subscribe.
type Subscription struct {
	sub *subscriber
}

// Events returns the receive-only channel of session events.
func (s *Subscription) Events() <-chan pipeline.SessionEvent {
	return s.sub.ch
}

// Drops returns the cumulative event drops due to a full channel.
func (s *Subscription) Drops() uint64 {
	return s.sub.LoadDrops()
}

// publishLocked fans out event to every current subscriber. Must be called
// with s.mu held. Sends are non-blocking — a full channel increments the
// subscriber's drop counter and discards the event.
//
// Performance note: fan-out runs under s.mu, which serializes Append on the
// subscriber list length. This is fine for the debug API's expected load
// (O(1) live consumers: one agentop instance + maybe a curl tail). If the
// store ever needs to support many concurrent subscribers, refactor this
// to an unlocked broadcast (snapshot the slice under the lock, send outside).
func (s *Store) publishLocked(event pipeline.SessionEvent) {
	for _, sub := range s.subscribers {
		select {
		case sub.ch <- event:
		default:
			atomic.AddUint64(&sub.drops, 1)
		}
	}
}
