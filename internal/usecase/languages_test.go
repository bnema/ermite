// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
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

type langRig struct {
	meta   *mocks.RepoMetaStore
	reader *mocks.RepoReader
	store  *mocks.LanguageStore
	files  *mocks.TreeFileLister
	l      *usecase.Languages
}

func newLangRig(t *testing.T, repos ...domain.Repo) *langRig {
	t.Helper()
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	r := &langRig{
		meta: mocks.NewRepoMetaStore(t), reader: mocks.NewRepoReader(t),
		store: mocks.NewLanguageStore(t), files: mocks.NewTreeFileLister(t),
	}
	r.meta.EXPECT().List(mock.Anything).Return(repos, nil).Maybe()
	r.l = usecase.NewLanguages(context.Background(), usecase.LanguagesConfig{
		Meta: r.meta, Store: r.store, Reader: r.reader, Files: r.files, Clock: clock, Pace: -1,
	})
	t.Cleanup(func() { r.l.Close(context.Background()) })
	return r
}

func (r *langRig) repo(n domain.RepoName) {
	r.meta.EXPECT().Get(mock.Anything, n).Return(domain.Repo{Name: n, CreatedAt: now}, nil).Maybe()
}

func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: " + what)
	}
}

var (
	goFiles = domain.FileSizes{Files: []domain.FileSize{{Path: "main.go", Size: 30}, {Path: "run.sh", Size: 10}}}
	headA   = domain.ResolvedRevision{CommitID: mustOID(oidA)}
	headB   = domain.ResolvedRevision{CommitID: mustOID(oidB)}
)

func mustOID(s string) domain.ObjectID {
	id, err := domain.ParseObjectID(s)
	if err != nil {
		panic(err)
	}
	return id
}

func TestLanguagesCountsAndStores(t *testing.T) {
	r := newLangRig(t)
	n := name(t, "tools")
	r.repo(n)
	r.reader.EXPECT().ResolveRevision(mock.Anything, n, domain.Revision{}).Return(headA, nil)
	r.store.EXPECT().Get(mock.Anything, n).Return(domain.RepoLanguages{}, false, nil)
	r.files.EXPECT().ListFileSizes(mock.Anything, n, headA.CommitID, domain.MaxLanguageFiles).Return(goFiles, nil)
	done := make(chan struct{})
	r.store.EXPECT().Put(mock.Anything, mock.Anything).Run(func(_ context.Context, rec domain.RepoLanguages) {
		assert.Equal(t, n, rec.Repo)
		assert.Equal(t, headA.CommitID, rec.Commit)
		assert.Equal(t, now, rec.RepoCreatedAt)
		assert.Equal(t, domain.LanguagesVersion, rec.Version)
		assert.Equal(t, []domain.LanguageBytes{{Name: "Go", Bytes: 30}, {Name: "Shell", Bytes: 10}}, rec.Languages)
		close(done)
	}).Return(nil).Once()
	r.l.Start()
	r.l.OnRepoChanged(context.Background(), n)
	wait(t, done, "put")
}

func TestLanguagesSkipsAnUnchangedHeadButNotAnOldVersion(t *testing.T) {
	r := newLangRig(t)
	n, m := name(t, "same"), name(t, "oldver")
	r.repo(n)
	r.repo(m)
	r.reader.EXPECT().ResolveRevision(mock.Anything, mock.Anything, domain.Revision{}).Return(headA, nil)
	r.store.EXPECT().Get(mock.Anything, n).Return(domain.RepoLanguages{Repo: n, Commit: headA.CommitID, Version: domain.LanguagesVersion, RepoCreatedAt: now}, true, nil).Once()
	r.store.EXPECT().Get(mock.Anything, m).Return(domain.RepoLanguages{Repo: m, Commit: headA.CommitID, Version: domain.LanguagesVersion - 1, RepoCreatedAt: now}, true, nil).Once()
	r.files.EXPECT().ListFileSizes(mock.Anything, m, headA.CommitID, mock.Anything).Return(goFiles, nil).Once() // n: no call
	done := make(chan struct{})
	r.store.EXPECT().Put(mock.Anything, mock.Anything).Run(func(context.Context, domain.RepoLanguages) { close(done) }).Return(nil).Once()
	r.l.Start()
	r.l.OnRepoChanged(context.Background(), n)
	r.l.OnRepoChanged(context.Background(), m)
	wait(t, done, "put of the old version")
}

func TestLanguagesEmptyRepositoryDeletesTheRecord(t *testing.T) {
	r := newLangRig(t)
	n := name(t, "empty")
	r.repo(n)
	r.reader.EXPECT().ResolveRevision(mock.Anything, n, domain.Revision{}).Return(domain.ResolvedRevision{}, domain.ErrNotFound)
	done := make(chan struct{})
	r.store.EXPECT().Delete(mock.Anything, n).Run(func(context.Context, domain.RepoName) { close(done) }).Return(nil).Once()
	r.l.Start()
	r.l.OnRepoChanged(context.Background(), n)
	wait(t, done, "delete")
}

