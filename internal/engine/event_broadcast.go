package engine

import "sync"

// EventBroadcast is the in-process notice that events were appended. Open
// event streams subscribe to it and re-read the store when woken, so a wake
// carries no event data and the store stays the only source of events.
//
// The zero value is ready to use. Publish never blocks: each subscriber has
// a one-slot wake channel, and a publish that finds the slot full coalesces
// into the wake already pending.
type EventBroadcast struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// Subscribe registers a wake channel that every later Publish rings. cancel
// removes it; after cancel returns, no publish rings the channel again.
func (b *EventBroadcast) Subscribe() (wake <-chan struct{}, cancel func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[chan struct{}]struct{}{}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Publish wakes every current subscriber without blocking.
func (b *EventBroadcast) Publish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
