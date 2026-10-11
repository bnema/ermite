// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	appmocks "bnema.dev/ermite/internal/app/mocks"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

// eventRig is an Events use case fed by a channel the test writes to.
type eventRig struct {
	t             *testing.T
	meta          *mocks.RepoMetaStore
	git           *mocks.GitRepos
	feed          chan domain.Event
	auth          *appmocks.Revalidator
	revoked       sync.Map     // token IDs revoked by the test
	revalidations atomic.Int32 // calls to auth.Revalidate
	events        *app.Events
}

func newEventRig(t *testing.T, size int) *eventRig {
	t.Helper()
	return newEventRigConfig(t, app.EventsConfig{Size: size})
}

func newEventRigConfig(t *testing.T, cfg app.EventsConfig) *eventRig {
	t.Helper()
	r := &eventRig{
		t:    t,
		meta: mocks.NewRepoMetaStore(t),
		git:  mocks.NewGitRepos(t),
		feed: make(chan domain.Event, 4096),
		auth: appmocks.NewRevalidator(t),
	}
	// Tokens are valid until the test revokes them.
	r.auth.EXPECT().Revalidate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p domain.Principal) error {
		r.revalidations.Add(1)
		if _, gone := r.revoked.Load(p.TokenID()); gone {
			return domain.ErrUnauthenticated
		}
		return nil
	}).Maybe()
	cfg.Auth = r.auth
	bus := mocks.NewEventBus(t)
	bus.EXPECT().Subscribe(mock.Anything).RunAndReturn(func(ctx context.Context) <-chan domain.Event {
		go func() { <-ctx.Done(); close(r.feed) }()
		return r.feed
	})
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	r.git.EXPECT().HeadBranch(mock.Anything, mock.Anything).Return("main", nil).Maybe()
	repos := app.NewRepos(r.meta, r.git, bus, clock)
	r.events = app.NewEvents(context.Background(), repos, bus, clock, cfg)
	t.Cleanup(func() { r.events.Close(context.Background()) })
	return r
}

// repo declares a repository the rig knows.
func (r *eventRig) repo(s string, vis domain.Visibility) domain.RepoName {
	n := name(r.t, s)
	r.meta.EXPECT().Get(mock.Anything, n).Return(domain.Repo{Name: n, Visibility: vis, CreatedAt: now}, nil).Maybe()
	return n
}

// pushEvent builds the PushEvent the Pushes use case would publish now: it
// carries the repository as the store answers at this moment.
func (r *eventRig) pushEvent(n domain.RepoName, oid, ref string) domain.PushEvent {
	repo, err := r.meta.Get(context.Background(), n)
	require.NoError(r.t, err)
	return domain.PushEvent{
		Repo: n, Updates: []domain.RefUpdate{{Old: domain.ZeroOID, New: oid, Ref: ref}}, At: now,
		Visibility: repo.Visibility, CreatedAt: repo.CreatedAt,
	}
}

func (r *eventRig) push(n domain.RepoName, i int) {
	r.feed <- r.pushEvent(n, fmt.Sprintf("%040x", i+1), "refs/heads/main")
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
	synctest.Wait() // everything the event can reach has run
	select {
	case e := <-w.got:
		return e
	case err := <-w.done:
		t.Fatalf("watch ended: %v", err)
	default:
		t.Fatal("no event")
	}
	return app.WatchEvent{}
}

// sentinelOID marks the push events that only prove the ones before them were
// handled.
var sentinelOID = fmt.Sprintf("%040x", 0xfeed)

// quiet proves that the watcher got nothing since its last read: it pushes an
// event on n, which the watcher may see, and requires it to be the next one.
// Events reach a watcher in order, so anything sent before would show up first.
func (w *watcher) quiet(t *testing.T, r *eventRig, n domain.RepoName) {
	t.Helper()
	r.feed <- r.pushEvent(n, sentinelOID, "refs/heads/sentinel")
	e := w.next(t)
	push, ok := e.Event.(domain.PushEvent)
	if !ok || len(push.Updates) != 1 || push.Updates[0].New != sentinelOID {
		t.Fatalf("expected nothing before the sentinel, got %+v", e)
	}
}

