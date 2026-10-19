// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

type mrUnit struct {
	meta  *mocks.RepoMetaStore
	store *mocks.MergeRequestStore
	repos *mocks.GitRepos
	git   *mocks.MergeGit
	mrs   *usecase.MergeRequests
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
	sched := mocks.NewMaintenanceScheduler(t)
	sched.EXPECT().Schedule(mock.Anything, mock.Anything).Return().Maybe()
	u.mrs = usecase.NewMergeRequests(usecase.MergeRequestsConfig{
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
	params := ports.CreateMergeRequestParams{Title: "t", Source: "feat", Target: "main"}

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
		_, err := u.mrs.Create(context.Background(), domain.Root(), u.repo.Name, ports.CreateMergeRequestParams{Title: "t", Source: "feat"})
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
			_, err := u.mrs.Merge(context.Background(), domain.Root(), u.repo.Name, 1, ports.MergeParams{HeadID: someID(t)})
			require.ErrorIs(t, err, tc.want)
			if tc.want == boom {
				assert.NotErrorIs(t, err, domain.ErrNotFound)
			}
		})
	}
}

func TestNoteMutationRetries(t *testing.T) {
	for _, action := range []string{"edit", "delete", "resolve"} {
		t.Run(action, func(t *testing.T) {
			u := newMRUnit(t)
			u.meta.EXPECT().Get(mock.Anything, u.repo.Name).Return(u.repo, nil)
			mr := domain.MergeRequest{Repo: u.repo.Name, Number: 1, State: domain.MRStateOpen}
			text, err := domain.ParsePlainText("before")
			require.NoError(t, err)
			c := domain.Comment{ID: 1, Author: "root", Text: text}
			u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(mr, nil).Twice()
			u.store.EXPECT().GetComment(mock.Anything, u.repo.Name, int64(1), int64(1)).Return(c, nil).Twice()
			u.store.EXPECT().SaveComment(mock.Anything, mock.Anything, mock.Anything).Return(domain.ErrConflict).Once()
			u.store.EXPECT().SaveComment(mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
			// The bus expectation must be exactly once despite the conflict.
			bus := mocks.NewEventBus(t)
			bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(e domain.Event) bool {
				return e.EventKind() == "mr.comment_"+map[string]string{"edit": "edited", "delete": "deleted", "resolve": "resolved"}[action]
			})).Return().Once()
			clock := mocks.NewClock(t)
			clock.EXPECT().Now().Return(now)
			svc := usecase.NewMergeRequests(usecase.MergeRequestsConfig{Meta: u.meta, Store: u.store, Bus: bus, Clock: clock})
			switch action {
			case "edit":
				_, err = svc.EditComment(context.Background(), domain.Root(), u.repo.Name, 1, 1, "after")
			case "delete":
				_, err = svc.DeleteComment(context.Background(), domain.Root(), u.repo.Name, 1, 1)
			case "resolve":
				_, err = svc.ResolveComment(context.Background(), domain.Root(), u.repo.Name, 1, 1, true)
			}
			require.NoError(t, err)
		})
	}
}

func TestAddNoteValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  ports.AddCommentParams
		target  domain.Comment
		missing bool
	}{
		{name: "range without path", params: ports.AddCommentParams{StartLine: 1, Text: "x"}},
		{name: "equal ends", params: ports.AddCommentParams{Path: "a", StartLine: 3, Line: 3, Side: domain.SideNew, Text: "x"}},
		{name: "reversed ends", params: ports.AddCommentParams{Path: "a", StartLine: 4, Line: 3, Side: domain.SideNew, Text: "x"}},
		{name: "reply anchor", params: ports.AddCommentParams{ReplyTo: 1, Path: "a", Line: 1, Side: domain.SideNew, Text: "x"}},
		{name: "reply verdict", params: ports.AddCommentParams{ReplyTo: 1, Verdict: domain.VerdictApprove}},
		{name: "reply to reply", params: ports.AddCommentParams{ReplyTo: 1, Text: "x"}, target: domain.Comment{ReplyTo: 2}},
		{name: "deleted root", params: ports.AddCommentParams{ReplyTo: 1, Text: "x"}, target: domain.Comment{Deleted: true}},
		{name: "missing root", params: ports.AddCommentParams{ReplyTo: 1, Text: "x"}, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newMRUnit(t)
			u.meta.EXPECT().Get(mock.Anything, u.repo.Name).Return(u.repo, nil)
			u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(domain.MergeRequest{State: domain.MRStateOpen}, nil)
			if tc.name == "reply to reply" || tc.name == "deleted root" || tc.missing {
				var err error
				if tc.missing {
					err = domain.ErrNotFound
				}
				u.store.EXPECT().GetComment(mock.Anything, u.repo.Name, int64(1), int64(1)).Return(tc.target, err)
			}
			_, err := u.mrs.AddComment(context.Background(), domain.Root(), u.repo.Name, 1, tc.params)
			require.ErrorIs(t, err, domain.ErrInvalid)
		})
	}
}
