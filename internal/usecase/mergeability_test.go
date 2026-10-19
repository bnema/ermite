// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

func fakeOID(t *testing.T, c string) domain.ObjectID {
	t.Helper()
	return oidFrom(t, strings.Repeat(c, 40))
}

// mergeRig runs the mergeability service over the real adapters of mrRig.
type mergeRig struct {
	*mrRig
	svc *usecase.Mergeability
	mev <-chan domain.Event
}

func newMergeRig(t *testing.T) *mergeRig {
	t.Helper()
	r := newMRRig(t)
	svc := usecase.NewMergeability(r.ctx, usecase.MergeabilityConfig{
		Store: r.store.MergeRequests(), Meta: r.store, Git: r.git, Bus: r.bus, Clock: fixedClock{}, Workers: 2,
	})
	r.mrs = usecase.NewMergeRequests(usecase.MergeRequestsConfig{
		Meta: r.store, Store: r.store.MergeRequests(), Repos: r.git, Git: r.git, Signer: nil,
		Committer: domain.Identity{Name: "ermite", Email: "ermite@git.example.org"}, Bus: r.bus, Clock: fixedClock{}, Maint: r.sched,
		Mergeability: svc,
	})
	t.Cleanup(func() { svc.Close(context.Background()) })
	return &mergeRig{mrRig: r, svc: svc, mev: r.bus.Subscribe(r.ctx)}
}

func (r *mergeRig) record(n int64) (domain.Mergeability, error) {
	return r.store.MergeRequests().GetMergeability(r.ctx, r.repo, n)
}

func (r *mergeRig) waitState(n int64, want domain.MergeabilityState, target, head domain.ObjectID) domain.Mergeability {
	r.t.Helper()
	var m domain.Mergeability
	require.Eventually(r.t, func() bool {
		var err error
		m, err = r.record(n)
		return err == nil && m.State == want && m.IsFor(target, head)
	}, 10*time.Second, 10*time.Millisecond, "mergeability of !%d did not become %s", n, want)
	return m
}

// events returns the mergeability events published so far.
func (r *mergeRig) events() []domain.MergeRequestMergeability {
	var out []domain.MergeRequestMergeability
	for {
		select {
		case ev := <-r.mev:
			if m, ok := ev.(domain.MergeRequestMergeability); ok {
				out = append(out, m)
			}
		default:
			return out
		}
	}
}

// awaitEvents receives mergeability events until n arrived, or fails after a
// bound: the workers publish after they stored the record the tests wait for.
func (r *mergeRig) awaitEvents(n int) []domain.MergeRequestMergeability {
	r.t.Helper()
	var out []domain.MergeRequestMergeability
	deadline := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case ev := <-r.mev:
			if m, ok := ev.(domain.MergeRequestMergeability); ok {
				out = append(out, m)
			}
		case <-deadline:
			r.t.Fatalf("got %d mergeability events, want %d", len(out), n)
		}
	}
	return out
}

func TestMergeabilityStatesEndToEnd(t *testing.T) {
	r := newMergeRig(t)
	r.svc.Start()
	main := r.seed()
	head := r.feature("feat")
	mr := r.open("feat")

	// up to date: the target is an ancestor of the head.
	m := r.waitState(mr.Number, domain.MergeabilityUpToDate, main, head)
	assert.False(t, m.CheckedAt.IsZero())

	// behind: the target moved with a change that merges cleanly.
	r.sh("checkout", "-q", "main")
	main2 := r.commit(author, "d.txt", "dee\n", "add d")
	r.push("main")
	r.svc.OnBranchesMoved(r.ctx, r.repo, []domain.RefUpdate{{Old: main.String(), New: main2.String(), Ref: "refs/heads/main"}})
	m = r.waitState(mr.Number, domain.MergeabilityBehind, main2, head)
	assert.Equal(t, 1, m.Behind)
	assert.False(t, m.BehindCapped)

	// conflict: the target changes a line the head changed.
	main3 := r.commit(author, "a.txt", "one\nTWO\nthree\n", "conflict a")
	r.push("main")
	r.svc.OnBranchesMoved(r.ctx, r.repo, []domain.RefUpdate{{Old: main2.String(), New: main3.String(), Ref: "refs/heads/main"}})
	m = r.waitState(mr.Number, domain.MergeabilityConflict, main3, head)
	assert.Equal(t, []string{"a.txt"}, m.ConflictPaths)

	got := r.awaitEvents(3)
	for i, s := range []domain.MergeabilityState{domain.MergeabilityUpToDate, domain.MergeabilityBehind, domain.MergeabilityConflict} {
		assert.Equal(t, s, got[i].Mergeability.State)
		assert.Equal(t, mr.Number, got[i].Number)
	}
}

