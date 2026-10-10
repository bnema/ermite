// SPDX-License-Identifier: AGPL-3.0-or-later

package eventbus_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/eventbus"
	"bnema.dev/ermite/internal/domain"
)

func ev(ref string) domain.Event {
	return domain.PushEvent{Updates: []domain.RefUpdate{{Ref: ref}}}
}

func recv(t *testing.T, ch <-chan domain.Event) domain.Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		require.True(t, ok, "channel closed")
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return nil
	}
}

func TestFanOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := eventbus.New()
	a, c := b.Subscribe(ctx), b.Subscribe(ctx)

	b.Publish(ctx, ev("1"))
	b.Publish(ctx, domain.RepoCreated{})
	assert.Equal(t, ev("1"), recv(t, a))
	assert.Equal(t, ev("1"), recv(t, c))
	assert.Equal(t, "repo.created", recv(t, a).EventKind())
	assert.Equal(t, "repo.created", recv(t, c).EventKind())
}

func TestOnlyEventsFromSubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := eventbus.New()
	b.Publish(ctx, ev("before"))
	ch := b.Subscribe(ctx)
	b.Publish(ctx, ev("after"))
	assert.Equal(t, ev("after"), recv(t, ch))
}

func TestSlowSubscriberGetsGapAndDoesNotBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := eventbus.NewWithBuffer(4)
	slow, fast := b.Subscribe(ctx), b.Subscribe(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 20 {
			b.Publish(ctx, ev(string(rune('a'+i))))
			// The fast subscriber keeps up.
			recv(t, fast)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}

	// The slow one lost events: it sees a gap marker, then only recent events.
	var got []domain.Event
	for len(slow) > 0 {
		got = append(got, <-slow)
	}
	require.NotEmpty(t, got)
	var gapAt = -1
	for i, e := range got {
		if _, ok := e.(domain.EventGap); ok {
			gapAt = i
		}
	}
	assert.GreaterOrEqual(t, gapAt, 0, "a gap marker is delivered")
	assert.Equal(t, ev("t"), got[len(got)-1], "the latest event is delivered")
	assert.LessOrEqual(t, len(got), 4)
}

func TestUnsubscribeOnContext(t *testing.T) {
	b := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	ch := b.Subscribe(ctx)
	require.Equal(t, 1, b.Subscribers())
	cancel()
	select {
	case _, ok := <-ch:
		assert.False(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed")
	}
	assert.Equal(t, 0, b.Subscribers())
	b.Publish(context.Background(), ev("x")) // no panic
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	b := eventbus.NewWithBuffer(8)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 200 {
				b.Publish(ctx, ev("x"))
			}
		}()
		go func() {
			defer wg.Done()
			sctx, scancel := context.WithCancel(ctx)
			ch := b.Subscribe(sctx)
			for range 5 {
				select {
				case <-ch:
				case <-time.After(10 * time.Millisecond):
				}
			}
			scancel()
		}()
	}
	wg.Wait()
	cancel()
}