func TestWatchLiveAndFiltered(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 0)
		pub := r.repo("pub", domain.VisibilityPublic)
		priv := r.repo("priv", domain.VisibilityPrivate)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		anon := r.watch(ctx, domain.Anonymous(), app.WatchParams{})
		root := r.watch(ctx, domain.Root(), app.WatchParams{Topics: []string{"home"}})
		scoped := r.watch(ctx, tokenPrincipal(domain.ScopeRepoRead, "other"), app.WatchParams{})
		onlyPriv := r.watch(ctx, domain.Root(), app.WatchParams{Topics: []string{"repo:priv"}})
		synctest.Wait()
		require.Equal(t, 4, r.events.Watchers())

		r.push(priv, 1)
		r.push(pub, 2)

		// Anonymous and a token limited to another repository never see priv.
		for name, w := range map[string]*watcher{"anonymous": anon, "scoped": scoped} {
			e := w.next(t)
			assert.Equal(t, pub, e.Event.(domain.PushEvent).Repo, name)
			w.quiet(t, r, pub)
		}
		first, second := root.next(t), root.next(t)
		assert.Equal(t, priv, first.Event.(domain.PushEvent).Repo)
		assert.Equal(t, pub, second.Event.(domain.PushEvent).Repo)
		assert.NotEmpty(t, first.ID)
		assert.NotEqual(t, first.ID, second.ID)

		assert.Equal(t, priv, onlyPriv.next(t).Event.(domain.PushEvent).Repo)
		onlyPriv.quiet(t, r, priv)
	})
}

func TestWatchRechecksAccessPerEvent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 0)
		n := name(t, "flip")
		vis := domain.VisibilityPublic
		var mu sync.Mutex
		r.meta.EXPECT().Get(mock.Anything, n).RunAndReturn(func(context.Context, domain.RepoName) (domain.Repo, error) {
			mu.Lock()
			defer mu.Unlock()
			return domain.Repo{Name: n, Visibility: vis, CreatedAt: now}, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := r.watch(ctx, domain.Anonymous(), app.WatchParams{})
		synctest.Wait()
		require.Equal(t, 1, r.events.Watchers())

		r.push(n, 1)
		w.next(t)

		mu.Lock()
		vis = domain.VisibilityPrivate
		mu.Unlock()
		r.push(n, 2)
		w.quiet(t, r, r.repo("open", domain.VisibilityPublic)) // push 2 is not delivered to anonymous
	})
}