func TestMergeabilityReaderIsLazy(t *testing.T) {
	r := newMergeRig(t)
	main := r.seed()
	head := r.feature("feat")
	mr := r.open("feat") // queued, but the workers are not started

	d, err := r.mrs.Get(r.ctx, root, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, domain.MergeabilityChecking, d.Mergeability.State)

	r.svc.Start()
	r.waitState(mr.Number, domain.MergeabilityUpToDate, main, head)
	d, err = r.mrs.Get(r.ctx, root, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, domain.MergeabilityUpToDate, d.Mergeability.State)

	page, err := r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{})
	require.NoError(t, err)
	assert.Equal(t, domain.MergeabilityUpToDate, page.Mergeability[mr.Number].State)
}

func TestMergeabilityTransitionOnlyPublication(t *testing.T) {
	r := newMergeRig(t)
	r.seed()
	r.feature("feat")
	mr := r.open("feat")
	r.svc.Check(r.repo, mr.Number)
	require.Len(t, r.events(), 1)

	// Same tips: nothing is computed, nothing is published.
	r.svc.Check(r.repo, mr.Number)
	assert.Empty(t, r.events())

	// The target moves without changing the state: a new record but, being the
	// same state and counts, no event.
	r.sh("checkout", "-q", "feat")
	r.commit(author, "e.txt", "e\n", "more")
	r.push("feat")
	r.svc.Check(r.repo, mr.Number)
	assert.Empty(t, r.events())
	m, err := r.record(mr.Number)
	require.NoError(t, err)
	assert.Equal(t, domain.MergeabilityUpToDate, m.State)
}

func TestMergeabilityPublishesWhenReadersSawChecking(t *testing.T) {
	r := newMergeRig(t)
	r.seed()
	r.feature("feat")
	mr := r.open("feat")
	r.svc.Check(r.repo, mr.Number)
	require.Len(t, r.events(), 1)

	r.sh("checkout", "-q", "feat")
	r.commit(author, "e.txt", "e\n", "more")
	r.push("feat")
	_, err := r.mrs.Get(r.ctx, root, r.repo, mr.Number, ports.GetMergeRequestOptions{}) // sees "checking"
	require.NoError(t, err)
	r.svc.Check(r.repo, mr.Number)
	got := r.events()
	require.Len(t, got, 1)
	assert.Equal(t, domain.MergeabilityUpToDate, got[0].Mergeability.State)
}

func TestMergeabilityMergeReevaluatesOtherRequests(t *testing.T) {
	r := newMergeRig(t)
	r.svc.Start()
	main := r.seed()
	r.sh("checkout", "-q", "-B", "one", "main")
	oneHead := r.commit(author, "one.txt", "1\n", "one")
	r.push("one")
	r.sh("checkout", "-q", "-B", "two", "main")
	twoHead := r.commit(author, "two.txt", "2\n", "two")
	r.push("two")
	one, two := r.open("one"), r.open("two")
	r.waitState(one.Number, domain.MergeabilityUpToDate, main, oneHead)
	r.waitState(two.Number, domain.MergeabilityUpToDate, main, twoHead)

	_, err := r.mrs.Merge(r.ctx, mrM, r.repo, one.Number, ports.MergeParams{HeadID: oneHead})
	require.NoError(t, err)
	m := r.waitState(two.Number, domain.MergeabilityBehind, oneHead, twoHead)
	assert.Equal(t, 1, m.Behind)
	_, err = r.record(one.Number)
	assert.ErrorIs(t, err, domain.ErrNotFound, "the merged request keeps no record")
}

