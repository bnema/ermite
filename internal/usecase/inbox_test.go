// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"strings"
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

func TestInboxWorkSortsAndSkipsIssuesOff(t *testing.T) {
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

	got, err := in.Work(ctx, domain.Owner())
	require.NoError(t, err)
	require.Len(t, got.MergeRequests, 2)
	assert.Equal(t, "beta", got.MergeRequests[0].Repo.String(), "most recently updated first")
	require.Len(t, got.Issues, 2)
	assert.Equal(t, "beta", got.Issues[0].Repo.String())
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
	a, err := in.Attention(ctx, domain.Owner())
	require.NoError(t, err)
	items := a.Items
	require.Len(t, items, 1, "issues off hides pending ones too")
	require.Equal(t, ports.AttentionModeration, items[0].Kind)
	assert.Nil(t, items[0].Pending.IPHash)
}

func TestInboxRuns(t *testing.T) {
	t.Parallel()
	meta, ci := mocks.NewRepoMetaStore(t), mocks.NewCIStore(t)
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
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, CI: ci})
	all, err := in.Runs(t.Context(), domain.Owner())
	require.NoError(t, err)
	assert.True(t, all.Enabled)
	require.Len(t, all.Runs, 2)
	assert.Equal(t, int64(2), all.Runs[0].ID)
	assert.Equal(t, int64(1), all.Runs[1].ID)
}

func TestInboxDepsSkipsSnoozedArchivedAndEmpty(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	upd := []domain.DepUpdate{{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "x/y", Version: "v1.1.0"}, Current: "v1.0.0", Kind: domain.DepMinor}}
	rec := func(repo string) domain.RepoDeps {
		return domain.RepoDeps{Repo: name(t, repo), Batches: []domain.DepBatch{{Updates: upd}}}
	}
	snoozed, expired, empty := rec("snoozed"), rec("expired"), domain.RepoDeps{Repo: name(t, "empty")}
	snoozed.SnoozedUntil = now.Add(time.Hour)
	expired.SnoozedUntil = now.Add(-time.Hour)
	alpha, other := rec("alpha"), expired
	alpha.CheckedAt, other.CheckedAt = now.Add(-48*time.Hour), now.Add(-time.Hour)

	meta, store, clock := mocks.NewRepoMetaStore(t), mocks.NewDepsStore(t), mocks.NewClock(t)
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, Deps: store, Clock: clock})
	clock.EXPECT().Now().Return(now)
	store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{alpha, rec("archived"), rec("gone"), snoozed, other, empty}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "alpha")).Return(domain.Repo{Name: name(t, "alpha")}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "expired")).Return(domain.Repo{Name: name(t, "expired")}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "archived")).Return(domain.Repo{Name: name(t, "archived"), Archived: true}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "gone")).Return(domain.Repo{}, domain.ErrNotFound)

	got, err := in.Deps(context.Background(), domain.Owner())
	require.NoError(t, err)
	require.Len(t, got.Deps, 2)
	assert.Equal(t, "alpha", got.Deps[0].Rec.Repo.String())
	assert.True(t, got.CheckedAt.Equal(other.CheckedAt), "the latest check across repositories")
}

