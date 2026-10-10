// SPDX-License-Identifier: AGPL-3.0-or-later

// Package eventbus implements ports.EventBus: in-process fan-out to
// subscribers, never blocking the publisher.
package eventbus

import (
	"context"
	"sync"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// DefaultBuffer is the number of events a subscriber may lag behind.
const DefaultBuffer = 64

var _ ports.EventBus = (*Bus)(nil)

// Bus is the in-process event bus.
type Bus struct {
	buffer int

	mu   sync.Mutex
	subs map[chan domain.Event]struct{}
}

// New builds a bus whose subscribers buffer DefaultBuffer events.
func New() *Bus { return NewWithBuffer(DefaultBuffer) }

// NewWithBuffer builds a bus whose subscribers buffer n events (at least 2,
// so that a gap marker and the event that follows it fit).
func NewWithBuffer(n int) *Bus {
	return &Bus{buffer: max(n, 2), subs: make(map[chan domain.Event]struct{})}
}

// Publish delivers ev to every subscriber. A subscriber whose buffer is full
// is cleared and gets a domain.EventGap followed by ev: it lost events and
// must refresh its view, but it is never blocked on and never blocks others.
func (b *Bus) Publish(_ context.Context, ev domain.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
			continue
		default:
		}
		// Full. Only Publish sends and it holds b.mu, so after draining
		// there is room for the marker and the event.
	drain:
		for {
			select {
			case <-ch:
			default:
				break drain
			}
		}
		ch <- domain.EventGap{}
		ch <- ev
	}
}

// Subscribe registers a subscriber until ctx ends, then closes its channel.
func (b *Bus) Subscribe(ctx context.Context) <-chan domain.Event {
	ch := make(chan domain.Event, b.buffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	context.AfterFunc(ctx, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		close(ch)
		b.mu.Unlock()
	})
	return ch
}
