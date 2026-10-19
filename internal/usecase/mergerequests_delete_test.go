// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zerowrap"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

// A merge can delete its source branch; the push event carries both refs.
func TestMergeDeletesTheSourceBranch(t *testing.T) {
	t.Parallel()
	for _, mode := range []domain.MergeMode{domain.MergeModeFastForward, domain.MergeModeSquash} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			r := newMRRig(t)
			main := r.seed()
			head := r.feature("feat")
			mr := r.open("feat")
			r.drain()

			res, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{Mode: mode, HeadID: head, DeleteSource: true})
			require.NoError(t, err)
			assert.True(t, res.SourceDeleted)
			assert.Equal(t, domain.MRStateMerged, res.State)
			r.mrs.Wait(r.ctx) // the deletion happens after Merge returned
			_, err = r.git.BranchHead(r.ctx, r.repo, "feat")
			assert.ErrorIs(t, err, domain.ErrNotFound, "the branch is gone")

			evs := r.drain()
			require.Len(t, evs, 2)
			assert.Equal(t, []domain.RefUpdate{
				{Old: main.String(), New: res.MergedCommitID.String(), Ref: "refs/heads/main"},
				{Old: head.String(), New: domain.ZeroOID, Ref: "refs/heads/feat"},
			}, evs[1].(domain.PushEvent).Updates)

			// The merged request still reads, with its commits and diff.
			d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
			require.NoError(t, err)
			assert.Len(t, d.Commits, 2)
			assert.Len(t, d.Diff.Files, 2)
		})
	}

	t.Run("without the option the branch stays", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		res, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
		require.NoError(t, err)
		assert.False(t, res.SourceDeleted)
		assert.Equal(t, head, r.branchHead("feat"))
	})
}

// Deleting the source is refused before the merge when the branch is still
// needed: nothing changes.
func TestMergeRefusesToDeleteANeededBranch(t *testing.T) {
	t.Parallel()
	t.Run("another open request uses it as its source", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		main := r.seed()
		head := r.feature("feat")
		r.sh("checkout", "-q", "-B", "release", "main")
		r.push("release")
		mr := r.open("feat")
		_, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "to release", Source: "feat", Target: "release"})
		require.NoError(t, err)

		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: true})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "!2 still uses")
		assert.Equal(t, main, r.branchHead("main"), "not merged")
		assert.Equal(t, head, r.branchHead("feat"))
		d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
		require.NoError(t, err)
		assert.Equal(t, domain.MRStateOpen, d.MergeRequest.State)
	})

	t.Run("another open request targets it", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		r.sh("checkout", "-q", "-B", "sub", "feat")
		r.commit(author, "d.txt", "dee\n", "add d")
		r.push("sub")
		_, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "stacked", Source: "sub", Target: "feat"})
		require.NoError(t, err)
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: true})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Equal(t, head, r.branchHead("feat"))
	})

	t.Run("the default branch", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		main := r.seed()
		r.sh("checkout", "-q", "-B", "old", "main~1")
		r.push("old")
		old := r.branchHead("old")
		mr, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "back", Source: "main", Target: "old"})
		require.NoError(t, err)
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: main, DeleteSource: true})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "default branch")
		assert.Equal(t, old, r.branchHead("old"), "not merged")
		assert.Equal(t, main, r.branchHead("main"))
	})
}

// After a squash with deletion, gc may prune the reviewed commits: the merged
// request still reads, without commits or diff.
func TestSquashMergedRequestReadsAfterItsCommitsArePruned(t *testing.T) {
	t.Parallel()
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	res, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{Mode: domain.MergeModeSquash, HeadID: head, DeleteSource: true})
	require.NoError(t, err)
	require.True(t, res.SourceDeleted)

	r.run(r.bare(), r.env(author), "reflog", "expire", "--expire=now", "--all")
	r.run(r.bare(), r.env(author), "gc", "-q", "--prune=now")
	ok, err := r.git.HasCommit(r.ctx, r.repo, head)
	require.NoError(t, err)
	require.False(t, ok, "the reviewed commits are pruned")

	d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, domain.MRStateMerged, d.MergeRequest.State)
	assert.Equal(t, res.MergedCommitID, d.MergeRequest.MergedCommitID)
	assert.Empty(t, d.Commits)
	assert.Empty(t, d.Diff.Files)
	assert.True(t, d.DiffBaseID.IsZero())
}