func TestWatchResumeAndGap(t *testing.T) {
	t.Parallel()
	const size = 8
	// Each subtest gets its own bubble and a fresh run of 20 events recorded
	// through a first watcher; the ring then holds the last 8.
	setup := func(t *testing.T) (r *eventRig, n domain.RepoName, ctx context.Context, ids []string) {
		r = newEventRig(t, size)
		n = r.repo("pub", domain.VisibilityPublic)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		first := r.watch(ctx, domain.Root(), app.WatchParams{})
		synctest.Wait()
		require.Equal(t, 1, r.events.Watchers())
		// One at a time: this watcher must not fall behind the small ring.
		for i := range 20 {
			r.push(n, i)
			ids = append(ids, first.next(t).ID)
		}
		require.Len(t, ids, 20)
		return r, n, ctx, ids
	}

	t.Run("an id that fell out of the buffer gives a gap, then the oldest buffered event", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r, _, ctx, ids := setup(t)
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
	})

	t.Run("an unknown id gives a gap", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r, _, ctx, _ := setup(t)
			for _, id := range []string{"nonsense", "zz-1", "-", "0-0"} {
				w := r.watch(ctx, domain.Root(), app.WatchParams{ResumeAfter: id})
				assert.True(t, w.next(t).Gap, id)
			}
		})
	})

	t.Run("an id of a later run is a gap, not a newer event", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r, _, ctx, _ := setup(t)
			w := r.watch(ctx, domain.Root(), app.WatchParams{ResumeAfter: "otherboot-999"})
			assert.True(t, w.next(t).Gap)
		})
	})

	t.Run("resume replays the buffered events after the id, then goes live", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r, n, ctx, ids := setup(t)
			w := r.watch(ctx, domain.Root(), app.WatchParams{ResumeAfter: ids[16]})
			for _, want := range ids[17:] {
				e := w.next(t)
				assert.False(t, e.Gap)
				assert.Equal(t, want, e.ID)
			}
			// Nothing else was replayed: the next event is the one pushed now.
			r.push(n, 98)
			live := w.next(t)
			assert.False(t, live.Gap)
			assert.NotContains(t, ids, live.ID)
		})
	})

	t.Run("resume at the last event replays nothing, then goes live", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r, n, ctx, ids := setup(t)
			w := r.watch(ctx, domain.Root(), app.WatchParams{ResumeAfter: ids[len(ids)-1]})
			r.push(n, 99)
			e := w.next(t)
			assert.False(t, e.Gap)
			assert.NotContains(t, ids, e.ID, "the first event is the new one, nothing was replayed")
		})
	})
}

func TestWatchTopicErrors(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	t.Run("on context cancellation", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newEventRig(t, 0)
			ctx, cancel := context.WithCancel(context.Background())
			w := r.watch(ctx, domain.Root(), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, 1, r.events.Watchers())
			cancel()
			synctest.Wait()
			select {
			case err := <-w.done:
				assert.NoError(t, err)
			default:
				t.Fatal("watch did not end")
			}
			assert.Zero(t, r.events.Watchers())
		})
	})

	t.Run("on Close", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newEventRig(t, 0)
			w := r.watch(context.Background(), domain.Root(), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, 1, r.events.Watchers())
			r.events.Close(context.Background())
			r.events.Close(context.Background()) // twice is fine
			synctest.Wait()
			select {
			case err := <-w.done:
				assert.NoError(t, err)
			default:
				t.Fatal("watch did not end")
			}
		})
	})

	t.Run("a failing send ends the watch with its error", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newEventRig(t, 0)
			n := r.repo("pub", domain.VisibilityPublic)
			boom := errors.New("client went away")
			done := make(chan error, 1)
			go func() {
				done <- r.events.Watch(context.Background(), domain.Root(), app.WatchParams{}, func(app.WatchEvent) error { return boom })
			}()
			synctest.Wait()
			require.Equal(t, 1, r.events.Watchers())
			r.push(n, 1)
			assert.ErrorIs(t, <-done, boom)
		})
	})
}

func TestWatchSlowWatcherGetsAGap(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
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
		// A second, fast watcher tells when the use case has taken every event:
		// it receives them in order.
		fast := r.watch(context.Background(), domain.Root(), app.WatchParams{})
		synctest.Wait()
		require.Equal(t, 2, r.events.Watchers())
		r.push(n, 0)
		<-blocked
		for i := 1; i < 30; i++ {
			r.push(n, i)
		}
		// The ring holds four events: the fast watcher may get a gap, but it ends
		// on the last push, which proves the use case took all of them.
		last := fmt.Sprintf("%040x", 29+1)
		for {
			e := fast.next(t)
			if p, ok := e.Event.(domain.PushEvent); ok && p.Updates[0].New == last {
				break
			}
		}
		close(release)

		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		require.GreaterOrEqual(t, len(got), 2)
		hasGap := false
		for _, e := range got[1:] {
			hasGap = hasGap || e.Gap
		}
		assert.True(t, hasGap, "events were lost while the watcher was blocked: %+v", got)
	})
}