func TestMergeabilityClosedRequestRecordDeleted(t *testing.T) {
	r := newMergeRig(t)
	r.seed()
	r.feature("feat")
	mr := r.open("feat")
	r.svc.Check(r.repo, mr.Number)
	_, err := r.record(mr.Number)
	require.NoError(t, err)

	_, err = r.mrs.Close(r.ctx, mrW, r.repo, mr.Number)
	require.NoError(t, err)
	_, err = r.record(mr.Number)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	r.svc.Check(r.repo, mr.Number) // a late check writes nothing
	_, err = r.record(mr.Number)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMergeabilityStartupRescanComputesMissingRecords(t *testing.T) {
	r := newMergeRig(t)
	main := r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	_, err := r.record(mr.Number)
	require.ErrorIs(t, err, domain.ErrNotFound)

	r.svc.Start()
	r.waitState(mr.Number, domain.MergeabilityUpToDate, main, head)
}

func TestMergeabilityNeedsNoCI(t *testing.T) {
	// The service is built without any CI: a target move still updates the
	// record, and the repository gets no pull ref (the service has no CI to create a run with).
	r := newMergeRig(t)
	r.svc.Start()
	main := r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	r.waitState(mr.Number, domain.MergeabilityUpToDate, main, head)
	r.sh("checkout", "-q", "main")
	main2 := r.commit(author, "d.txt", "dee\n", "add d")
	r.push("main")
	r.svc.OnBranchesMoved(r.ctx, r.repo, []domain.RefUpdate{{Old: main.String(), New: main2.String(), Ref: "refs/heads/main"}})
	r.waitState(mr.Number, domain.MergeabilityBehind, main2, head)

	assert.Empty(t, r.run(r.bare(), r.env(author), "for-each-ref", "refs/pull"))
}

// --- unit tests over mocks ---

type mockRig struct {
	store *mocks.MergeRequestStore
	meta  *mocks.RepoMetaStore
	git   *mocks.MergeGit
	bus   *mocks.EventBus
	svc   *usecase.Mergeability
	mr    domain.MergeRequest
}

func newMockRig(t *testing.T) *mockRig {
	t.Helper()
	repo := name(t, "proj")
	r := &mockRig{
		store: mocks.NewMergeRequestStore(t), meta: mocks.NewRepoMetaStore(t), git: mocks.NewMergeGit(t), bus: mocks.NewEventBus(t),
		mr: domain.MergeRequest{Repo: repo, Number: 7, Source: "feat", Target: "main", State: domain.MRStateOpen},
	}
	r.store.EXPECT().Get(mock.Anything, repo, int64(7)).Return(r.mr, nil).Maybe()
	r.store.EXPECT().GetMergeability(mock.Anything, repo, int64(7)).Return(domain.Mergeability{}, domain.ErrNotFound).Maybe()
	r.meta.EXPECT().List(mock.Anything).Return(nil, nil).Maybe()
	r.meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo, CreatedAt: now}, nil).Maybe()
	r.svc = usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: r.store, Meta: r.meta, Git: r.git, Bus: r.bus, Clock: fixedClock{},
	})
	t.Cleanup(func() { r.svc.Close(context.Background()) })
	return r
}

func TestMergeabilityStaleResultIsDiscarded(t *testing.T) {
	r := newMockRig(t)
	repo := r.mr.Repo
	head, t1, t2 := fakeOID(t, "a"), fakeOID(t, "b"), fakeOID(t, "c")
	var moved atomic.Bool
	r.git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
	r.git.EXPECT().BranchHead(mock.Anything, repo, "main").RunAndReturn(
		func(context.Context, domain.RepoName, string) (domain.ObjectID, error) {
			if moved.Load() {
				return t2, nil
			}
			return t1, nil
		})
	// The target moves while the first computation runs.
	r.git.EXPECT().IsAncestor(mock.Anything, repo, mock.Anything, head).RunAndReturn(
		func(context.Context, domain.RepoName, domain.ObjectID, domain.ObjectID) (bool, error) {
			moved.Store(true)
			return true, nil
		})
	var puts []domain.Mergeability
	r.store.EXPECT().PutMergeability(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, m domain.Mergeability) error { puts = append(puts, m); return nil })
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Return()

	r.svc.Check(repo, 7)
	require.Len(t, puts, 1, "the result for the old tip is dropped, the one for the new tip is stored")
	assert.Equal(t, t2, puts[0].TargetID)
}