func TestInboxDepsJoinsTheRun(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	upd := []domain.DepUpdate{{DepRef: domain.DepRef{Name: "x/y", Version: "v1.1.0"}, Current: "v1.0.0", Kind: domain.DepMinor}}
	older, newer := now.Add(-48*time.Hour), now.Add(-time.Hour)
	withRun := domain.RepoDeps{Repo: name(t, "alpha"), CheckedAt: older, Batches: []domain.DepBatch{{Updates: upd, State: domain.DepsApplying, RunID: 5}}}
	withRun.Batches = append(withRun.Batches, domain.DepBatch{Key: "major", MR: 10})
	gone := domain.RepoDeps{Repo: name(t, "beta"), CheckedAt: newer, Batches: []domain.DepBatch{{Updates: append(upd, upd...), State: domain.DepsFailed, RunID: 9}}}
	none := domain.RepoDeps{Repo: name(t, "gamma"), CheckedAt: older, Batches: []domain.DepBatch{{Updates: upd}, {Key: "hidden", State: domain.DepsFailed, RunID: 1}}}

	meta, store, clock, ci := mocks.NewRepoMetaStore(t), mocks.NewDepsStore(t), mocks.NewClock(t), mocks.NewCIStore(t)
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, Deps: store, Clock: clock, CI: ci})
	clock.EXPECT().Now().Return(now)
	store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{withRun, gone, none}, nil)
	for _, r := range []string{"alpha", "beta", "gamma"} {
		meta.EXPECT().Get(mock.Anything, name(t, r)).Return(domain.Repo{Name: name(t, r)}, nil)
	}
	ci.EXPECT().GetRun(mock.Anything, name(t, "alpha"), int64(5)).Return(domain.CIRun{ID: 5, Number: 3, Status: domain.CIQueued}, nil)
	ci.EXPECT().GetRun(mock.Anything, name(t, "beta"), int64(9)).Return(domain.CIRun{}, domain.ErrNotFound)

	got, err := in.Deps(context.Background(), domain.Owner())
	require.NoError(t, err)
	require.Len(t, got.Deps, 4)
	assert.Equal(t, domain.DepBatchKey("major"), got.Deps[1].Batch.Key)
	require.NotNil(t, got.Deps[0].Run)
	assert.EqualValues(t, 3, got.Deps[0].Run.Number)
	assert.Nil(t, got.Deps[2].Run, "a run that is gone is no run")
	assert.Nil(t, got.Deps[3].Run, "a record without a run reads none")
	assert.True(t, got.CheckedAt.Equal(newer), "the latest check")
}

// A run that cannot be read does not fail the Inbox: the row shows the record
// without it.
func TestInboxDepsKeepsTheRowWhenTheRunCannotBeRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	upd := []domain.DepUpdate{{DepRef: domain.DepRef{Name: "x/y", Version: "v1.1.0"}, Current: "v1.0.0", Kind: domain.DepMinor}}
	rec := domain.RepoDeps{Repo: name(t, "alpha"), Batches: []domain.DepBatch{{Updates: upd, State: domain.DepsApplying, RunID: 5}}}

	meta, store, clock, ci := mocks.NewRepoMetaStore(t), mocks.NewDepsStore(t), mocks.NewClock(t), mocks.NewCIStore(t)
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, Deps: store, Clock: clock, CI: ci})
	clock.EXPECT().Now().Return(now)
	store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{rec}, nil)
	meta.EXPECT().Get(mock.Anything, rec.Repo).Return(domain.Repo{Name: rec.Repo}, nil)
	ci.EXPECT().GetRun(mock.Anything, rec.Repo, int64(5)).Return(domain.CIRun{}, errors.New("disk"))

	got, err := in.Deps(context.Background(), domain.Owner())
	require.NoError(t, err)
	require.Len(t, got.Deps, 1)
	assert.Equal(t, rec, got.Deps[0].Rec)
	assert.Nil(t, got.Deps[0].Run)
}

func TestInboxFillsHeadsFromGitForReturnedRequestsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mrs, is, meta, git := mocks.NewMergeRequestStore(t), mocks.NewIssueStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t)
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, MergeRequests: mrs, Issues: is, Git: git})
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stale, err := domain.ParseObjectID(strings.Repeat("a", 40))
	require.NoError(t, err)
	fresh, err := domain.ParseObjectID(strings.Repeat("b", 40))
	require.NoError(t, err)
	withSource := func(repo string, n int64, src string) domain.MergeRequest {
		m := inboxMR(t, repo, n, t0)
		m.Source, m.HeadID = src, stale
		return m
	}
	mrs.EXPECT().ListOpenAll(mock.Anything).Return([]domain.MergeRequest{
		withSource("alpha", 1, "topic"), withSource("alpha", 2, "gone"), withSource("beta", 3, "other"), withSource("ghost", 4, "x"),
	}, nil)
	is.EXPECT().ListOpenAll(mock.Anything).Return(nil, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "alpha")).Return(repoWithIssues(t, "alpha", domain.IssuesOff), nil)
	meta.EXPECT().Get(mock.Anything, name(t, "beta")).Return(repoWithIssues(t, "beta", domain.IssuesOff), nil)
	meta.EXPECT().Get(mock.Anything, name(t, "ghost")).Return(domain.Repo{}, domain.ErrNotFound)
	git.EXPECT().BranchHead(mock.Anything, name(t, "alpha"), "topic").Return(fresh, nil).Once()
	git.EXPECT().BranchHead(mock.Anything, name(t, "alpha"), "gone").Return(domain.ObjectID{}, domain.ErrNotFound).Once()
	git.EXPECT().BranchHead(mock.Anything, name(t, "beta"), "other").Return(fresh, nil).Once()
	// The ghost repository does not exist and is not read from Git.

	got, err := in.Work(ctx, domain.Owner())
	require.NoError(t, err)
	require.Len(t, got.MergeRequests, 3)
	heads := map[int64]domain.ObjectID{}
	for _, m := range got.MergeRequests {
		heads[m.Number] = m.HeadID
	}
	assert.Equal(t, fresh, heads[1])
	assert.True(t, heads[2].IsZero(), "a gone branch leaves the head zero")
}

