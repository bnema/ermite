// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
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

			res, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, app.MergeParams{Mode: mode, HeadID: head, DeleteSource: true})
			require.NoError(t, err)
			assert.True(t, res.SourceDeleted)
			assert.Equal(t, domain.MRStateMerged, res.State)
			_, err = r.git.BranchHead(r.ctx, r.repo, "feat")
			assert.ErrorIs(t, err, domain.ErrNotFound, "the branch is gone")

			evs := r.drain()
			require.Len(t, evs, 2)
			assert.Equal(t, []domain.RefUpdate{
				{Old: main.String(), New: res.MergedCommitID.String(), Ref: "refs/heads/main"},
				{Old: head.String(), New: domain.ZeroOID, Ref: "refs/heads/feat"},
			}, evs[1].(domain.PushEvent).Updates)

			// The merged request still reads, with its commits and diff.
			d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, false)
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
		res, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, app.MergeParams{HeadID: head})
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
		_, err := r.mrs.Create(r.ctx, root, r.repo, app.CreateMergeRequestParams{Title: "to release", Source: "feat", Target: "release"})
		require.NoError(t, err)

		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, app.MergeParams{HeadID: head, DeleteSource: true})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "!2 still uses")
		assert.Equal(t, main, r.branchHead("main"), "not merged")
		assert.Equal(t, head, r.branchHead("feat"))
		d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, false)
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
		_, err := r.mrs.Create(r.ctx, root, r.repo, app.CreateMergeRequestParams{Title: "stacked", Source: "sub", Target: "feat"})
		require.NoError(t, err)
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, app.MergeParams{HeadID: head, DeleteSource: true})
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
		mr, err := r.mrs.Create(r.ctx, root, r.repo, app.CreateMergeRequestParams{Title: "back", Source: "main", Target: "old"})
		require.NoError(t, err)
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, app.MergeParams{HeadID: main, DeleteSource: true})
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
	res, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, app.MergeParams{Mode: domain.MergeModeSquash, HeadID: head, DeleteSource: true})
	require.NoError(t, err)
	require.True(t, res.SourceDeleted)

	r.run(r.bare(), r.env(author), "reflog", "expire", "--expire=now", "--all")
	r.run(r.bare(), r.env(author), "gc", "-q", "--prune=now")
	ok, err := r.git.HasCommit(r.ctx, r.repo, head)
	require.NoError(t, err)
	require.False(t, ok, "the reviewed commits are pruned")

	d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, false)
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
	_, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, app.MergeParams{HeadID: head, DeleteSource: true})
	require.NoError(t, err)
	_, err = r.mrs.Create(r.ctx, root, r.repo, app.CreateMergeRequestParams{Title: "late", Source: "feat"})
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
	mrs := app.NewMergeRequests(app.MergeRequestsConfig{
		Meta: r.store, Store: r.store.MergeRequests(), Repos: r.git, Bus: r.bus, Clock: fixedClock{}, Maint: r.sched,
		Git: movingGit{Git: r.git, move: func() {
			moved = r.commit(author, "late.txt", "late\n", "pushed during the merge")
			r.push("feat")
		}},
	})
	r.drain()

	res, err := mrs.Merge(r.ctx, mrM, r.repo, mr.Number, app.MergeParams{HeadID: head, DeleteSource: true})
	require.NoError(t, err, "the merge stands")
	assert.False(t, res.SourceDeleted)
	assert.Equal(t, domain.MRStateMerged, res.State)
	assert.Equal(t, head, r.branchHead("main"))
	assert.Equal(t, moved, r.branchHead("feat"), "the late commit is kept")
	evs := r.drain()
	require.Len(t, evs, 2)
	assert.Len(t, evs[1].(domain.PushEvent).Updates, 1, "only the target moved")
}