func TestEventsRepoPayloads(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 0)
		pub := r.repo("pub", domain.VisibilityPublic)
		priv := r.repo("priv", domain.VisibilityPrivate)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		anon := r.watch(ctx, domain.Anonymous(), app.WatchParams{})
		root := r.watch(ctx, domain.Root(), app.WatchParams{})
		synctest.Wait()
		require.Equal(t, 2, r.events.Watchers())

		r.feed <- domain.RepoCreated{Repo: pub, Visibility: domain.VisibilityPublic, At: now}
		r.feed <- domain.RepoUpdated{Repo: pub, At: now, Visibility: domain.VisibilityPublic, Previous: domain.VisibilityPublic, CreatedAt: now}
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
		anon.quiet(t, r, pub)
	})
}

// tokenWith builds the principal of a stored token with an id and an expiry.
func tokenWith(id string, expires time.Time, repos ...string) domain.Principal {
	tok := domain.Token{ID: id, Scopes: []domain.Scope{domain.ScopeRepoRead}, ExpiresAt: expires}
	for _, r := range repos {
		n, err := domain.ParseRepoName(r)
		if err != nil {
			panic(err)
		}
		tok.Repos = append(tok.Repos, n)
	}
	return domain.TokenPrincipal(tok)
}

func TestWatchEndsWhenTheTokenIsRevoked(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// Asking at every turn keeps the test independent of the clock.
		r := newEventRigConfig(t, app.EventsConfig{RevalidateEvery: -1})
		pub := r.repo("pub", domain.VisibilityPublic)
		priv := r.repo("priv", domain.VisibilityPrivate)
		p := tokenWith("tok", time.Time{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		w := r.watch(ctx, p, app.WatchParams{})
		synctest.Wait()
		require.Equal(t, 1, r.events.Watchers())
		r.push(pub, 1)
		w.next(t)

		r.revoked.Store("tok", true)
		r.push(priv, 2) // would be delivered to a token that may read priv
		synctest.Wait()
		select {
		case err := <-w.done:
			assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		default:
			t.Fatal("the watch of a revoked token did not end")
		}
		select {
		case e := <-w.got:
			t.Fatalf("an event was sent after the revocation: %+v", e)
		default:
		}
	})
}

func TestWatchEndsWhenTheTokenExpires(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 0)
		pub := r.repo("pub", domain.VisibilityPublic)
		// The rig's clock stands at now: this token expired at now.
		p := tokenWith("tok", now)
		w := r.watch(context.Background(), p, app.WatchParams{})
		r.push(pub, 1) // whether or not the watch had started, it ends unauthenticated
		synctest.Wait()
		select {
		case err := <-w.done:
			assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		default:
			t.Fatal("the watch of an expired token did not end")
		}
		assert.Empty(t, w.got)
	})
}

