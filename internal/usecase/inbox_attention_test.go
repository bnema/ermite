// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
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

func TestInboxAttentionAndCountsAreOwnerOnly(t *testing.T) {
	t.Parallel()
	in, _, _, _ := newInboxUnit(t) // the stores are never read
	for _, p := range []domain.Principal{domain.Anonymous(), issuePrincipal(domain.ScopeIssueWrite)} {
		_, err := in.Attention(t.Context(), p)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = in.Counts(t.Context(), p)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}
}

func check(status domain.CIStatus, c domain.CIConclusion) domain.CICheck {
	return domain.CICheck{Name: "test", Status: status, Conclusion: c}
}

// readyRig answers open merge requests named by their source branch, each
// with its own head, in repository alpha.
func readyRig(t *testing.T, withCI bool) (*usecase.Inbox, *mocks.CIStore, map[string]domain.ObjectID) {
	t.Helper()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mrs, is, meta, git := mocks.NewMergeRequestStore(t), mocks.NewIssueStore(t), mocks.NewRepoMetaStore(t), mocks.NewMergeGit(t)
	cfg := usecase.InboxConfig{Meta: meta, MergeRequests: mrs, Issues: is, Git: git}
	var ci *mocks.CIStore
	if withCI {
		ci = mocks.NewCIStore(t)
		cfg.CI = ci
	}
	heads := map[string]domain.ObjectID{}
	var open []domain.MergeRequest
	for i, src := range []string{"success", "none", "pending", "failed", "truncated", "gone", "draft"} {
		m := inboxMR(t, "alpha", int64(i+1), t0.Add(time.Duration(i)*time.Hour))
		m.Source = src
		if src == "draft" {
			ti, err := domain.ParseMRTitle("WIP: later")
			require.NoError(t, err)
			m.Title = ti
		}
		open = append(open, m)
		if src == "gone" {
			git.EXPECT().BranchHead(mock.Anything, m.Repo, src).Return(domain.ObjectID{}, domain.ErrNotFound).Once()
			continue
		}
		if src == "draft" {
			continue // a draft is never read from Git
		}
		heads[src] = oid(string("0123456789"[i]))
		git.EXPECT().BranchHead(mock.Anything, m.Repo, src).Return(heads[src], nil).Once()
	}
	mrs.EXPECT().ListOpenAll(mock.Anything).Return(open, nil)
	is.EXPECT().ListOpenAll(mock.Anything).Return(nil, nil)
	meta.EXPECT().Get(mock.Anything, name(t, "alpha")).Return(repoWithIssues(t, "alpha", domain.IssuesOff), nil).Once()
	return usecase.NewInbox(cfg), ci, heads
}

func readySources(items []ports.AttentionItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.MergeRequest.Source)
	}
	return out
}

func TestInboxAttentionReadyFollowsTheChecks(t *testing.T) {
	t.Parallel()
	in, ci, heads := readyRig(t, true)
	ok := []domain.CICheck{check(domain.CICompleted, domain.CISuccess)}
	answers := map[string]struct {
		checks []domain.CICheck
		more   bool
	}{
		"success":   {checks: ok},
		"none":      {},
		"pending":   {checks: []domain.CICheck{check(domain.CIInProgress, domain.CINoConclusion)}},
		"failed":    {checks: []domain.CICheck{check(domain.CICompleted, domain.CIFailure)}},
		"truncated": {checks: ok, more: true},
	}
	for src, a := range answers {
		ci.EXPECT().ListChecks(mock.Anything, name(t, "alpha"), heads[src].String(), usecase.MaxCIChecks).Return(a.checks, a.more, nil).Once()
	}
	got, err := in.Attention(t.Context(), domain.Owner())
	require.NoError(t, err)
	assert.Equal(t, []string{"none", "success"}, readySources(got), "newest first; pending, failed, truncated, gone and draft left out")
	assert.Equal(t, ok, got[1].Checks)
	assert.Equal(t, heads["success"], got[1].MergeRequest.HeadID)
	assert.Equal(t, ports.AttentionReady, got[0].Kind)
}

func TestInboxAttentionReadyWithoutCI(t *testing.T) {
	t.Parallel()
	in, _, _ := readyRig(t, false)
	got, err := in.Attention(t.Context(), domain.Owner())
	require.NoError(t, err)
	assert.Equal(t, []string{"truncated", "failed", "pending", "none", "success"}, readySources(got))
	assert.Nil(t, got[0].Checks)
}

