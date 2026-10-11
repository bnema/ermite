// SPDX-License-Identifier: AGPL-3.0-or-later

package eventbus_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/eventbus"
	"bnema.dev/ermite/internal/domain"
)

func ev(ref string) domain.Event {
	return domain.PushEvent{Updates: []domain.RefUpdate{{Ref: ref}}}
}

// recv takes the event a subscriber already has. Publish delivers into the
// buffer before it returns, so there is nothing to wait for.
func recv(t *testing.T, ch <-chan domain.Event) domain.Event {
	t.Helper()
	synctest.Wait()
	select {
	case e, ok := <-ch:
		require.True(t, ok, "channel closed")
		return e
	default:
		t.Fatal("no event")
		return nil
	}
}

func TestFanOut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
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
	})
}

func TestOnlyEventsFromSubscription(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		b := eventbus.New()
		b.Publish(ctx, ev("before"))
		ch := b.Subscribe(ctx)
		b.Publish(ctx, ev("after"))
		assert.Equal(t, ev("after"), recv(t, ch))
		synctest.Wait()
		assert.Empty(t, ch, "nothing else is buffered")
	})
}

func TestSlowSubscriberGetsGapAndDoesNotBlock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		b := eventbus.NewWithBuffer(4)
		slow, fast := b.Subscribe(ctx), b.Subscribe(ctx)

		// Publish runs in this goroutine: one that blocked on the slow
		// subscriber would deadlock the bubble and fail the test.
		for i := range 20 {
			b.Publish(ctx, ev(string(rune('a'+i))))
			// The fast subscriber keeps up.
			assert.Equal(t, ev(string(rune('a'+i))), recv(t, fast))
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
	})
}

func TestUnsubscribeOnContext(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := eventbus.New()
		ctx, cancel := context.WithCancel(context.Background())
		ch := b.Subscribe(ctx)
		require.Equal(t, 1, b.Subscribers())
		synctest.Wait()
		require.Equal(t, 1, b.Subscribers(), "still subscribed while ctx lives")
		cancel()
		synctest.Wait() // the AfterFunc of the context has run
		select {
		case _, ok := <-ch:
			assert.False(t, ok)
		default:
			t.Fatal("channel not closed")
		}
		assert.Equal(t, 0, b.Subscribers())
		b.Publish(context.Background(), ev("x")) // no panic
	})
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
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
					default: // nothing buffered yet: Publish never blocks, nor does this
					}
					runtime.Gosched()
				}
				scancel()
			}()
		}
		wg.Wait()
		cancel()
		synctest.Wait()
		assert.Zero(t, b.Subscribers(), "every subscription ended")
	})
}
