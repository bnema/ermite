// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

// eventRig is an Events use case fed by a channel the test writes to.
type eventRig struct {
	t      *testing.T
	meta   *mocks.RepoMetaStore
	git    *mocks.GitRepos
	feed   chan domain.Event
	events *app.Events
}

func newEventRig(t *testing.T, size int) *eventRig {
	t.Helper()
	r := &eventRig{
		t:    t,
		meta: mocks.NewRepoMetaStore(t),
		git:  mocks.NewGitRepos(t),
		feed: make(chan domain.Event, 4096),
	}
	bus := mocks.NewEventBus(t)
	bus.EXPECT().Subscribe(mock.Anything).RunAndReturn(func(ctx context.Context) <-chan domain.Event {
		go func() { <-ctx.Done(); close(r.feed) }()
		return r.feed
	})
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	r.git.EXPECT().HeadBranch(mock.Anything, mock.Anything).Return("main", nil).Maybe()
	repos := app.NewRepos(r.meta, r.git, bus, clock)
	r.events = app.NewEvents(context.Background(), repos, bus, size, clock)
	t.Cleanup(func() { r.events.Close(context.Background()) })
	return r
}

// repo declares a repository the rig knows.
func (r *eventRig) repo(s string, vis domain.Visibility) domain.RepoName {
	n := name(r.t, s)
	r.meta.EXPECT().Get(mock.Anything, n).Return(domain.Repo{Name: n, Visibility: vis}, nil).Maybe()
	return n
}

func (r *eventRig) push(n domain.RepoName, i int) {
	oid := fmt.Sprintf("%040x", i+1)
	r.feed <- domain.PushEvent{Repo: n, Updates: []domain.RefUpdate{{Old: domain.ZeroOID, New: oid, Ref: "refs/heads/main"}}, At: now}
}

// watcher is a running Watch.
type watcher struct {
	got  chan app.WatchEvent
	done chan error
}

func (r *eventRig) watch(ctx context.Context, p domain.Principal, params app.WatchParams) *watcher {
	w := &watcher{got: make(chan app.WatchEvent, 4096), done: make(chan error, 1)}
	go func() {
		w.done <- r.events.Watch(ctx, p, params, func(e app.WatchEvent) error {
			w.got <- e
			return nil
		})
	}()
	return w
}

