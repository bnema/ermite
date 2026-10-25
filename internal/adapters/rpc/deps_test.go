// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestRepoDepsToPB(t *testing.T) {
	repo, err := domain.ParseRepoName("app")
	require.NoError(t, err)
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	got := repoDepsToPB(domain.RepoDeps{
		Repo: repo, CheckedAt: at, State: domain.DepsFailed, Error: "boom", MR: 7, RunID: 12,
		Updates: []domain.DepUpdate{{
			DepRef:  domain.DepRef{Ecosystem: domain.EcosystemActions, Directory: ".github/workflows/ci.yml", Name: "actions/checkout", Version: "v5"},
			Current: "v4", Kind: domain.DepMajor, New: true,
		}},
	})
	assert.Equal(t, "app", got.Repo)
	assert.Equal(t, pb.RepoDependencyUpdates_STATE_FAILED, got.State)
	assert.Equal(t, "boom", got.Error)
	assert.EqualValues(t, 7, got.MergeRequestNumber)
	assert.EqualValues(t, 12, got.RunId)
	assert.Zero(t, repoDepsToPB(domain.RepoDeps{}).RunId)
	assert.True(t, got.CheckedAt.AsTime().Equal(at))
	assert.Nil(t, got.SnoozedUntil)
	require.Len(t, got.Updates, 1)
	u := got.Updates[0]
	assert.Equal(t, "github-actions", u.Ecosystem)
	assert.Equal(t, "v5", u.Latest)
	assert.Equal(t, "v4", u.Current)
	assert.Equal(t, pb.DependencyUpdate_KIND_MAJOR, u.Kind)
	assert.True(t, u.New)

	assert.Equal(t, pb.RepoDependencyUpdates_STATE_IDLE, repoDepsToPB(domain.RepoDeps{}).State)
	assert.Equal(t, pb.RepoDependencyUpdates_STATE_APPLYING, depsStateToPB(domain.DepsApplying))
	assert.Equal(t, pb.DependencyUpdate_KIND_PATCH, depKindToPB(domain.DepPatch))
	assert.Equal(t, pb.DependencyUpdate_KIND_MINOR, depKindToPB(domain.DepMinor))
}

func TestDepsExpectedVersionsAndErrors(t *testing.T) {
	repo, err := domain.ParseRepoName("app")
	require.NoError(t, err)
	svc := mocks.NewDepsService(t)
	s := &depsService{svc}
	ctx := context.Background()
	want := []domain.DepRef{{Ecosystem: domain.EcosystemGoMod, Directory: "sub", Name: "x/y", Version: "v1.2.3"}}
	svc.EXPECT().Ignore(mock.Anything, mock.Anything, repo, want).Return(domain.RepoDeps{Repo: repo}, nil).Once()
	res, err := s.IgnoreDependencyUpdates(ctx, connect.NewRequest(&pb.IgnoreDependencyUpdatesRequest{
		Repo: "app", ExpectedVersions: []*pb.DependencyVersion{{Ecosystem: "gomod", Directory: "sub", Name: "x/y", Version: "v1.2.3"}},
	}))
	require.NoError(t, err)
	assert.Equal(t, "app", res.Msg.Updates.Repo)

	svc.EXPECT().Open(mock.Anything, mock.Anything, repo, []domain.DepRef{}).Return(domain.RepoDeps{}, domain.ErrPreconditionFailed).Once()
	_, err = s.OpenDependencyUpdates(ctx, connect.NewRequest(&pb.OpenDependencyUpdatesRequest{Repo: "app"}))
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))

	svc.EXPECT().Check(mock.Anything, mock.Anything, repo).Return(domain.RepoDeps{}, domain.ErrNotFound).Once()
	_, err = s.CheckDependencies(ctx, connect.NewRequest(&pb.CheckDependenciesRequest{Repo: "app"}))
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err), "a non-owner gets not found")

	svc.EXPECT().Snooze(mock.Anything, mock.Anything, repo).Return(domain.RepoDeps{}, domain.ErrNotFound).Once()
	_, err = s.SnoozeDependencyUpdates(ctx, connect.NewRequest(&pb.SnoozeDependencyUpdatesRequest{Repo: "app"}))
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))

	svc.EXPECT().List(mock.Anything, mock.Anything, domain.RepoName{}).Return(nil, domain.ErrNotFound).Once()
	_, err = s.ListDependencyUpdates(ctx, connect.NewRequest(&pb.ListDependencyUpdatesRequest{}))
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))

	_, err = s.CheckDependencies(ctx, connect.NewRequest(&pb.CheckDependenciesRequest{Repo: "../bad"}))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func TestDepsListWithRepoFilter(t *testing.T) {
	repo, err := domain.ParseRepoName("app")
	require.NoError(t, err)
	svc := mocks.NewDepsService(t)
	svc.EXPECT().List(mock.Anything, mock.Anything, repo).Return([]domain.RepoDeps{{Repo: repo}}, nil).Once()
	r := "app"
	res, err := (&depsService{svc}).ListDependencyUpdates(context.Background(), connect.NewRequest(&pb.ListDependencyUpdatesRequest{Repo: &r}))
	require.NoError(t, err)
	require.Len(t, res.Msg.Repos, 1)
}