func TestMergeabilityDedupsTriggers(t *testing.T) {
	r := newMockRig(t)
	repo := r.mr.Repo
	head, tip := fakeOID(t, "a"), fakeOID(t, "b")
	r.git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
	r.git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
	var computes atomic.Int32
	r.git.EXPECT().IsAncestor(mock.Anything, repo, tip, head).RunAndReturn(
		func(context.Context, domain.RepoName, domain.ObjectID, domain.ObjectID) (bool, error) {
			computes.Add(1)
			return true, nil
		})
	r.store.EXPECT().PutMergeability(mock.Anything, mock.Anything).Return(nil)
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Return()

	for range 50 {
		r.svc.OnOpened(context.Background(), r.mr) // queued before any worker runs
	}
	r.svc.Start()
	require.Eventually(t, func() bool { return computes.Load() >= 1 }, 5*time.Second, 5*time.Millisecond)
	r.svc.Close(context.Background()) // the workers have each looked at the queue once more
	assert.EqualValues(t, 1, computes.Load())
}

func TestMergeabilityNoWriteForClosedRequest(t *testing.T) {
	r := newMockRig(t)
	repo := r.mr.Repo
	head, tip := fakeOID(t, "a"), fakeOID(t, "b")
	r.git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
	r.git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
	r.git.EXPECT().IsAncestor(mock.Anything, repo, tip, head).Return(true, nil)
	r.store.EXPECT().PutMergeability(mock.Anything, mock.Anything).Return(domain.ErrNotFound).Once() // closed meanwhile
	r.svc.Check(repo, 7)                                                                             // no event, no further write
}

// A reader that saw a stale record marks the request seen and queues it; the
// worker finds the record already current (stored and published by an earlier
// check) and must still publish it, as the reader was told "checking".
func TestMergeabilityFastPathPublishesWhenAReaderSawChecking(t *testing.T) {
	repo := name(t, "proj")
	head, tip := fakeOID(t, "a"), fakeOID(t, "b")
	mr := domain.MergeRequest{Repo: repo, Number: 7, Source: "feat", Target: "main", State: domain.MRStateOpen, HeadID: head}
	rec := domain.Mergeability{Repo: repo, Number: 7, State: domain.MergeabilityUpToDate, TargetID: tip, HeadID: head, CheckedAt: now}
	store, meta, git, bus := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t), mocks.NewEventBus(t)
	store.EXPECT().Get(mock.Anything, repo, int64(7)).Return(mr, nil)
	store.EXPECT().GetMergeability(mock.Anything, repo, int64(7)).Return(domain.Mergeability{}, domain.ErrNotFound).Once() // the reader's read: stale
	store.EXPECT().GetMergeability(mock.Anything, repo, int64(7)).Return(rec, nil)                                         // the worker has stored it since
	meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo, CreatedAt: now}, nil)
	git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
	git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
	var published []domain.Event
	bus.EXPECT().Publish(mock.Anything, mock.Anything).Run(func(_ context.Context, ev domain.Event) { published = append(published, ev) }).Return().Once()
	svc := usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{Store: store, Meta: meta, Git: git, Bus: bus, Clock: fixedClock{}})
	t.Cleanup(func() { svc.Close(context.Background()) })

	got := svc.Read(context.Background(), mr, tip)
	assert.Equal(t, domain.MergeabilityChecking, got.State)
	svc.Check(repo, 7)
	require.Len(t, published, 1, "the current record is published for the reader")
	assert.Equal(t, domain.MergeabilityUpToDate, published[0].(domain.MergeRequestMergeability).Mergeability.State)

	svc.Check(repo, 7) // nobody saw "checking" since: nothing more
	assert.Len(t, published, 1)
}

// A rescan claims a key that is also pending; a worker must then not run it
// concurrently (GO-001).
func TestMergeabilityRescanAndWorkersNeverRunAKeyConcurrently(t *testing.T) {
	r := newMockRig(t)
	repo := r.mr.Repo
	head, tip := fakeOID(t, "a"), fakeOID(t, "b")
	r.git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
	r.git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
	var inflight, peak atomic.Int32
	entered, release := make(chan struct{}, 8), make(chan struct{})
	r.git.EXPECT().IsAncestor(mock.Anything, repo, tip, head).RunAndReturn(
		func(context.Context, domain.RepoName, domain.ObjectID, domain.ObjectID) (bool, error) {
			n := inflight.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			entered <- struct{}{}
			<-release
			inflight.Add(-1)
			return true, nil
		})
	r.store.EXPECT().PutMergeability(mock.Anything, mock.Anything).Return(nil)
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Return()

	r.svc.OnOpened(context.Background(), r.mr) // pending
	done := make(chan struct{})
	go func() { r.svc.Check(repo, 7); close(done) }() // claims it, as a rescan does
	<-entered
	r.svc.Start() // two workers
	r.svc.Close(context.Background())
	close(release)
	<-done
	assert.EqualValues(t, 1, peak.Load(), "one check of the key at a time")
}