func TestInboxAttentionKindsOrderAndCounts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	mrs, is, meta, pending := mocks.NewMergeRequestStore(t), mocks.NewIssueStore(t), mocks.NewRepoMetaStore(t), mocks.NewPendingIssueStore(t)
	store, clock, repos, reader := mocks.NewDepsStore(t), mocks.NewClock(t), mocks.NewGitRepos(t), mocks.NewRepoReader(t)
	in := usecase.NewInbox(usecase.InboxConfig{
		Meta: meta, MergeRequests: mrs, Issues: is, Pending: pending, Deps: store, Clock: clock, Repos: repos, Reader: reader,
	})
	clock.EXPECT().Now().Return(now)
	alpha, beta := name(t, "alpha"), name(t, "beta")
	for _, n := range []string{"alpha", "beta"} {
		meta.EXPECT().Get(mock.Anything, name(t, n)).Return(repoWithIssues(t, n, domain.IssuesPublic), nil).Twice() // once per Attention and Counts call
	}
	meta.EXPECT().Get(mock.Anything, name(t, "archived")).Return(domain.Repo{Name: name(t, "archived"), Archived: true}, nil).Twice()

	// Work: an MR in beta (ready: no Git keeps its stored head, no CI) and an
	// issue in alpha.
	mr := inboxMR(t, "beta", 7, now.Add(-time.Hour))
	mr.HeadID = oid("c")
	mrs.EXPECT().ListOpenAll(mock.Anything).Return([]domain.MergeRequest{mr}, nil)
	is.EXPECT().ListOpenAll(mock.Anything).Return([]domain.Issue{inboxIssue(t, "alpha", 1, now)}, nil)

	ti, err := domain.ParseIssueTitle("hi")
	require.NoError(t, err)
	pending.EXPECT().List(mock.Anything, domain.RepoName{}, mock.Anything).Return(ports.PendingIssuePage{Issues: []domain.PendingIssue{
		{ID: "old", Repo: beta, Title: ti, CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "new", Repo: alpha, Title: ti, CreatedAt: now.Add(-time.Hour)},
	}}, nil)

	upd := []domain.DepUpdate{{DepRef: domain.DepRef{Name: "x/y", Version: "v1.1.0"}, Current: "v1.0.0", Kind: domain.DepMinor}}
	batch := func(key string, st domain.DepsState) domain.DepBatch {
		return domain.DepBatch{Key: domain.DepBatchKey(key), Updates: upd, State: st, RunID: 0}
	}
	failed := domain.RepoDeps{Repo: alpha, CheckedAt: now.Add(-3 * time.Hour), Batches: []domain.DepBatch{batch("b", domain.DepsFailed), batch("a", domain.DepsFailed), batch("c", domain.DepsApplying)}}
	snoozed := domain.RepoDeps{Repo: beta, SnoozedUntil: now.Add(time.Hour), Batches: []domain.DepBatch{batch("", domain.DepsFailed)}}
	archived := domain.RepoDeps{Repo: name(t, "archived"), Batches: []domain.DepBatch{batch("", domain.DepsFailed)}}
	store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{failed, snoozed, archived}, nil)

	// Releases: alpha due (ten commits), beta soon (three), archived skipped.
	meta.EXPECT().List(mock.Anything).Return([]domain.Repo{
		{Name: alpha}, {Name: beta}, {Name: name(t, "archived"), Archived: true},
	}, nil)
	tag := domain.TagInfo{Name: "v1", At: now.Add(-24 * time.Hour)}
	for n, commits := range map[domain.RepoName]int{alpha: 10, beta: 3} {
		repos.EXPECT().HeadBranch(mock.Anything, n).Return("main", nil)
		reader.EXPECT().ResolveRevision(mock.Anything, n, domain.Revision{}).Return(domain.ResolvedRevision{CommitID: oid("a")}, nil)
		reader.EXPECT().TagDistance(mock.Anything, n, oid("a"), domain.MaxTagDistance).Return(domain.TagDistance{Tag: tag, Commits: commits}, true, nil)
	}

	got, err := in.Attention(t.Context(), domain.Owner())
	require.NoError(t, err)
	type row struct {
		kind ports.AttentionKind
		repo string
	}
	var rows []row
	for _, it := range got {
		rows = append(rows, row{it.Kind, it.Repo.String()})
	}
	assert.Equal(t, []row{
		{ports.AttentionModeration, "alpha"}, {ports.AttentionModeration, "beta"},
		{ports.AttentionDepsFailed, "alpha"}, {ports.AttentionDepsFailed, "alpha"},
		{ports.AttentionReady, "beta"},
		{ports.AttentionRelease, "alpha"},
	}, rows)
	assert.Equal(t, domain.DepBatchKey("a"), got[2].Deps.Batch.Key, "same time and repository: by batch key")
	assert.True(t, got[2].Since.Equal(failed.CheckedAt))
	assert.Nil(t, got[0].Pending.IPHash)
	assert.True(t, got[5].Since.Equal(tag.At))
	assert.Equal(t, 10, got[5].Unreleased.Distance.Commits)

	counts, err := in.Counts(t.Context(), domain.Owner())
	require.NoError(t, err)
	assert.Equal(t, ports.OwnerCounts{Attention: len(got), Work: 2}, counts)
}
