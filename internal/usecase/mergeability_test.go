// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
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
	svc     *usecase.Mergeability
	mev     <-chan domain.Event
	history []domain.MergeRequestMergeability
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
	deadline := time.After(10 * time.Second)
	for {
		var err error
		m, err = r.record(n)
		if err == nil && m.State == want && m.IsFor(target, head) {
			break
		}
		select {
		case ev := <-r.mev:
			if e, ok := ev.(domain.MergeRequestMergeability); ok {
				r.history = append(r.history, e)
			}
		case <-deadline:
			r.t.Fatalf("mergeability of !%d did not become %s", n, want)
		}
	}

	return m
}

// events returns the mergeability events published so far.
func (r *mergeRig) events() []domain.MergeRequestMergeability {
	out := r.history
	r.history = nil
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
	out := r.history
	r.history = nil
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

func TestMergeabilityPublishesEachSettledResultOnce(t *testing.T) {
	r := newMergeRig(t)
	r.seed()
	r.feature("feat")
	mr := r.open("feat")
	r.svc.ReconcileForTest(r.repo)
	require.Len(t, r.events(), 1)

	// Same tips: nothing is computed, nothing is published.
	r.svc.ReconcileForTest(r.repo)
	assert.Empty(t, r.events())

	// New tips publish even when the state and counts stay the same.
	r.sh("checkout", "-q", "feat")
	r.commit(author, "e.txt", "e\n", "more")
	r.push("feat")
	r.svc.ReconcileForTest(r.repo)
	assert.Len(t, r.events(), 1)
	m, err := r.record(mr.Number)
	require.NoError(t, err)
	assert.Equal(t, domain.MergeabilityUpToDate, m.State)
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
	r.svc.ReconcileForTest(r.repo)
	_, err := r.record(mr.Number)
	require.NoError(t, err)

	_, err = r.mrs.Close(r.ctx, mrW, r.repo, mr.Number)
	require.NoError(t, err)
	_, err = r.record(mr.Number)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	r.svc.ReconcileForTest(r.repo) // a late check writes nothing
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

	// A fresh service has received no OnOpened or Read trigger.
	svc := usecase.NewMergeability(r.ctx, usecase.MergeabilityConfig{
		Store: r.store.MergeRequests(), Meta: r.store, Git: r.git, Bus: r.bus, Clock: fixedClock{},
	})
	defer svc.Close(context.Background())
	svc.Start()
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
	r.store.EXPECT().ListOpen(mock.Anything, repo).Return([]domain.MergeRequest{r.mr}, nil).Maybe()
	r.store.EXPECT().GetMergeability(mock.Anything, repo, int64(7)).Return(domain.Mergeability{}, domain.ErrNotFound).Maybe()
	r.meta.EXPECT().List(mock.Anything).Return(nil, nil).Maybe()
	r.meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo, CreatedAt: now}, nil).Maybe()
	r.svc = usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: r.store, Meta: r.meta, Git: r.git, Bus: r.bus, Clock: fixedClock{},
	})
	t.Cleanup(func() { r.svc.Close(context.Background()) })
	return r
}

func TestMergeabilityDedupsTriggers(t *testing.T) {
	r := newMockRig(t)
	repo := r.mr.Repo
	head, tip := fakeOID(t, "a"), fakeOID(t, "b")
	r.git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
	r.git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
	computed := make(chan struct{}, 1)
	var computes atomic.Int32
	r.git.EXPECT().IsAncestor(mock.Anything, repo, tip, head).RunAndReturn(
		func(context.Context, domain.RepoName, domain.ObjectID, domain.ObjectID) (bool, error) {
			computes.Add(1)
			computed <- struct{}{}
			return true, nil
		})
	r.store.EXPECT().PutMergeability(mock.Anything, mock.Anything).Return(nil)
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Return()

	for range 50 {
		r.svc.OnOpened(context.Background(), r.mr) // queued before any worker runs
	}
	r.svc.Start()
	<-computed
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
	r.svc.ReconcileForTest(repo)                                                                     // no event, no further write
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
		r.svc.ReconcileForTest(repo)
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
		r.svc.ReconcileForTest(repo)
		assert.Equal(t, domain.MaxBehindCount, put.Behind)
		assert.True(t, put.BehindCapped)
	})
}

// A transient Git failure is retried once after the backoff, by rescanning
// the repository through the queue.
func TestMergeabilityRetriesATransientFailureWithBackoff(t *testing.T) {
	repo := name(t, "proj")
	head, tip := fakeOID(t, "a"), fakeOID(t, "b")
	mr := domain.MergeRequest{Repo: repo, Number: 7, Source: "feat", Target: "main", State: domain.MRStateOpen}
	store, meta, git, bus := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t), mocks.NewEventBus(t)
	store.EXPECT().ListOpen(mock.Anything, repo).Return([]domain.MergeRequest{mr}, nil)
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
	store.EXPECT().ListOpen(mock.Anything, repo).Return(nil, errors.New("db down"))
	meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo}, nil)
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
	store.EXPECT().ListOpen(mock.Anything, repo).Return([]domain.MergeRequest{mr}, nil)
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