func TestMergeabilityBoundsConflictPathsAndBehind(t *testing.T) {
	t.Run("conflict paths", func(t *testing.T) {
		r := newMockRig(t)
		repo := r.mr.Repo
		head, tip := fakeOID(t, "a"), fakeOID(t, "b")
		r.git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
		r.git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
		r.git.EXPECT().IsAncestor(mock.Anything, repo, tip, head).Return(false, nil)
		var paths []string
		for i := 0; i < domain.MaxConflictPaths+3; i++ {
			paths = append(paths, strings.Repeat("p", i+1))
		}
		r.git.EXPECT().MergeTree(mock.Anything, repo, tip, head).Return(domain.MergeTreeResult{ConflictPaths: paths}, nil)
		var put domain.Mergeability
		r.store.EXPECT().PutMergeability(mock.Anything, mock.Anything).RunAndReturn(
			func(_ context.Context, m domain.Mergeability) error { put = m; return nil })
		r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Return()
		r.svc.Check(repo, 7)
		assert.Equal(t, domain.MergeabilityConflict, put.State)
		assert.Len(t, put.ConflictPaths, domain.MaxConflictPaths)
		assert.True(t, put.ConflictPathsTruncated)
	})
	t.Run("behind", func(t *testing.T) {
		r := newMockRig(t)
		repo := r.mr.Repo
		head, tip := fakeOID(t, "a"), fakeOID(t, "b")
		r.git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
		r.git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
		r.git.EXPECT().IsAncestor(mock.Anything, repo, tip, head).Return(false, nil)
		r.git.EXPECT().MergeTree(mock.Anything, repo, tip, head).Return(domain.MergeTreeResult{Clean: true}, nil)
		r.git.EXPECT().CountBehind(mock.Anything, repo, head, tip, domain.MaxBehindCount+1).Return(domain.MaxBehindCount+1, nil)
		var put domain.Mergeability
		r.store.EXPECT().PutMergeability(mock.Anything, mock.Anything).RunAndReturn(
			func(_ context.Context, m domain.Mergeability) error { put = m; return nil })
		r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Return()
		r.svc.Check(repo, 7)
		assert.Equal(t, domain.MaxBehindCount, put.Behind)
		assert.True(t, put.BehindCapped)
	})
}

// A transient Git failure is retried once after the backoff.
func TestMergeabilityRetriesATransientFailureWithBackoff(t *testing.T) {
	repo := name(t, "proj")
	head, tip := fakeOID(t, "a"), fakeOID(t, "b")
	mr := domain.MergeRequest{Repo: repo, Number: 7, Source: "feat", Target: "main", State: domain.MRStateOpen}
	store, meta, git, bus := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t), mocks.NewEventBus(t)
	store.EXPECT().Get(mock.Anything, repo, int64(7)).Return(mr, nil)
	store.EXPECT().GetMergeability(mock.Anything, repo, int64(7)).Return(domain.Mergeability{}, domain.ErrNotFound)
	store.EXPECT().PutMergeability(mock.Anything, mock.Anything).Return(nil)
	meta.EXPECT().List(mock.Anything).Return(nil, nil)
	meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo, CreatedAt: now}, nil)
	var fails atomic.Int32
	git.EXPECT().BranchHead(mock.Anything, repo, "feat").RunAndReturn(
		func(context.Context, domain.RepoName, string) (domain.ObjectID, error) {
			if fails.Add(1) == 1 {
				return domain.ObjectID{}, errors.New("transient")
			}
			return head, nil
		})
	git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
	git.EXPECT().IsAncestor(mock.Anything, repo, tip, head).Return(true, nil)
	published := make(chan struct{})
	bus.EXPECT().Publish(mock.Anything, mock.Anything).Run(func(context.Context, domain.Event) { close(published) }).Return().Once()

	asked, fire := make(chan time.Duration, 4), make(chan time.Time)
	svc := usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: store, Meta: meta, Git: git, Bus: bus, Clock: fixedClock{},
		After: func(d time.Duration) <-chan time.Time { asked <- d; return fire },
	})
	t.Cleanup(func() { svc.Close(context.Background()) })
	svc.Start()
	svc.OnOpened(context.Background(), mr)

	assert.Equal(t, time.Second, <-asked)
	select {
	case <-published:
		t.Fatal("published before the backoff elapsed")
	default:
	}
	fire <- time.Time{}
	<-published
	assert.Empty(t, asked, "one retry was enough and only one was scheduled")
}