func TestLanguagesDropsAStaleResultAndComputesAgain(t *testing.T) {
	r := newLangRig(t)
	n := name(t, "tools")
	r.repo(n)
	var resolves atomic.Int32
	// HEAD: A (job 1), B (re-check, moved), then B and B (job 2).
	r.reader.EXPECT().ResolveRevision(mock.Anything, n, domain.Revision{}).RunAndReturn(func(context.Context, domain.RepoName, domain.Revision) (domain.ResolvedRevision, error) {
		if resolves.Add(1) == 1 {
			return headA, nil
		}
		return headB, nil
	})
	r.store.EXPECT().Get(mock.Anything, n).Return(domain.RepoLanguages{}, false, nil)
	r.files.EXPECT().ListFileSizes(mock.Anything, n, mock.Anything, mock.Anything).Return(goFiles, nil)
	done := make(chan struct{})
	r.store.EXPECT().Put(mock.Anything, mock.Anything).Run(func(_ context.Context, rec domain.RepoLanguages) {
		assert.Equal(t, headB.CommitID, rec.Commit, "the stale count was never stored")
		close(done)
	}).Return(nil).Once()
	r.l.Start()
	r.l.OnRepoChanged(context.Background(), n)
	wait(t, done, "put of the fresh count")
}

func TestLanguagesTriggerDuringARunRunsExactlyOnceMore(t *testing.T) {
	r := newLangRig(t)
	n := name(t, "tools")
	r.repo(n)
	r.reader.EXPECT().ResolveRevision(mock.Anything, n, domain.Revision{}).Return(headA, nil)
	r.store.EXPECT().Get(mock.Anything, n).Return(domain.RepoLanguages{}, false, nil)
	entered, release := make(chan struct{}, 4), make(chan struct{})
	var runs atomic.Int32
	r.files.EXPECT().ListFileSizes(mock.Anything, n, mock.Anything, mock.Anything).RunAndReturn(func(context.Context, domain.RepoName, domain.ObjectID, int) (domain.FileSizes, error) {
		runs.Add(1)
		entered <- struct{}{}
		<-release
		return goFiles, nil
	})
	puts := make(chan struct{}, 4)
	r.store.EXPECT().Put(mock.Anything, mock.Anything).Run(func(context.Context, domain.RepoLanguages) { puts <- struct{}{} }).Return(nil)
	r.l.Start()
	r.l.OnRepoChanged(context.Background(), n)
	wait(t, entered, "first run")
	for range 5 { // coalesced into one more run
		r.l.OnRepoChanged(context.Background(), n)
	}
	release <- struct{}{}
	wait(t, puts, "first put")
	wait(t, entered, "second run")
	close(release)
	wait(t, puts, "second put")
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, 2, runs.Load())
}

func TestLanguagesPanicIsRecoveredAndTheNextRepositoryIsServed(t *testing.T) {
	r := newLangRig(t)
	bad, good := name(t, "bad"), name(t, "good")
	r.repo(bad)
	r.repo(good)
	r.reader.EXPECT().ResolveRevision(mock.Anything, mock.Anything, domain.Revision{}).Return(headA, nil)
	r.store.EXPECT().Get(mock.Anything, mock.Anything).Return(domain.RepoLanguages{}, false, nil)
	r.files.EXPECT().ListFileSizes(mock.Anything, bad, mock.Anything, mock.Anything).Panic("boom")
	r.files.EXPECT().ListFileSizes(mock.Anything, good, mock.Anything, mock.Anything).Return(goFiles, nil)
	done := make(chan struct{})
	r.store.EXPECT().Put(mock.Anything, mock.MatchedBy(func(rec domain.RepoLanguages) bool { return rec.Repo == good })).
		Run(func(context.Context, domain.RepoLanguages) { close(done) }).Return(nil).Once()
	r.l.Start()
	r.l.OnRepoChanged(context.Background(), bad)
	r.l.OnRepoChanged(context.Background(), good)
	wait(t, done, "put of the second repository")
}

