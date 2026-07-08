// Package events is lab's in-process event bus plus the SSE wire format
// (design §5). Publishers never block: every subscriber gets a small buffered
// channel and slow subscribers simply lose events — clients refetch on event,
// so a dropped event costs one refresh, never correctness.
package events

import (
	"context"
	"sync"
)

// Event is one bus message. Type is the SSE event name (brief §8.1:
// repo.changed, run.changed, parked.changed, clone.progress,
// provider.auth.changed, issue.changed, cr.changed, heartbeat); Payload is a
// small JSON envelope ({type, repoID?, …}).
type Event struct {
	Type    string
	Payload any
}

// subscriberBuffer is the per-subscriber channel capacity. Small on purpose:
// SSE clients that cannot keep up lose events rather than backing up Publish.
const subscriberBuffer = 16

// Bus fans events out to all current subscribers.
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[chan Event]struct{})}
}

// Subscribe registers a new subscriber and returns its channel plus a cancel
// function. Cancel is idempotent and closes the channel; cancellation of ctx
// cancels the subscription too. The channel is buffered (subscriberBuffer);
// events published while it is full are dropped for this subscriber only.
func (b *Bus) Subscribe(ctx context.Context) (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)

	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			// Closing under b.mu is what makes this safe: Publish sends only
			// while holding b.mu, so no send can race the close.
			close(ch)
			b.mu.Unlock()
		})
	}

	stop := context.AfterFunc(ctx, cancel)
	return ch, func() { stop(); cancel() }
}

// Publish delivers e to every subscriber that has buffer space and drops it
// for the rest. It never blocks.
func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop, never block Publish
		}
	}
}

// SubscriberCount reports the current number of subscribers (tests, metrics).
func (b *Bus) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