// Close stops a pending retry timer's goroutine.
func TestMergeabilityCloseStopsPendingRetries(t *testing.T) {
	repo := name(t, "proj")
	mr := domain.MergeRequest{Repo: repo, Number: 7, Source: "feat", Target: "main", State: domain.MRStateOpen}
	store, meta, git, bus := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t), mocks.NewEventBus(t)
	store.EXPECT().Get(mock.Anything, repo, int64(7)).Return(domain.MergeRequest{}, errors.New("db down"))
	meta.EXPECT().List(mock.Anything).Return(nil, nil)
	asked := make(chan time.Duration, 4)
	svc := usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: store, Meta: meta, Git: git, Bus: bus, Clock: fixedClock{},
		After: func(d time.Duration) <-chan time.Time { asked <- d; return make(chan time.Time) },
	})
	svc.Start()
	svc.OnOpened(context.Background(), mr)
	<-asked
	done := make(chan struct{})
	go func() { svc.Close(context.Background()); close(done) }()
	<-done // Close waits for the timer goroutine: it returns only if it stopped
}

// Workers are not stopped by the cancellation of the context given to the
// constructor; Close stops them.
func TestMergeabilityWorkersOutliveTheParentContext(t *testing.T) {
	repo := name(t, "proj")
	head, tip := fakeOID(t, "a"), fakeOID(t, "b")
	mr := domain.MergeRequest{Repo: repo, Number: 7, Source: "feat", Target: "main", State: domain.MRStateOpen}
	store, meta, git, bus := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t), mocks.NewEventBus(t)
	store.EXPECT().Get(mock.Anything, repo, int64(7)).Return(mr, nil)
	store.EXPECT().GetMergeability(mock.Anything, repo, int64(7)).Return(domain.Mergeability{}, domain.ErrNotFound)
	store.EXPECT().PutMergeability(mock.Anything, mock.Anything).Return(nil)
	meta.EXPECT().List(mock.Anything).Return(nil, nil)
	meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo, CreatedAt: now}, nil)
	git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
	git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
	git.EXPECT().IsAncestor(mock.Anything, repo, tip, head).Return(true, nil)
	published := make(chan struct{})
	bus.EXPECT().Publish(mock.Anything, mock.Anything).Run(func(context.Context, domain.Event) { close(published) }).Return().Once()

	parent, cancel := context.WithCancel(context.Background())
	svc := usecase.NewMergeability(parent, usecase.MergeabilityConfig{Store: store, Meta: meta, Git: git, Bus: bus, Clock: fixedClock{}})
	svc.Start()
	cancel()
	svc.OnOpened(context.Background(), mr)
	<-published // computed although the parent is cancelled
	svc.Close(context.Background())
}

// A global failure schedules a bounded number of retries on a single
// scheduler goroutine, however many requests fail.
func TestMergeabilityRetriesAreBounded(t *testing.T) {
	const queue, repos, perRepo = 4, 3, 20
	store, meta, git, bus := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t), mocks.NewEventBus(t)
	store.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).Return(domain.MergeRequest{}, errors.New("db down"))
	meta.EXPECT().List(mock.Anything).Return(nil, nil)
	svc := usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: store, Meta: meta, Git: git, Bus: bus, Clock: fixedClock{}, Workers: 1, Queue: queue,
		After: func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	})
	svc.Start()
	before := runtime.NumGoroutine()
	for r := range repos {
		repo := name(t, "proj"+strconv.Itoa(r))
		for n := range perRepo {
			svc.Check(repo, int64(n+1))
		}
	}
	assert.LessOrEqual(t, svc.ScheduledRetries(), queue+repos)
	assert.Positive(t, svc.ScheduledRetries())
	assert.LessOrEqual(t, runtime.NumGoroutine(), before, "retries must not start goroutines")
	svc.Close(context.Background()) // returns although retries are pending
}

