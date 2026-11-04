// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

// fakeHold holds one head of one branch.
type fakeHold struct {
	branch string
	sha    domain.ObjectID
	calls  int
}

func (h *fakeHold) Held(_ context.Context, _ domain.RepoName, branch string, sha domain.ObjectID) bool {
	h.calls++
	return branch == h.branch && sha == h.sha
}

var (
	heldSHA  = mustOID("1111111111111111111111111111111111111111")
	otherSHA = mustOID("2222222222222222222222222222222222222222")
	parentSH = mustOID("3333333333333333333333333333333333333333")
)

// The planning of a push that would start runs reads the repository first
// (handleRef): a "deleted meanwhile" there ends it without a run, so whether
// Meta.Get is called tells whether runs would have started.
func TestHoldSkipsPushRunsOfTheHeldHeadOnly(t *testing.T) {
	u := newCIUnit(t, nil)
	repo := name(t, "proj")
	hold := &fakeHold{branch: "deps", sha: heldSHA}
	u.ci.SetHold(hold)
	ctx := context.Background()
	u.store.EXPECT().ListRunners(mock.Anything).Return([]domain.CIRunner{{ID: "r1"}}, nil)

	// Held: nothing past the hold (an unexpected Meta.Get fails the mock).
	u.ci.OnPush(ctx, repo, []domain.RefUpdate{{Ref: "refs/heads/deps", Old: parentSH.String(), New: heldSHA.String()}}, domain.Root())
	u.ci.PlanQueuedPush()
	assert.Equal(t, 1, hold.calls)

	// Another head of the same branch is not held: planning goes on.
	u.meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{}, domain.ErrNotFound).Once()
	u.ci.OnPush(ctx, repo, []domain.RefUpdate{{Ref: "refs/heads/deps", Old: heldSHA.String(), New: otherSHA.String()}}, domain.Root())
	u.ci.PlanQueuedPush()
	assert.Equal(t, 2, hold.calls)
}

// startPullRuns lists the runners first: no ListRunners call means no run.
func TestHoldSkipsPullRequestRunsOfTheHeldHead(t *testing.T) {
	u := newCIUnit(t, nil)
	repo := name(t, "proj")
	u.ci.SetHold(&fakeHold{branch: "deps", sha: heldSHA})
	ctx := context.Background()
	mr := domain.MergeRequest{Repo: repo, Number: 1, Source: "deps", Target: "main", State: domain.MRStateOpen}

	for _, action := range []string{domain.PullOpened, domain.PullSynchronize, domain.PullReopened} {
		require.NoError(t, u.ci.StartPullRunsForTest(ctx, mr, action, heldSHA), action)
	}
	u.store.EXPECT().ListRunners(mock.Anything).Return(nil, nil).Once()
	require.NoError(t, u.ci.StartPullRunsForTest(ctx, mr, domain.PullOpened, otherSHA))
}

func TestReleaseHoldStartsPushAndSynchronizeRuns(t *testing.T) {
	mrs := mocks.NewMergeRequestStore(t)
	git := mocks.NewMergeGit(t)
	repos := mocks.NewGitRepos(t)
	u := newCIUnit(t, func(cfg *usecase.CIConfig) {
		cfg.MRs, cfg.Pull, cfg.Git, cfg.Repos = mrs, mocks.NewPullGit(t), git, repos
	})
	repo := name(t, "proj")
	hold := &fakeHold{branch: "deps", sha: heldSHA}
	u.ci.SetHold(hold)
	ctx := context.Background()
	mr := domain.MergeRequest{Repo: repo, Number: 1, Source: "deps", Target: "main", State: domain.MRStateOpen}

	git.EXPECT().BranchHead(mock.Anything, repo, "deps").Return(heldSHA, nil)
	git.EXPECT().BranchHead(mock.Anything, repo, "main").Return(parentSH, nil)
	git.EXPECT().ListRange(mock.Anything, repo, domain.ObjectID{}, heldSHA, 1).
		Return([]domain.Commit{{ID: heldSHA, Parents: []domain.ObjectID{parentSH}}}, false, nil)
	repos.EXPECT().DefaultBranch(mock.Anything, repo).Return("main", true, nil)
	mrs.EXPECT().OpenByTarget(mock.Anything, repo, []string{"deps"}, mock.Anything).Return(nil, false, nil)
	mrs.EXPECT().OpenBySource(mock.Anything, repo, []string{"deps"}, mock.Anything).Return([]int64{1}, false, nil)
	mrs.EXPECT().Get(mock.Anything, repo, int64(1)).Return(mr, nil)
	require.NoError(t, u.ci.ReleaseHold(ctx, repo, "deps", heldSHA))

	// The push runs: planning reaches the repository despite the hold.
	u.store.EXPECT().ListRunners(mock.Anything).Return([]domain.CIRunner{{ID: "r1"}}, nil).Once()
	u.meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{}, domain.ErrNotFound).Once()
	u.ci.PlanQueuedPush()

	// The synchronize runs of the request from the branch, despite the hold.
	u.store.EXPECT().ListRunners(mock.Anything).Return(nil, nil).Once()
	action, err := u.ci.StartQueuedPullRuns(ctx, mr)
	require.NoError(t, err)
	assert.Equal(t, domain.PullSynchronize, action)
	assert.Zero(t, hold.calls, "a release does not ask the hold")
}

func TestReleaseHoldOfAMovedBranchStartsNothing(t *testing.T) {
	git := mocks.NewMergeGit(t)
	u := newCIUnit(t, func(cfg *usecase.CIConfig) { cfg.Git = git })
	repo := name(t, "proj")
	git.EXPECT().BranchHead(mock.Anything, repo, "deps").Return(otherSHA, nil)
	require.NoError(t, u.ci.ReleaseHold(context.Background(), repo, "deps", heldSHA))
	assert.True(t, u.ci.Settled(context.Background(), repo, 1))
}