func TestInboxDepsQueue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	upd := []domain.DepUpdate{{DepRef: domain.DepRef{Name: "x/y", Version: "v1.1.0"}, Current: "v1.0.0", Kind: domain.DepMinor}}
	alpha := domain.RepoDeps{Repo: name(t, "alpha"), Batches: []domain.DepBatch{
		{Updates: upd, State: domain.DepsDrafted, QueuedAt: now.Add(-time.Hour)},
		{Key: "m", Updates: upd, State: domain.DepsApplying},
	}}
	beta := domain.RepoDeps{Repo: name(t, "beta"), Batches: []domain.DepBatch{{Updates: upd, State: domain.DepsQueued, Manual: true, QueuedAt: now}}}

	meta, store, clock := mocks.NewRepoMetaStore(t), mocks.NewDepsStore(t), mocks.NewClock(t)
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, Deps: store, Clock: clock, DepsLimit: 2})
	clock.EXPECT().Now().Return(now)
	store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{alpha, beta}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "alpha")).Return(domain.Repo{Name: name(t, "alpha")}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "beta")).Return(domain.Repo{Name: name(t, "beta")}, nil)

	got, err := in.Deps(context.Background(), domain.Owner())
	require.NoError(t, err)
	assert.Equal(t, ports.DepsQueue{Running: 1, Limit: 2, Queued: 2}, got.Queue)
	pos := map[string]int{}
	for _, d := range got.Deps {
		pos[d.Rec.Repo.String()+"/"+string(d.Batch.Key)] = d.QueuePos
	}
	assert.Equal(t, map[string]int{"alpha/": 2, "alpha/m": 0, "beta/": 1}, pos, "manual first, 1 = next")
}

func TestInboxFocusedReadsAreOwnerOnly(t *testing.T) {
	t.Parallel()
	in, _, _, _ := newInboxUnit(t) // the stores are never read
	for _, p := range []domain.Principal{domain.Anonymous(), issuePrincipal(domain.ScopeIssueWrite)} {
		_, err := in.Work(t.Context(), p)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = in.Runs(t.Context(), p)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = in.Deps(t.Context(), p)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}
}

func TestInboxWorkSkipsMissingAndIssuesOffWithHeads(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mrs, is, meta, git := mocks.NewMergeRequestStore(t), mocks.NewIssueStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t)
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, MergeRequests: mrs, Issues: is, Git: git})
	head, err := domain.ParseObjectID(strings.Repeat("b", 40))
	require.NoError(t, err)
	alphaMR, betaMR := inboxMR(t, "alpha", 1, t0), inboxMR(t, "beta", 1, t0.Add(time.Hour))
	alphaMR.Source, betaMR.Source = "a", "b"
	mrs.EXPECT().ListOpenAll(mock.Anything).Return([]domain.MergeRequest{alphaMR, betaMR, inboxMR(t, "gone", 1, t0)}, nil)
	is.EXPECT().ListOpenAll(mock.Anything).Return([]domain.Issue{
		inboxIssue(t, "alpha", 2, t0), inboxIssue(t, "beta", 2, t0.Add(2*time.Hour)), inboxIssue(t, "quiet", 2, t0),
	}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "alpha")).Return(repoWithIssues(t, "alpha", domain.IssuesOwner), nil).Once()
	meta.EXPECT().Get(mock.Anything, name(t, "beta")).Return(repoWithIssues(t, "beta", domain.IssuesPublic), nil).Once()
	meta.EXPECT().Get(mock.Anything, name(t, "quiet")).Return(repoWithIssues(t, "quiet", domain.IssuesOff), nil).Once()
	meta.EXPECT().Get(mock.Anything, name(t, "gone")).Return(domain.Repo{}, domain.ErrNotFound).Once()
	git.EXPECT().BranchHead(mock.Anything, name(t, "alpha"), "a").Return(head, nil).Once()
	git.EXPECT().BranchHead(mock.Anything, name(t, "beta"), "b").Return(domain.ObjectID{}, domain.ErrNotFound).Once()

	got, err := in.Work(t.Context(), domain.Owner())
	require.NoError(t, err)
	require.Len(t, got.MergeRequests, 2)
	assert.Equal(t, "beta", got.MergeRequests[0].Repo.String(), "most recently updated first")
	assert.True(t, got.MergeRequests[0].HeadID.IsZero(), "a gone branch leaves the head zero")
	assert.Equal(t, head, got.MergeRequests[1].HeadID)
	require.Len(t, got.Issues, 2, "issues off and missing repositories are skipped")
	assert.Equal(t, "beta", got.Issues[0].Repo.String())
}