func TestWatchRevalidatesIdleWatchersAndCachesTheAnswer(t *testing.T) {
	t.Parallel()
	t.Run("a quiet forge still notices a revocation", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newEventRigConfig(t, app.EventsConfig{}) // the production default interval
			w := r.watch(context.Background(), tokenWith("tok", time.Time{}), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, 1, r.events.Watchers())
			r.revoked.Store("tok", true)
			synctest.Sleep(app.DefaultRevalidateEvery - time.Nanosecond)
			select {
			case err := <-w.done:
				t.Fatalf("the watch ended before the interval: %v", err)
			default:
			}
			assert.Zero(t, r.revalidations.Load(), "no store lookup before the interval")
			synctest.Sleep(time.Nanosecond)
			select {
			case err := <-w.done:
				assert.ErrorIs(t, err, domain.ErrUnauthenticated)
			default:
				t.Fatal("an idle watch of a revoked token did not end at the interval")
			}
			assert.Equal(t, int32(1), r.revalidations.Load())
		})
	})

	t.Run("the answer is kept between checks", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newEventRigConfig(t, app.EventsConfig{RevalidateEvery: time.Hour})
			pub := r.repo("pub", domain.VisibilityPublic)
			w := r.watch(context.Background(), tokenWith("tok", time.Time{}), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, 1, r.events.Watchers())
			for i := range 20 {
				r.push(pub, i)
			}
			for range 20 {
				w.next(t)
			}
			// Idle, the watcher wakes up every few seconds but keeps its answer
			// until the interval is over.
			synctest.Sleep(time.Hour - time.Nanosecond)
			assert.Zero(t, r.revalidations.Load(), "no store lookup within the interval")
			synctest.Sleep(time.Nanosecond)
			assert.Equal(t, int32(1), r.revalidations.Load(), "one lookup when the interval is over")
		})
	})

	t.Run("root and anonymous never ask", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newEventRigConfig(t, app.EventsConfig{RevalidateEvery: -1})
			pub := r.repo("pub", domain.VisibilityPublic)
			a := r.watch(context.Background(), domain.Anonymous(), app.WatchParams{})
			b := r.watch(context.Background(), domain.Root(), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, 2, r.events.Watchers())
			r.push(pub, 1)
			a.next(t)
			b.next(t)
			r.auth.AssertNotCalled(t, "Revalidate", mock.Anything, mock.Anything)
		})
	})
}

func TestWatchLimits(t *testing.T) {
	t.Parallel()
	noop := func(app.WatchEvent) error { return nil }

	t.Run("too many topics", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newEventRig(t, 0)
			r.repo("pub", domain.VisibilityPublic)
			ok := make([]string, app.MaxWatchTopics)
			for i := range ok {
				ok[i] = "home"
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // returns at once once the topics are accepted
			assert.NoError(t, r.events.Watch(ctx, domain.Root(), app.WatchParams{Topics: ok}, noop))
			err := r.events.Watch(context.Background(), domain.Root(), app.WatchParams{Topics: append(ok, "home")}, noop)
			assert.ErrorIs(t, err, domain.ErrInvalid)
		})
	})

	t.Run("total watchers", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newEventRig(t, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			for range app.MaxWatchers {
				r.watch(ctx, domain.Root(), app.WatchParams{})
			}
			synctest.Wait()
			require.Equal(t, app.MaxWatchers, r.events.Watchers())
			err := r.events.Watch(ctx, domain.Root(), app.WatchParams{}, noop)
			assert.ErrorIs(t, err, domain.ErrResourceExhausted)
			assert.Equal(t, app.MaxWatchers, r.events.Watchers(), "a refused watch is not counted")

			cancel() // all end; a new one is accepted again
			synctest.Wait()
			require.Equal(t, 0, r.events.Watchers())
			again, stop := context.WithCancel(context.Background())
			stop()
			assert.NoError(t, r.events.Watch(again, domain.Root(), app.WatchParams{}, noop))
		})
	})

	t.Run("watchers per client", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newEventRig(t, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			for range app.MaxWatchersPerClient {
				r.watch(ctx, domain.Root(), app.WatchParams{Client: "198.51.100.1"})
			}
			synctest.Wait()
			require.Equal(t, app.MaxWatchersPerClient, r.events.Watchers())

			err := r.events.Watch(ctx, domain.Root(), app.WatchParams{Client: "198.51.100.1"}, noop)
			assert.ErrorIs(t, err, domain.ErrResourceExhausted)

			// Another client, and callers in process, are not affected.
			r.watch(ctx, domain.Root(), app.WatchParams{Client: "198.51.100.2"})
			r.watch(ctx, domain.Root(), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, app.MaxWatchersPerClient+2, r.events.Watchers())

			cancel()
			synctest.Wait()
			require.Equal(t, 0, r.events.Watchers())
			again, stop := context.WithCancel(context.Background())
			stop()
			assert.NoError(t, r.events.Watch(again, domain.Root(), app.WatchParams{Client: "198.51.100.1"}, noop), "the slots were released")
		})
	})
}

