// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
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

func inboxMR(t *testing.T, repo string, n int64, up time.Time) domain.MergeRequest {
	t.Helper()
	ti, err := domain.ParseMRTitle("mr")
	require.NoError(t, err)
	return domain.MergeRequest{Repo: name(t, repo), Number: n, Title: ti, State: domain.MRStateOpen, UpdatedAt: up}
}

func inboxIssue(t *testing.T, repo string, n int64, up time.Time) domain.Issue {
	t.Helper()
	ti, err := domain.ParseIssueTitle("issue")
	require.NoError(t, err)
	return domain.Issue{Repo: name(t, repo), Number: n, Title: ti, State: domain.IssueStateOpen, UpdatedAt: up}
}

func newInboxUnit(t *testing.T) (*usecase.Inbox, *mocks.MergeRequestStore, *mocks.IssueStore, *mocks.RepoMetaStore) {
	t.Helper()
	mrs, is, meta := mocks.NewMergeRequestStore(t), mocks.NewIssueStore(t), mocks.NewRepoMetaStore(t)
	return usecase.NewInbox(usecase.InboxConfig{Meta: meta, MergeRequests: mrs, Issues: is}), mrs, is, meta
}

func repoWithIssues(t *testing.T, n string, mode domain.IssuesMode) domain.Repo {
	s := domain.DefaultRepoSettings()
	s.Issues = mode
	return domain.Repo{Name: name(t, n), Visibility: domain.VisibilityPrivate, Settings: s}
}

func TestInboxIsOwnerOnly(t *testing.T) {
	t.Parallel()
	in, _, _, _ := newInboxUnit(t) // the stores are never read
	for _, p := range []domain.Principal{domain.Anonymous(), issuePrincipal(domain.ScopeIssueWrite)} {
		_, err := in.Overview(context.Background(), p, domain.RepoName{})
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}
}

func TestInboxOverviewSortsFiltersAndSkipsIssuesOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in, mrs, is, meta := newInboxUnit(t)
	mrs.EXPECT().ListOpenAll(mock.Anything).Return([]domain.MergeRequest{
		inboxMR(t, "alpha", 1, t0), inboxMR(t, "beta", 1, t0.Add(time.Hour)), inboxMR(t, "gone", 1, t0),
	}, nil)
	is.EXPECT().ListOpenAll(mock.Anything).Return([]domain.Issue{
		inboxIssue(t, "alpha", 2, t0), inboxIssue(t, "beta", 2, t0.Add(2*time.Hour)), inboxIssue(t, "quiet", 2, t0),
	}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "alpha")).Return(repoWithIssues(t, "alpha", domain.IssuesOwner), nil)
	meta.EXPECT().Get(mock.Anything, name(t, "beta")).Return(repoWithIssues(t, "beta", domain.IssuesPublic), nil)
	meta.EXPECT().Get(mock.Anything, name(t, "quiet")).Return(repoWithIssues(t, "quiet", domain.IssuesOff), nil)
	meta.EXPECT().Get(mock.Anything, name(t, "gone")).Return(domain.Repo{}, domain.ErrNotFound)

	got, err := in.Overview(ctx, domain.Owner(), domain.RepoName{})
	require.NoError(t, err)
	assert.Equal(t, ports.InboxCounts{MergeRequests: 2, Issues: 2}, got.Counts)
	require.Len(t, got.MergeRequests, 2)
	assert.Equal(t, "beta", got.MergeRequests[0].Repo.String(), "most recently updated first")
	require.Len(t, got.Issues, 2)
	assert.Equal(t, "beta", got.Issues[0].Repo.String())
	assert.Equal(t, []domain.RepoName{name(t, "alpha"), name(t, "beta")}, got.Repos)

	got, err = in.Overview(ctx, domain.Root(), name(t, "alpha"))
	require.NoError(t, err)
	assert.Equal(t, ports.InboxCounts{MergeRequests: 2, Issues: 2}, got.Counts, "counts ignore the filter")
	require.Len(t, got.MergeRequests, 1)
	require.Len(t, got.Issues, 1)
	assert.Equal(t, "alpha", got.Issues[0].Repo.String())
	assert.Len(t, got.Repos, 2, "the filter list keeps every repository")
}