// Failures of many requests of 3 repositories schedule at most one retry per
// repository on a single scheduler goroutine.
func TestMergeabilityRetriesAreBounded(t *testing.T) {
	const repos = 3
	store, meta := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t)
	barrier := name(t, "barrier")
	done := make(chan struct{})
	store.EXPECT().ListOpen(mock.Anything, barrier).RunAndReturn(func(context.Context, domain.RepoName) ([]domain.MergeRequest, error) { close(done); return nil, nil }).Once()
	store.EXPECT().ListOpen(mock.Anything, mock.Anything).Return(nil, errors.New("db down"))
	meta.EXPECT().Get(mock.Anything, mock.Anything).Return(domain.Repo{}, nil)
	meta.EXPECT().List(mock.Anything).Return(nil, nil)
	asked := make(chan time.Duration, repos)
	svc := usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: store, Meta: meta, Clock: fixedClock{}, Workers: 1,
		After: func(d time.Duration) <-chan time.Time { asked <- d; return make(chan time.Time) },
	})
	svc.Start()
	for r := range repos {
		repo := name(t, "proj"+strconv.Itoa(r))
		for range 20 {
			svc.OnOpened(context.Background(), domain.MergeRequest{Repo: repo})
		}
	}
	svc.OnOpened(context.Background(), domain.MergeRequest{Repo: barrier})
	<-done
	// The shared scheduler only arms the earliest pending retry.
	<-asked
	svc.Close(context.Background())
	assert.Equal(t, repos, svc.ScheduledRetries())
	assert.Equal(t, repos, svc.RetryAttempts())
}

// retryRig is a service over mocks whose only repository holds request !7,
// whose read fails for its first failFor calls. Each repository rescan signals passes.
type retryRig struct {
	svc     *usecase.Mergeability
	repo    domain.RepoName
	reads   atomic.Int32
	failFor atomic.Int32
	passes  chan struct{}
	afters  chan chan time.Time
	barrier chan struct{}
}

func newRetryRig(t *testing.T) *retryRig {
	t.Helper()
	r := &retryRig{repo: name(t, "proj"), passes: make(chan struct{}, 32), afters: make(chan chan time.Time, 32), barrier: make(chan struct{})}
	store, meta, git, bus := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t), mocks.NewEventBus(t)
	r.failFor.Store(1 << 30)
	meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	store.EXPECT().ListOpen(mock.Anything, r.repo).RunAndReturn(
		func(context.Context, domain.RepoName) ([]domain.MergeRequest, error) {
			defer func() { r.passes <- struct{}{} }()
			if r.reads.Add(1) <= r.failFor.Load() {
				return nil, errors.New("db down")
			}
			return nil, nil
		})
	barrier := name(t, "barrier")
	meta.EXPECT().Get(mock.Anything, barrier).RunAndReturn(func(context.Context, domain.RepoName) (domain.Repo, error) {
		r.barrier <- struct{}{}
		return domain.Repo{}, domain.ErrNotFound
	})
	meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: r.repo}}, nil)
	r.svc = usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: store, Meta: meta, Git: git, Bus: bus, Clock: fixedClock{}, Workers: 1,
		After: func(time.Duration) <-chan time.Time { ch := make(chan time.Time, 1); r.afters <- ch; return ch },
	})
	t.Cleanup(func() { r.svc.Close(context.Background()) })
	return r
}

// next fires the pending retry and waits for the rescan it queues.
func (r *retryRig) next() {
	ch := <-r.afters
	ch <- time.Time{}
	<-r.passes
}

func (r *retryRig) settled(t *testing.T) {
	t.Helper()
	r.svc.OnOpened(context.Background(), domain.MergeRequest{Repo: name(t, "barrier")})
	<-r.barrier
	require.Zero(t, r.svc.RetryAttempts())
	require.Zero(t, r.svc.ScheduledRetries())
}

// A persistent failure is retried 5 times, then given up with no state kept.
func TestMergeabilityGivesUpAfterFiveRetries(t *testing.T) {
	r := newRetryRig(t)
	r.svc.Start()
	<-r.passes // the startup rescan fails
	for range 5 {
		r.next()
	}
	r.settled(t)
	assert.Empty(t, r.afters, "no sixth retry")
	assert.Empty(t, r.passes)
}

// A successful full rescan resets the budget: a later failure gets 5 again.
func TestMergeabilitySuccessfulPassForgetsTheRetryBudget(t *testing.T) {
	r := newRetryRig(t)
	r.failFor.Store(2) // the first two reads fail, the third succeeds
	r.svc.Start()
	<-r.passes
	r.next() // second failing pass
	r.next() // success
	r.settled(t)
	r.failFor.Store(1 << 30)
	r.svc.OnOpened(context.Background(), domain.MergeRequest{Repo: r.repo})
	<-r.passes
	for range 5 {
		r.next()
	}
	r.settled(t)
	assert.Empty(t, r.afters)
	assert.Empty(t, r.passes)
}