// A request whose retry was coalesced into a repository rescan is checked
// again through the queue when the rescan is due.
func TestMergeabilityDueRetriesReenterTheQueue(t *testing.T) {
	repo := name(t, "proj")
	store, meta, git, bus := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t), mocks.NewEventBus(t)
	var calls [9]atomic.Int32
	done := make(chan struct{})
	store.EXPECT().Get(mock.Anything, repo, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.RepoName, n int64) (domain.MergeRequest, error) {
			if calls[n].Add(1) == 1 {
				return domain.MergeRequest{}, errors.New("db down")
			}
			if n == 8 {
				close(done)
			}
			return domain.MergeRequest{}, domain.ErrNotFound
		})
	store.EXPECT().ListOpen(mock.Anything, repo).Return([]domain.MergeRequest{{Repo: repo, Number: 8}}, nil)
	meta.EXPECT().List(mock.Anything).Return(nil, nil)
	afters := make(chan chan time.Time, 4)
	svc := usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: store, Meta: meta, Git: git, Bus: bus, Clock: fixedClock{}, Workers: 1, Queue: 1,
		After: func(time.Duration) <-chan time.Time { ch := make(chan time.Time, 1); afters <- ch; return ch },
	})
	t.Cleanup(func() { svc.Close(context.Background()) })
	svc.Start()
	svc.Check(repo, 7) // scheduled itself
	svc.Check(repo, 8) // queue full: coalesced into a rescan
	assert.Equal(t, 2, svc.ScheduledRetries())
	stop := fireAll(afters) // !7 is due and queued again, then the rescan checks !8
	defer stop()
	<-done
	assert.EqualValues(t, 2, calls[7].Load())
}

// A transient failure to publish after the store is retried and the event is
// published without any other trigger.
func TestMergeabilityRetriesAFailedPublish(t *testing.T) {
	repo := name(t, "proj")
	head, tip := fakeOID(t, "a"), fakeOID(t, "b")
	mr := domain.MergeRequest{Repo: repo, Number: 7, Source: "feat", Target: "main", State: domain.MRStateOpen}
	rec := domain.Mergeability{Repo: repo, Number: 7, TargetID: tip, HeadID: head, State: domain.MergeabilityUpToDate}
	store, meta, git, bus := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t), mocks.NewEventBus(t)
	store.EXPECT().Get(mock.Anything, repo, int64(7)).Return(mr, nil)
	var gets atomic.Int32
	store.EXPECT().GetMergeability(mock.Anything, repo, int64(7)).RunAndReturn(
		func(context.Context, domain.RepoName, int64) (domain.Mergeability, error) {
			if gets.Add(1) == 1 {
				return domain.Mergeability{}, domain.ErrNotFound
			}
			return rec, nil
		})
	store.EXPECT().PutMergeability(mock.Anything, mock.Anything).Return(nil).Once()
	meta.EXPECT().List(mock.Anything).Return(nil, nil)
	var metaGets atomic.Int32
	meta.EXPECT().Get(mock.Anything, repo).RunAndReturn(
		func(context.Context, domain.RepoName) (domain.Repo, error) {
			if metaGets.Add(1) == 1 {
				return domain.Repo{}, errors.New("transient")
			}
			return domain.Repo{Name: repo, CreatedAt: now}, nil
		})
	git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
	git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
	git.EXPECT().IsAncestor(mock.Anything, repo, tip, head).Return(true, nil)
	published := make(chan struct{})
	bus.EXPECT().Publish(mock.Anything, mock.Anything).Run(func(context.Context, domain.Event) { close(published) }).Return().Once()

	afters := make(chan chan time.Time, 4)
	svc := usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: store, Meta: meta, Git: git, Bus: bus, Clock: fixedClock{},
		After: func(time.Duration) <-chan time.Time { ch := make(chan time.Time, 1); afters <- ch; return ch },
	})
	t.Cleanup(func() { svc.Close(context.Background()) })
	svc.Start()
	svc.OnOpened(context.Background(), mr)
	<-svcWaiting(afters)
	select {
	case <-published:
		t.Fatal("published although the repository could not be read")
	default:
	}
	stop := fireAll(afters)
	defer stop()
	<-published
}

// svcWaiting blocks until the scheduler asked for a timer, and puts it back.
func svcWaiting(afters chan chan time.Time) <-chan struct{} {
	ready := make(chan struct{})
	go func() { ch := <-afters; afters <- ch; close(ready) }()
	return ready
}

// fireAll fires every timer the scheduler asks for (stale ones are harmless).
func fireAll(afters chan chan time.Time) (stop func()) {
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case ch := <-afters:
				ch <- time.Time{}
			case <-quit:
				return
			}
		}
	}()
	return func() { close(quit) }
}