func TestInboxListsPendingIssuesWithoutTheirHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mrs, is, meta, pending := mocks.NewMergeRequestStore(t), mocks.NewIssueStore(t), mocks.NewRepoMetaStore(t), mocks.NewPendingIssueStore(t)
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, MergeRequests: mrs, Issues: is, Pending: pending})
	mrs.EXPECT().ListOpenAll(mock.Anything).Return(nil, nil)
	is.EXPECT().ListOpenAll(mock.Anything).Return(nil, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "on")).Return(repoWithIssues(t, "on", domain.IssuesPublic), nil)
	meta.EXPECT().Get(mock.Anything, name(t, "off")).Return(repoWithIssues(t, "off", domain.IssuesOff), nil)
	ti, err := domain.ParseIssueTitle("hi")
	require.NoError(t, err)
	pending.EXPECT().List(mock.Anything, domain.RepoName{}, mock.Anything).Return(ports.PendingIssuePage{Issues: []domain.PendingIssue{
		{ID: "a", Repo: name(t, "on"), Title: ti, IPHash: []byte("secret")},
		{ID: "b", Repo: name(t, "off"), Title: ti},
	}}, nil)
	ov, err := in.Overview(ctx, domain.Owner(), name(t, "other"))
	require.NoError(t, err)
	assert.Equal(t, 1, ov.Counts.Pending, "issues off hides pending ones too")
	require.Len(t, ov.Pending, 1)
	assert.Nil(t, ov.Pending[0].IPHash)
	assert.Equal(t, []domain.RepoName{name(t, "on")}, ov.Repos)
}

func TestInboxRuns(t *testing.T) {
	t.Parallel()
	mrs, issues, meta, ci := mocks.NewMergeRequestStore(t), mocks.NewIssueStore(t), mocks.NewRepoMetaStore(t), mocks.NewCIStore(t)
	mrs.EXPECT().ListOpenAll(mock.Anything).Return(nil, nil)
	issues.EXPECT().ListOpenAll(mock.Anything).Return(nil, nil)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	alpha, beta := name(t, "alpha"), name(t, "beta")
	ci.EXPECT().ListOpenRuns(mock.Anything).Return([]domain.CIRun{
		{Repo: alpha, ID: 1, Status: domain.CIQueued, CreatedAt: now},
		{Repo: beta, ID: 2, Status: domain.CIInProgress, CreatedAt: now.Add(time.Hour)},
		{Repo: alpha, ID: 3, Status: domain.CICompleted, CreatedAt: now.Add(2 * time.Hour)},
		{Repo: name(t, "gone"), ID: 4, Status: domain.CIQueued, CreatedAt: now},
	}, nil)
	for _, n := range []string{"alpha", "beta"} {
		meta.EXPECT().Get(mock.Anything, name(t, n)).Return(repoWithIssues(t, n, domain.IssuesOff), nil)
	}
	meta.EXPECT().Get(mock.Anything, name(t, "gone")).Return(domain.Repo{}, domain.ErrNotFound)
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, MergeRequests: mrs, Issues: issues, CI: ci})
	all, err := in.Overview(t.Context(), domain.Owner(), domain.RepoName{})
	require.NoError(t, err)
	assert.True(t, all.RunsEnabled)
	assert.Equal(t, 2, all.Counts.Running)
	require.Len(t, all.Runs, 2)
	assert.Equal(t, int64(2), all.Runs[0].ID)
	assert.Equal(t, int64(1), all.Runs[1].ID)
	assert.Equal(t, []domain.RepoName{alpha, beta}, all.Repos)
	filtered, err := in.Overview(t.Context(), domain.Owner(), alpha)
	require.NoError(t, err)
	assert.Equal(t, 2, filtered.Counts.Running)
	require.Len(t, filtered.Runs, 1)
	assert.Equal(t, alpha, filtered.Runs[0].Repo)
}

func TestInboxDepsSkipsSnoozedArchivedAndEmpty(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	upd := []domain.DepUpdate{{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "x/y", Version: "v1.1.0"}, Current: "v1.0.0", Kind: domain.DepMinor}}
	rec := func(repo string) domain.RepoDeps { return domain.RepoDeps{Repo: name(t, repo), Updates: upd} }
	snoozed, expired, empty := rec("snoozed"), rec("expired"), domain.RepoDeps{Repo: name(t, "empty")}
	snoozed.SnoozedUntil = now.Add(time.Hour)
	expired.SnoozedUntil = now.Add(-time.Hour)

	mrs, is, meta, store, clock := mocks.NewMergeRequestStore(t), mocks.NewIssueStore(t), mocks.NewRepoMetaStore(t), mocks.NewDepsStore(t), mocks.NewClock(t)
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, MergeRequests: mrs, Issues: is, Deps: store, Clock: clock})
	mrs.EXPECT().ListOpenAll(mock.Anything).Return(nil, nil)
	is.EXPECT().ListOpenAll(mock.Anything).Return(nil, nil)
	clock.EXPECT().Now().Return(now)
	store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{rec("alpha"), rec("archived"), rec("gone"), snoozed, expired, empty}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "alpha")).Return(domain.Repo{Name: name(t, "alpha")}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "expired")).Return(domain.Repo{Name: name(t, "expired")}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "archived")).Return(domain.Repo{Name: name(t, "archived"), Archived: true}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "gone")).Return(domain.Repo{}, domain.ErrNotFound)

	got, err := in.Overview(context.Background(), domain.Owner(), name(t, "alpha"))
	require.NoError(t, err)
	assert.Equal(t, 2, got.Counts.Deps, "the filter does not change the count")
	require.Len(t, got.Deps, 1, "the filter limits the list")
	assert.Equal(t, "alpha", got.Deps[0].Repo.String())
}