// stepClock is a clock the test moves.
type stepClock struct{ at atomic.Int64 }

func (c *stepClock) Now() time.Time          { return time.Unix(0, c.at.Load()).UTC() }
func (c *stepClock) advance(d time.Duration) { c.at.Add(int64(d)) }

// An expired timer must not consume a replacement retry of its repository.
func TestMergeabilityStaleTimerKeepsReplacementRetry(t *testing.T) {
	repo := name(t, "proj")
	meta := mocks.NewRepoMetaStore(t)
	meta.EXPECT().List(mock.Anything).Return(nil, nil)
	clock := &stepClock{}
	clock.advance(time.Hour)
	armed := make(chan time.Duration, 8)
	svc := usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Meta: meta, Clock: clock, Workers: 1,
		After: func(d time.Duration) <-chan time.Time { armed <- d; return make(chan time.Time) },
	})
	t.Cleanup(func() { svc.Close(context.Background()) })
	svc.Start()

	svc.RetryForTest(repo)
	assert.Equal(t, time.Second, <-armed, "the scheduler armed the timer of retry A")
	oldDue, ok := svc.RetryDueForTest(repo)
	require.True(t, ok)

	svc.RetryDoneForTest(repo)
	clock.advance(10 * time.Second)
	svc.RetryForTest(repo)
	newDue, ok := svc.RetryDueForTest(repo)
	require.True(t, ok)
	require.True(t, newDue.After(oldDue))

	// The old timer fires while the replacement is not yet due.
	assert.Empty(t, svc.PopDueForTest(repo, oldDue))
	assert.Equal(t, 1, svc.ScheduledRetries())
	// The timer armed for the replacement itself releases it.
	assert.Equal(t, []domain.RepoName{repo}, svc.PopDueForTest(repo, newDue))
	assert.Zero(t, svc.ScheduledRetries())
}

func reconcileMocks(t *testing.T, repo domain.RepoName) (*mocks.MergeRequestStore, *mocks.RepoMetaStore, *mocks.MergeGit, domain.MergeRequest) {
	t.Helper()
	mr := domain.MergeRequest{Repo: repo, Number: 7, Source: "feat", Target: "main", State: domain.MRStateOpen}
	store, meta, git := mocks.NewMergeRequestStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t)
	meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo, CreatedAt: now}, nil)
	store.EXPECT().ListOpen(mock.Anything, repo).Return([]domain.MergeRequest{mr}, nil)
	return store, meta, git, mr
}

func TestMergeabilityDisappearedBranchDeletesTheRecord(t *testing.T) {
	repo := name(t, "proj")
	store, meta, git, _ := reconcileMocks(t, repo)
	git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(domain.ObjectID{}, domain.ErrNotFound)
	git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(fakeOID(t, "b"), nil)
	store.EXPECT().DeleteMergeability(mock.Anything, repo, int64(7)).Return(nil).Once()
	svc := usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: store, Meta: meta, Git: git, Clock: fixedClock{},
	})
	t.Cleanup(func() { svc.Close(context.Background()) })
	assert.True(t, svc.ReconcileForTest(repo))
}

func TestMergeabilityConflictMakesThePassFailAndRetries(t *testing.T) {
	repo := name(t, "proj")
	head, tip := fakeOID(t, "a"), fakeOID(t, "b")
	store, meta, git, mr := reconcileMocks(t, repo)
	meta.EXPECT().List(mock.Anything).Return(nil, nil)
	git.EXPECT().BranchHead(mock.Anything, repo, "feat").Return(head, nil)
	git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(tip, nil)
	git.EXPECT().IsAncestor(mock.Anything, repo, tip, head).Return(true, nil)
	store.EXPECT().GetMergeability(mock.Anything, repo, int64(7)).Return(domain.Mergeability{}, domain.ErrNotFound)
	store.EXPECT().PutMergeability(mock.Anything, mock.Anything).Return(domain.ErrConflict)

	asked := make(chan time.Duration, 4)
	svc := usecase.NewMergeability(context.Background(), usecase.MergeabilityConfig{
		Store: store, Meta: meta, Git: git, Clock: fixedClock{}, Workers: 1,
		After: func(d time.Duration) <-chan time.Time { asked <- d; return make(chan time.Time) },
	})
	t.Cleanup(func() { svc.Close(context.Background()) })
	assert.False(t, svc.ReconcileForTest(repo))

	svc.Start()
	svc.OnOpened(context.Background(), mr)
	assert.Equal(t, time.Second, <-asked, "the conflict scheduled a retry")
}