// A request opened on a branch a merge is deleting waits for the merge lock
// and then sees the branch is gone.
func TestCreateAfterAMergeDeletedTheBranch(t *testing.T) {
	t.Parallel()
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	_, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: true})
	require.NoError(t, err)
	r.mrs.Wait(r.ctx) // the deletion runs after Merge returned
	_, err = r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "late", Source: "feat"})
	require.ErrorIs(t, err, domain.ErrInvalid)
	assert.Contains(t, fieldsOf(t, err), "source_branch")
}

// movingGit pushes to the source branch between the merge and the deletion.
type movingGit struct {
	*gitcli.Git
	move func()
}

func (g movingGit) DeleteBranch(ctx context.Context, repo domain.RepoName, branch string, old domain.ObjectID) error {
	g.move()
	return g.Git.DeleteBranch(ctx, repo, branch, old)
}

// A branch that moves after the merge is kept: the merge stands.
func TestMergeKeepsASourceThatMovedAfterTheMerge(t *testing.T) {
	t.Parallel()
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	var moved domain.ObjectID
	mrs := usecase.NewMergeRequests(usecase.MergeRequestsConfig{
		Meta: r.store, Store: r.store.MergeRequests(), Repos: r.git, Bus: r.bus, Clock: fixedClock{}, Maint: r.sched,
		Git: movingGit{Git: r.git, move: func() {
			moved = r.commit(author, "late.txt", "late\n", "pushed during the merge")
			r.push("feat")
		}},
	})
	r.drain()

	res, err := mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: true})
	require.NoError(t, err, "the merge stands")
	assert.True(t, res.SourceDeleted, "the deletion was requested")
	assert.Equal(t, domain.MRStateMerged, res.State)
	mrs.Wait(r.ctx)
	assert.Equal(t, head, r.branchHead("main"))
	assert.Equal(t, moved, r.branchHead("feat"), "the late commit is kept")
	evs := r.drain()
	require.Len(t, evs, 2)
	assert.Len(t, evs[1].(domain.PushEvent).Updates, 1, "only the target moved")
}

// afterMergeRig builds merge request use cases over the rig with ref-tip
// records, whose post-merge goroutine waits for the returned release function.
func (r *mrRig) afterMergeRig(extra func(*usecase.MergeRequestsConfig)) (mrs *usecase.MergeRequests, release func()) {
	gate := make(chan struct{})
	cfg := usecase.MergeRequestsConfig{
		Meta: r.store, Store: r.store.MergeRequests(), Repos: r.git, Git: r.git, Signer: nil,
		Committer: domain.Identity{Name: "ermite", Email: "ermite@git.example.org"},
		Bus:       r.bus, Clock: fixedClock{}, Maint: r.sched, RefTips: r.store.CI(),
	}
	if extra != nil {
		extra(&cfg)
	}
	mrs = usecase.NewMergeRequests(cfg)
	mrs.SetAfterMergeGate(func() { <-gate })
	var once sync.Once
	return mrs, func() { once.Do(func() { close(gate) }) }
}

// A request opened on the source after the merge, before the deletion runs,
// keeps the branch (GO-002).
func TestMergeKeepsTheSourceWhenAnotherRequestOpensOnItMeanwhile(t *testing.T) {
	t.Parallel()
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	r.sh("checkout", "-q", "-B", "release", "main")
	r.push("release")
	mrs, release := r.afterMergeRig(nil)
	defer release()

	_, err := mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: true})
	require.NoError(t, err)
	_, err = mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "to release", Source: "feat", Target: "release"})
	require.NoError(t, err, "the merge released the lock; the deletion has not run")
	release()
	mrs.Wait(r.ctx)
	assert.Equal(t, head, r.branchHead("feat"), "the branch is kept")
}