// repoAt declares a repository whose metadata the test changes.
type liveRepo struct {
	mu   sync.Mutex
	repo domain.Repo
}

func (l *liveRepo) set(f func(*domain.Repo)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(&l.repo)
}

func (r *eventRig) liveRepo(s string, vis domain.Visibility, created time.Time) (*liveRepo, domain.RepoName) {
	n := name(r.t, s)
	l := &liveRepo{repo: domain.Repo{Name: n, Visibility: vis, CreatedAt: created}}
	r.meta.EXPECT().Get(mock.Anything, n).RunAndReturn(func(context.Context, domain.RepoName) (domain.Repo, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.repo, nil
	}).Maybe()
	return l, n
}

func TestEventsKeepTheVisibilityOfTheirTime(t *testing.T) {
	t.Parallel()
	t.Run("a repository made public does not reveal what happened while it was private", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newEventRig(t, 0)
			l, n := r.liveRepo("secret", domain.VisibilityPrivate, now)
			root := r.watch(ctx, domain.Root(), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, 1, r.events.Watchers())
			r.push(n, 1)
			r.push(n, 2)
			root.next(t)
			root.next(t) // both are buffered now

			l.set(func(repo *domain.Repo) { repo.Visibility = domain.VisibilityPublic })
			r.feed <- domain.RepoUpdated{Repo: n, At: now, Visibility: domain.VisibilityPublic, Previous: domain.VisibilityPrivate, CreatedAt: now}
			root.next(t)
			r.push(n, 3) // public era

			// An anonymous caller that resumes with an id nobody issued gets what
			// is buffered, filtered: the public update and the public push, not the
			// pushes of the private era.
			anon := r.watch(ctx, domain.Anonymous(), app.WatchParams{ResumeAfter: "bogus"})
			assert.True(t, anon.next(t).Gap)
			upd := anon.next(t)
			require.IsType(t, domain.RepoUpdated{}, upd.Event)
			push := anon.next(t)
			require.IsType(t, domain.PushEvent{}, push.Event)
			assert.Equal(t, fmt.Sprintf("%040x", 3+1), push.Event.(domain.PushEvent).Updates[0].New)
			anon.quiet(t, r, n)
		})
	})

	t.Run("a repository deleted and created again does not inherit old events", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newEventRig(t, 0)
			first := now.Add(-time.Hour)
			l, n := r.liveRepo("again", domain.VisibilityPublic, first)
			root := r.watch(ctx, domain.Root(), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, 1, r.events.Watchers())
			r.push(n, 1)
			root.next(t)

			l.set(func(repo *domain.Repo) { repo.CreatedAt = now }) // deleted, created again
			r.push(n, 2)
			root.next(t)

			anon := r.watch(ctx, domain.Anonymous(), app.WatchParams{ResumeAfter: "bogus"})
			assert.True(t, anon.next(t).Gap)
			e := anon.next(t)
			require.IsType(t, domain.PushEvent{}, e.Event)
			assert.Equal(t, fmt.Sprintf("%040x", 2+1), e.Event.(domain.PushEvent).Updates[0].New, "only the push to the new repository")
			anon.quiet(t, r, n)
		})
	})

	t.Run("a repository made private tells those who could read it, once", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newEventRig(t, 0)
			l, n := r.liveRepo("hide", domain.VisibilityPublic, now)
			anon := r.watch(ctx, domain.Anonymous(), app.WatchParams{})
			reader := r.watch(ctx, tokenPrincipal(domain.ScopeRepoRead, "hide"), app.WatchParams{})
			other := r.watch(ctx, tokenPrincipal(domain.ScopeRepoRead, "elsewhere"), app.WatchParams{})
			root := r.watch(ctx, domain.Root(), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, 4, r.events.Watchers())

			l.set(func(repo *domain.Repo) { repo.Visibility = domain.VisibilityPrivate })
			r.feed <- domain.RepoUpdated{Repo: n, At: now, Visibility: domain.VisibilityPrivate, Previous: domain.VisibilityPublic, CreatedAt: now}

			for name, w := range map[string]*watcher{"anonymous": anon, "reader": reader, "other": other, "root": root} {
				e := w.next(t)
				up, ok := e.Event.(domain.RepoUpdated)
				require.True(t, ok, name)
				assert.Equal(t, n, up.Repo, name)
				require.NotNil(t, e.Repo, name)
				assert.Equal(t, domain.VisibilityPrivate, e.Repo.Visibility, name)
				assert.Empty(t, e.Repo.Description, "nothing but the name and the visibility", name)
			}

			// Later events of the repository reach those who may read it now, only.
			r.push(n, 1)
			assert.IsType(t, domain.PushEvent{}, reader.next(t).Event)
			assert.IsType(t, domain.PushEvent{}, root.next(t).Event)
			anon.quiet(t, r, r.repo("open", domain.VisibilityPublic))
			other.quiet(t, r, r.repo("open2", domain.VisibilityPublic))
		})
	})

	t.Run("a private repository that never was public stays hidden", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newEventRig(t, 0)
			_, n := r.liveRepo("closed", domain.VisibilityPrivate, now)
			anon := r.watch(ctx, domain.Anonymous(), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, 1, r.events.Watchers())
			r.feed <- domain.RepoUpdated{Repo: n, At: now, Visibility: domain.VisibilityPrivate, Previous: domain.VisibilityPrivate, CreatedAt: now}
			r.push(n, 1)
			anon.quiet(t, r, r.repo("open", domain.VisibilityPublic))
		})
	})
}

