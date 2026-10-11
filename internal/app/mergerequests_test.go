// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	appmocks "bnema.dev/ermite/internal/app/mocks"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

type mrUnit struct {
	meta  *mocks.RepoMetaStore
	store *mocks.MergeRequestStore
	repos *mocks.GitRepos
	git   *mocks.MergeGit
	mrs   *app.MergeRequests
	repo  domain.Repo
}

func newMRUnit(t *testing.T) *mrUnit {
	t.Helper()
	u := &mrUnit{
		meta:  mocks.NewRepoMetaStore(t),
		store: mocks.NewMergeRequestStore(t),
		repos: mocks.NewGitRepos(t),
		git:   mocks.NewMergeGit(t),
		repo:  domain.Repo{Name: name(t, "proj"), Visibility: domain.VisibilityPrivate, Settings: domain.DefaultRepoSettings()},
	}
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	sched := appmocks.NewMaintenanceScheduler(t)
	sched.EXPECT().Schedule(mock.Anything, mock.Anything).Return().Maybe()
	u.mrs = app.NewMergeRequests(app.MergeRequestsConfig{
		Meta: u.meta, Store: u.store, Repos: u.repos, Git: u.git,
		Signer: mocks.NewCommitSigner(t), Bus: mocks.NewEventBus(t), Clock: clock, Maint: sched,
	})
	return u
}

// GO-004: a failure to read Git is an error for the log, not a field violation
// that would show its text to the caller.
func TestCreateReturnsGitFailuresAsErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("fatal: /srv/ermite/repos/proj.git: permission denied")
	params := app.CreateMergeRequestParams{Title: "t", Source: "feat", Target: "main"}

	t.Run("reading a branch", func(t *testing.T) {
		t.Parallel()
		u := newMRUnit(t)
		u.meta.EXPECT().Get(mock.Anything, u.repo.Name).Return(u.repo, nil)
		u.git.EXPECT().BranchHead(mock.Anything, u.repo.Name, "feat").Return(domain.ObjectID{}, boom)
		_, err := u.mrs.Create(context.Background(), domain.Root(), u.repo.Name, params)
		require.ErrorIs(t, err, boom)
		var fe *domain.InvalidFieldsError
		assert.False(t, errors.As(err, &fe), "not a field violation: %v", err)
		assert.NotErrorIs(t, err, domain.ErrInvalid)
	})

	t.Run("reading the default branch", func(t *testing.T) {
		t.Parallel()
		u := newMRUnit(t)
		u.meta.EXPECT().Get(mock.Anything, u.repo.Name).Return(u.repo, nil)
		u.repos.EXPECT().DefaultBranch(mock.Anything, u.repo.Name).Return("", false, boom)
		_, err := u.mrs.Create(context.Background(), domain.Root(), u.repo.Name, app.CreateMergeRequestParams{Title: "t", Source: "feat"})
		require.ErrorIs(t, err, boom)
		var fe *domain.InvalidFieldsError
		assert.False(t, errors.As(err, &fe), "not a field violation: %v", err)
	})

	t.Run("a branch that does not exist stays a violation", func(t *testing.T) {
		t.Parallel()
		u := newMRUnit(t)
		u.meta.EXPECT().Get(mock.Anything, u.repo.Name).Return(u.repo, nil)
		u.git.EXPECT().BranchHead(mock.Anything, u.repo.Name, "feat").Return(domain.ObjectID{}, domain.ErrNotFound)
		u.git.EXPECT().BranchHead(mock.Anything, u.repo.Name, "main").Return(someID(t), nil)
		_, err := u.mrs.Create(context.Background(), domain.Root(), u.repo.Name, params)
		assert.Contains(t, fieldsOf(t, err), "source_branch")
	})
}

func someID(t *testing.T) domain.ObjectID {
	t.Helper()
	return oidFrom(t, oidA)
}

// GO-003: the settings are read again under the merge lock; a failure of that
// read is returned, and a repository deleted meanwhile is not found.
func TestMergeReturnsTheFailureToReadTheSettings(t *testing.T) {
	t.Parallel()
	boom := errors.New("kv: closed")
	for label, tc := range map[string]struct{ err, want error }{
		"read failure":      {boom, boom},
		"deleted meanwhile": {domain.ErrNotFound, domain.ErrNotFound},
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			u := newMRUnit(t)
			u.meta.EXPECT().Get(mock.Anything, u.repo.Name).Return(u.repo, nil).Once()
			u.meta.EXPECT().Get(mock.Anything, u.repo.Name).Return(domain.Repo{}, tc.err).Once()
			u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(domain.MergeRequest{
				Repo: u.repo.Name, Number: 1, State: domain.MRStateOpen, Source: "feat", Target: "main",
			}, nil)
			_, err := u.mrs.Merge(context.Background(), domain.Root(), u.repo.Name, 1, app.MergeParams{HeadID: someID(t)})
			require.ErrorIs(t, err, tc.want)
			if tc.want == boom {
				assert.NotErrorIs(t, err, domain.ErrNotFound)
			}
		})
	}
}
