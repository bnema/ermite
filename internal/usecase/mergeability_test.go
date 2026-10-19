// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
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

func (r *mergeRig) push2(branch, path, content string) domain.ObjectID {
	r.t.Helper()
	r.sh("checkout", "-q", branch)
	id := r.commit(author, path, content, "edit "+path)
	r.push(branch)
	return id
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

	got := r.events()
	require.Len(t, got, 3)
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
	time.Sleep(50 * time.Millisecond)
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