// A push that moves the source after the merge keeps both the branch and the
// record of who pushed it (GO-003).
func TestMergeKeepsTheSourceAndItsRecordWhenAPushMovesItMeanwhile(t *testing.T) {
	t.Parallel()
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	mrs, release := r.afterMergeRig(nil)
	defer release()

	_, err := mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: true})
	require.NoError(t, err)
	r.sh("checkout", "-q", "feat")
	moved := r.commit(author, "later.txt", "later\n", "pushed meanwhile")
	r.push("feat")
	require.NoError(t, r.store.CI().PutRefTip(r.ctx, domain.CIRefTip{Repo: r.repo, Ref: "refs/heads/feat", Sha: moved.String(), Actor: "ada"}))
	release()
	mrs.Wait(r.ctx)

	assert.Equal(t, moved, r.branchHead("feat"), "the branch is kept")
	tip, err := r.store.CI().GetRefTip(r.ctx, r.repo, "refs/heads/feat")
	require.NoError(t, err, "the record of the new push is kept")
	assert.Equal(t, moved.String(), tip.Sha)
}

// The record of the source goes with the branch, once it is deleted.
func TestMergeRemovesTheSourceRecordAfterDeletingIt(t *testing.T) {
	t.Parallel()
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	mrs, release := r.afterMergeRig(nil)
	release()
	require.NoError(t, r.store.CI().PutRefTip(r.ctx, domain.CIRefTip{Repo: r.repo, Ref: "refs/heads/feat", Sha: head.String(), Actor: "ada"}))

	_, err := mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: true})
	require.NoError(t, err)
	mrs.Wait(r.ctx)
	_, err = r.git.BranchHead(r.ctx, r.repo, "feat")
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = r.store.CI().GetRefTip(r.ctx, r.repo, "refs/heads/feat")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// panicHooks is a CI trigger whose merge hook panics.
type panicHooks struct{}

func (panicHooks) OnPush(context.Context, domain.RepoName, []domain.RefUpdate, domain.Principal) {}
func (panicHooks) OnMerge(context.Context, domain.RepoName, []domain.RefUpdate, domain.Principal) {
	panic("boom from ci")
}

func TestMergeLogsAPanickingSideEffectAndWaitReturns(t *testing.T) {
	t.Parallel()
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	mrs, release := r.afterMergeRig(func(c *usecase.MergeRequestsConfig) { c.CI = panicHooks{} })
	release()
	var logs bytes.Buffer
	ctx := zerowrap.WithCtx(r.ctx, zerowrap.New(zerowrap.Config{Level: "info", Format: "json", Output: &logs}))

	_, err := mrs.Merge(ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
	require.NoError(t, err)
	waited := make(chan struct{})
	go func() { mrs.Wait(context.Background()); close(waited) }()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after a panicking side effect")
	}
	assert.Contains(t, logs.String(), "post-merge side effects panicked")
	assert.Contains(t, logs.String(), "boom from ci")
}

// A head behind the target has nothing to merge, whatever the mode (GO-007).
func TestMergeRefusesAHeadBehindTheTarget(t *testing.T) {
	t.Parallel()
	for _, mode := range []domain.MergeMode{domain.MergeModeSquash, domain.MergeModeMerge, domain.MergeModeFastForward} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			r := newMRRig(t)
			main := r.seed()
			r.sh("checkout", "-q", "-B", "old", "main~1")
			r.push("old")
			old := r.branchHead("old")
			require.NotEqual(t, main, old)
			mr := r.open("old")

			_, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{Mode: mode, HeadID: old})
			require.ErrorIs(t, err, domain.ErrPreconditionFailed)
			assert.Contains(t, err.Error(), "nothing to merge")
			assert.Equal(t, main, r.branchHead("main"), "no commit was made")
		})
	}
}