func TestInboxRunsOnlyOpenOfExistingRepos(t *testing.T) {
	t.Parallel()
	meta, ci := mocks.NewRepoMetaStore(t), mocks.NewCIStore(t)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	alpha, beta := name(t, "alpha"), name(t, "beta")
	ci.EXPECT().ListOpenRuns(mock.Anything).Return([]domain.CIRun{
		{Repo: alpha, ID: 1, Status: domain.CIQueued, CreatedAt: now},
		{Repo: beta, ID: 2, Status: domain.CIInProgress, CreatedAt: now.Add(time.Hour)},
		{Repo: alpha, ID: 3, Status: domain.CICompleted, CreatedAt: now.Add(2 * time.Hour)},
		{Repo: name(t, "gone"), ID: 4, Status: domain.CIQueued, CreatedAt: now},
	}, nil)
	for _, n := range []string{"alpha", "beta"} {
		meta.EXPECT().Get(mock.Anything, name(t, n)).Return(repoWithIssues(t, n, domain.IssuesOff), nil).Once()
	}
	meta.EXPECT().Get(mock.Anything, name(t, "gone")).Return(domain.Repo{}, domain.ErrNotFound).Once()
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, CI: ci})
	got, err := in.Runs(t.Context(), domain.Owner())
	require.NoError(t, err)
	assert.True(t, got.Enabled)
	require.Len(t, got.Runs, 2)
	assert.Equal(t, int64(2), got.Runs[0].ID, "newest first")
	assert.Equal(t, int64(1), got.Runs[1].ID)

	off, err := usecase.NewInbox(usecase.InboxConfig{Meta: meta}).Runs(t.Context(), domain.Owner())
	require.NoError(t, err)
	assert.Equal(t, ports.InboxRuns{}, off, "no CI reads as disabled")
}

func TestInboxDepsViewSkipsSnoozedArchivedAndMissing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	upd := []domain.DepUpdate{{DepRef: domain.DepRef{Name: "x/y", Version: "v1.1.0"}, Current: "v1.0.0", Kind: domain.DepMinor}}
	rec := func(repo string) domain.RepoDeps {
		return domain.RepoDeps{Repo: name(t, repo), Batches: []domain.DepBatch{{Updates: upd, State: domain.DepsQueued, QueuedAt: now}}}
	}
	alpha, snoozed := rec("alpha"), rec("snoozed")
	alpha.CheckedAt = now.Add(-time.Hour)
	snoozed.SnoozedUntil = now.Add(time.Hour)

	meta, store, clock := mocks.NewRepoMetaStore(t), mocks.NewDepsStore(t), mocks.NewClock(t)
	in := usecase.NewInbox(usecase.InboxConfig{Meta: meta, Deps: store, Clock: clock, DepsLimit: 2})
	clock.EXPECT().Now().Return(now)
	store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{alpha, rec("archived"), rec("gone"), snoozed}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "alpha")).Return(domain.Repo{Name: name(t, "alpha")}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "archived")).Return(domain.Repo{Name: name(t, "archived"), Archived: true}, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "gone")).Return(domain.Repo{}, domain.ErrNotFound)

	got, err := in.Deps(t.Context(), domain.Owner())
	require.NoError(t, err)
	require.Len(t, got.Deps, 1)
	assert.Equal(t, "alpha", got.Deps[0].Rec.Repo.String())
	assert.True(t, got.CheckedAt.Equal(alpha.CheckedAt))
	assert.Equal(t, 2, got.Queue.Limit)
	assert.Equal(t, 4, got.Queue.Queued, "the queue spans snoozed, archived and missing records")
}