// TestPushEventsKeepTheirSnapshot: a push is told by the repository as it was
// when published, not as the store says when the feeder gets to it.
func TestPushEventsKeepTheirSnapshot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		swap     func(repo *domain.Repo)
		rootSees bool // the repository is still the one the push is about
	}{
		{"made public before the feeder appends", func(repo *domain.Repo) { repo.Visibility = domain.VisibilityPublic }, true},
		{"recreated before the feeder appends", func(repo *domain.Repo) {
			repo.Visibility = domain.VisibilityPublic
			repo.CreatedAt = now.Add(time.Hour)
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				r := newEventRig(t, 0)
				l, n := r.liveRepo("secret", domain.VisibilityPrivate, now)
				anon := r.watch(ctx, domain.Anonymous(), app.WatchParams{})
				root := r.watch(ctx, domain.Root(), app.WatchParams{})
				synctest.Wait()
				require.Equal(t, 2, r.events.Watchers())

				// Published while private; the store changes before the feeder
				// can have looked at it: the event is built, then the state is
				// swapped, and only then is it handed to the feeder.
				ev := r.pushEvent(n, fmt.Sprintf("%040x", 1), "refs/heads/main")
				l.set(tt.swap)
				r.feed <- ev
				if tt.rootSees {
					root.next(t)
				}

				anon.quiet(t, r, r.repo("open", domain.VisibilityPublic))

				late := r.watch(ctx, domain.Anonymous(), app.WatchParams{ResumeAfter: "bogus"})
				assert.True(t, late.next(t).Gap)
				late.quiet(t, r, r.repo("open2", domain.VisibilityPublic))
			})
		})
	}

	t.Run("a push of an unknown repository is told to nobody", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newEventRig(t, 0)
			n := r.repo("ghost", domain.VisibilityPublic)
			root := r.watch(ctx, domain.Root(), app.WatchParams{})
			synctest.Wait()
			require.Equal(t, 1, r.events.Watchers())
			r.feed <- domain.PushEvent{Repo: n, Visibility: domain.VisibilityPublic, At: now}
			root.quiet(t, r, n)
		})
	})
}