func (w *watcher) next(t *testing.T) app.WatchEvent {
	t.Helper()
	select {
	case e := <-w.got:
		return e
	case err := <-w.done:
		t.Fatalf("watch ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return app.WatchEvent{}
}

func (w *watcher) none(t *testing.T) {
	t.Helper()
	select {
	case e := <-w.got:
		t.Fatalf("unexpected event %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatchLiveAndFiltered(t *testing.T) {
	r := newEventRig(t, 0)
	pub := r.repo("pub", domain.VisibilityPublic)
	priv := r.repo("priv", domain.VisibilityPrivate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	anon := r.watch(ctx, domain.Anonymous(), app.WatchParams{})
	root := r.watch(ctx, domain.Root(), app.WatchParams{Topics: []string{"home"}})
	scoped := r.watch(ctx, tokenPrincipal(domain.ScopeRepoRead, "other"), app.WatchParams{})
	onlyPriv := r.watch(ctx, domain.Root(), app.WatchParams{Topics: []string{"repo:priv"}})
	require.Eventually(t, func() bool { return r.events.Watchers() == 4 }, 5*time.Second, time.Millisecond)

	r.push(priv, 1)
	r.push(pub, 2)

	// Anonymous and a token limited to another repository never see priv.
	for name, w := range map[string]*watcher{"anonymous": anon, "scoped": scoped} {
		e := w.next(t)
		assert.Equal(t, pub, e.Event.(domain.PushEvent).Repo, name)
		w.none(t)
	}
	first, second := root.next(t), root.next(t)
	assert.Equal(t, priv, first.Event.(domain.PushEvent).Repo)
	assert.Equal(t, pub, second.Event.(domain.PushEvent).Repo)
	assert.NotEmpty(t, first.ID)
	assert.NotEqual(t, first.ID, second.ID)

	assert.Equal(t, priv, onlyPriv.next(t).Event.(domain.PushEvent).Repo)
	onlyPriv.none(t)
}

func TestWatchRechecksAccessPerEvent(t *testing.T) {
	r := newEventRig(t, 0)
	n := name(t, "flip")
	vis := domain.VisibilityPublic
	var mu sync.Mutex
	r.meta.EXPECT().Get(mock.Anything, n).RunAndReturn(func(context.Context, domain.RepoName) (domain.Repo, error) {
		mu.Lock()
		defer mu.Unlock()
		return domain.Repo{Name: n, Visibility: vis}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := r.watch(ctx, domain.Anonymous(), app.WatchParams{})
	require.Eventually(t, func() bool { return r.events.Watchers() == 1 }, 5*time.Second, time.Millisecond)

	r.push(n, 1)
	w.next(t)

	mu.Lock()
	vis = domain.VisibilityPrivate
	mu.Unlock()
	r.push(n, 2)
	w.none(t)
}

func TestWatchResumeAndGap(t *testing.T) {
	const size = 8
	r := newEventRig(t, size)
	n := r.repo("pub", domain.VisibilityPublic)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Record the ids of 20 events through a first watcher.
	first := r.watch(ctx, domain.Root(), app.WatchParams{})
	require.Eventually(t, func() bool { return r.events.Watchers() == 1 }, 5*time.Second, time.Millisecond)
	// One at a time: this watcher must not fall behind the small ring.
	var ids []string
	for i := range 20 {
		r.push(n, i)
		ids = append(ids, first.next(t).ID)
	}
	require.Len(t, ids, 20)

	t.Run("resume replays the buffered events after the id, then goes live", func(t *testing.T) {
		w := r.watch(ctx, domain.Root(), app.WatchParams{ResumeAfter: ids[16]})
		for _, want := range ids[17:] {
			e := w.next(t)
			assert.False(t, e.Gap)
			assert.Equal(t, want, e.ID)
		}
		w.none(t)
	})

	t.Run("resume at the last event replays nothing", func(t *testing.T) {
		w := r.watch(ctx, domain.Root(), app.WatchParams{ResumeAfter: ids[19]})
		w.none(t)
	})

	t.Run("an id that fell out of the buffer gives a gap, then the oldest buffered event", func(t *testing.T) {
		// 20 events were taken, the buffer holds the last 8: ids[12] is the
		// oldest still present; ids[0] is long gone.
		w := r.watch(ctx, domain.Root(), app.WatchParams{ResumeAfter: ids[0]})
		gap := w.next(t)
		assert.True(t, gap.Gap)
		assert.Empty(t, gap.ID)
		oldest := w.next(t)
		assert.False(t, oldest.Gap)
		assert.Equal(t, ids[12], oldest.ID)
	})

	t.Run("an unknown id gives a gap", func(t *testing.T) {
		for _, id := range []string{"nonsense", "zz-1", "-", "0-0"} {
			w := r.watch(ctx, domain.Root(), app.WatchParams{ResumeAfter: id})
			assert.True(t, w.next(t).Gap, id)
		}
	})

	t.Run("an id of a later run is a gap, not a newer event", func(t *testing.T) {
		w := r.watch(ctx, domain.Root(), app.WatchParams{ResumeAfter: "otherboot-999"})
		assert.True(t, w.next(t).Gap)
	})

	t.Run("a resumed watch goes live after the replay", func(t *testing.T) {
		w := r.watch(ctx, domain.Root(), app.WatchParams{ResumeAfter: ids[19]})
		w.none(t)
		r.push(n, 99)
		e := w.next(t)
		assert.False(t, e.Gap)
		assert.NotEmpty(t, e.ID)
	})
}

func TestWatchTopicErrors(t *testing.T) {
	r := newEventRig(t, 0)
	r.repo("pub", domain.VisibilityPublic)
	r.repo("priv", domain.VisibilityPrivate)
	r.meta.EXPECT().Get(mock.Anything, name(t, "gone")).Return(domain.Repo{}, domain.ErrNotFound).Maybe()
	noop := func(app.WatchEvent) error { return nil }
	ctx := context.Background()

	for topic, want := range map[string]error{
		"nope":         domain.ErrInvalid,
		"repo:Bad":     domain.ErrInvalidName,
		"repo:":        domain.ErrInvalidName,
		"repo:gone":    domain.ErrNotFound,
		"repo:priv":    domain.ErrNotFound, // hidden from anonymous
		"repo:pub:foo": domain.ErrInvalidName,
	} {
		err := r.events.Watch(ctx, domain.Anonymous(), app.WatchParams{Topics: []string{topic}}, noop)
		assert.ErrorIs(t, err, want, topic)
	}
}

func TestWatchEndsCleanly(t *testing.T) {
	t.Run("on context cancellation", func(t *testing.T) {
		r := newEventRig(t, 0)
		ctx, cancel := context.WithCancel(context.Background())
		w := r.watch(ctx, domain.Root(), app.WatchParams{})
		require.Eventually(t, func() bool { return r.events.Watchers() == 1 }, 5*time.Second, time.Millisecond)
		cancel()
		select {
		case err := <-w.done:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("watch did not end")
		}
		assert.Zero(t, r.events.Watchers())
	})

	t.Run("on Close", func(t *testing.T) {
		r := newEventRig(t, 0)
		w := r.watch(context.Background(), domain.Root(), app.WatchParams{})
		require.Eventually(t, func() bool { return r.events.Watchers() == 1 }, 5*time.Second, time.Millisecond)
		r.events.Close(context.Background())
		r.events.Close(context.Background()) // twice is fine
		select {
		case err := <-w.done:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("watch did not end")
		}
	})

	t.Run("a failing send ends the watch with its error", func(t *testing.T) {
		r := newEventRig(t, 0)
		n := r.repo("pub", domain.VisibilityPublic)
		boom := errors.New("client went away")
		done := make(chan error, 1)
		go func() {
			done <- r.events.Watch(context.Background(), domain.Root(), app.WatchParams{}, func(app.WatchEvent) error { return boom })
		}()
		require.Eventually(t, func() bool { return r.events.Watchers() == 1 }, 5*time.Second, time.Millisecond)
		r.push(n, 1)
		assert.ErrorIs(t, <-done, boom)
	})
}

func TestWatchSlowWatcherGetsAGap(t *testing.T) {
	const size = 4
	r := newEventRig(t, size)
	n := r.repo("pub", domain.VisibilityPublic)

	release := make(chan struct{})
	var mu sync.Mutex
	var got []app.WatchEvent
	done := make(chan error, 1)
	blocked := make(chan struct{})
	first := true
	go func() {
		done <- r.events.Watch(context.Background(), domain.Root(), app.WatchParams{}, func(e app.WatchEvent) error {
			if first { // block on the first event while the ring turns over
				first = false
				close(blocked)
				<-release
			}
			mu.Lock()
			got = append(got, e)
			mu.Unlock()
			return nil
		})
	}()
	require.Eventually(t, func() bool { return r.events.Watchers() == 1 }, 5*time.Second, time.Millisecond)
	r.push(n, 0)
	<-blocked
	for i := 1; i < 30; i++ {
		r.push(n, i)
	}
	// Wait for the use case to have taken them all (it does not block).
	time.Sleep(100 * time.Millisecond)
	close(release)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 2
	}, 5*time.Second, time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	hasGap := false
	for _, e := range got[1:] {
		hasGap = hasGap || e.Gap
	}
	assert.True(t, hasGap, "events were lost while the watcher was blocked: %+v", got)
}

func TestEventsRepoPayloads(t *testing.T) {
	r := newEventRig(t, 0)
	pub := r.repo("pub", domain.VisibilityPublic)
	priv := r.repo("priv", domain.VisibilityPrivate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	anon := r.watch(ctx, domain.Anonymous(), app.WatchParams{})
	root := r.watch(ctx, domain.Root(), app.WatchParams{})
	require.Eventually(t, func() bool { return r.events.Watchers() == 2 }, 5*time.Second, time.Millisecond)

	r.feed <- domain.RepoCreated{Repo: pub, Visibility: domain.VisibilityPublic, At: now}
	r.feed <- domain.RepoUpdated{Repo: pub, At: now}
	// A deleted repository cannot be read any more: its visibility decides.
	r.feed <- domain.RepoDeleted{Repo: priv, Visibility: domain.VisibilityPrivate, At: now}
	r.feed <- domain.RepoDeleted{Repo: pub, Visibility: domain.VisibilityPublic, At: now}

	for _, w := range []*watcher{anon, root} {
		created := w.next(t)
		require.NotNil(t, created.Repo)
		assert.Equal(t, "main", created.Repo.DefaultBranch)
		assert.Equal(t, pub, created.Repo.Name)
		require.NotNil(t, w.next(t).Repo)
	}
	assert.Equal(t, priv, root.next(t).Event.(domain.RepoDeleted).Repo)
	assert.Equal(t, pub, root.next(t).Event.(domain.RepoDeleted).Repo)
	assert.Equal(t, pub, anon.next(t).Event.(domain.RepoDeleted).Repo, "the private deletion was skipped")
	anon.none(t)
}