func TestLanguagesBackfillCountsEveryRepositoryAndHotGoesFirst(t *testing.T) {
	a, b, hot := name(t, "aaa"), name(t, "bbb"), name(t, "hot")
	r := newLangRig(t, domain.Repo{Name: a, CreatedAt: now}, domain.Repo{Name: b, CreatedAt: now})
	r.repo(a)
	r.repo(b)
	r.repo(hot)
	r.reader.EXPECT().ResolveRevision(mock.Anything, mock.Anything, domain.Revision{}).Return(headA, nil)
	r.store.EXPECT().Get(mock.Anything, mock.Anything).Return(domain.RepoLanguages{}, false, nil)
	r.files.EXPECT().ListFileSizes(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(goFiles, nil)
	order := make(chan domain.RepoName, 3)
	r.store.EXPECT().Put(mock.Anything, mock.Anything).Run(func(_ context.Context, rec domain.RepoLanguages) { order <- rec.Repo }).Return(nil)
	// Mark before Start: the hot repository is queued ahead of the backfill.
	r.l.OnRepoChanged(context.Background(), hot)
	r.l.Start()
	got := map[domain.RepoName]bool{}
	first := <-order
	assert.Equal(t, hot, first)
	got[first] = true
	for range 2 {
		select {
		case n := <-order:
			got[n] = true
		case <-time.After(5 * time.Second):
			t.Fatal("backfill did not finish")
		}
	}
	assert.Len(t, got, 3)
}

func TestLanguagesCloseCancelsABlockedJob(t *testing.T) {
	r := newLangRig(t)
	n := name(t, "tools")
	r.repo(n)
	r.reader.EXPECT().ResolveRevision(mock.Anything, n, domain.Revision{}).Return(headA, nil)
	r.store.EXPECT().Get(mock.Anything, n).Return(domain.RepoLanguages{}, false, nil)
	entered := make(chan struct{})
	r.files.EXPECT().ListFileSizes(mock.Anything, n, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ domain.RepoName, _ domain.ObjectID, _ int) (domain.FileSizes, error) {
		close(entered)
		<-ctx.Done()
		return domain.FileSizes{}, ctx.Err()
	})
	r.l.Start()
	r.l.OnRepoChanged(context.Background(), n)
	wait(t, entered, "job start")
	closed := make(chan struct{})
	go func() { r.l.Close(context.Background()); close(closed) }()
	wait(t, closed, "Close")
}

func TestLanguagesPushRecordNotifies(t *testing.T) {
	f := newPushFixture(t)
	n := name(t, "tools")
	trigger := mocks.NewLanguageTrigger(t)
	f.meta.On("Mutate", mock.Anything, n, mock.Anything).Return(domain.Repo{Name: n}, nil)
	f.git.On("DefaultBranch", mock.Anything, n).Return("main", true, nil)
	f.bus.On("Publish", mock.Anything, mock.Anything).Return()
	f.sched.On("Schedule", mock.Anything, n).Return()
	trigger.EXPECT().OnRepoChanged(mock.Anything, n).Return().Once()
	require.NoError(t, f.pushes.WithLanguages(trigger).Record(context.Background(), domain.Root(), n, []domain.RefUpdate{}))
}

func TestLanguagesMergeNotifies(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	trigger := mocks.NewLanguageTrigger(t)
	notified := make(chan struct{}, 1)
	trigger.EXPECT().OnRepoChanged(mock.Anything, r.repo).Run(func(context.Context, domain.RepoName) { notified <- struct{}{} }).Return().Once()
	r.mrs.WithLanguages(trigger)
	head := r.feature("f0")
	mr := r.open("f0")
	_, err := r.mrs.Merge(r.ctx, root, r.repo, mr.Number, ports.MergeParams{HeadID: head})
	require.NoError(t, err)
	r.mrs.Wait(r.ctx)
	wait(t, notified, "merge notification")
}

func TestLanguagesDefaultBranchChangeNotifies(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	n := name(t, "tools")
	trigger := mocks.NewLanguageTrigger(t)
	trigger.EXPECT().OnRepoChanged(mock.Anything, n).Return().Once()
	f.repos.WithLanguages(trigger, nil)
	f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{Name: n, Visibility: domain.VisibilityPrivate}, nil)
	f.git.EXPECT().HasBranch(ctx, n, "dev").Return(true, nil)
	f.git.EXPECT().SetDefaultBranch(ctx, n, "dev").Return(nil)
	f.bus.EXPECT().Publish(ctx, mock.Anything).Return().Once()
	_, err := f.repos.Update(ctx, tokenPrincipal(domain.ScopeRepoAdmin, "tools"), n, ports.UpdateRepoParams{DefaultBranch: ptr("dev")})
	require.NoError(t, err)
}

func TestReposViewLanguages(t *testing.T) {
	ctx := context.Background()
	n := name(t, "tools")
	rec := domain.RepoLanguages{Repo: n, RepoCreatedAt: now, Languages: []domain.LanguageBytes{{Name: "Go", Bytes: 3}, {Name: "Shell", Bytes: 1}}}

	f := newFixture(t)
	store := mocks.NewLanguageStore(t)
	f.repos.WithLanguages(nil, store)
	f.git.EXPECT().HeadBranch(ctx, n).Return("main", nil)
	store.EXPECT().Get(ctx, n).Return(rec, true, nil)
	v, err := f.repos.View(ctx, domain.Repo{Name: n, CreatedAt: now})
	require.NoError(t, err)
	assert.Equal(t, rec.Shares(), v.Languages)
	assert.InDelta(t, 75, v.Languages[0].Percent, 1e-9)

	// A record of a repository created again under the name is not served.
	v, err = f.repos.View(ctx, domain.Repo{Name: n, CreatedAt: now.Add(time.Hour)})
	require.NoError(t, err)
	assert.Nil(t, v.Languages)
}
