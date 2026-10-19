// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func newPending(t *testing.T, repo domain.RepoName, title, author string, at time.Time) domain.PendingIssue {
	t.Helper()
	id, err := domain.NewPendingIssueID()
	require.NoError(t, err)
	ti, err := domain.ParseIssueTitle(title)
	require.NoError(t, err)
	body, err := domain.ParseIssueBody("details")
	require.NoError(t, err)
	return domain.PendingIssue{
		ID: id, Repo: repo, Title: ti, Body: body, Author: author, CreatedAt: at,
		IPHash: bytes.Repeat([]byte{7}, domain.IPHashLen),
	}
}

func TestPendingIssuePutGetListCount(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	other := name(t, "other")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: other, CreatedAt: created, Visibility: domain.VisibilityPublic}))
	ps := s.PendingIssues()

	a := newPending(t, n, "a", "Alex", created.Add(time.Minute))
	b := newPending(t, other, "b", "", created.Add(2*time.Minute))
	c := newPending(t, n, "c", "", created.Add(3*time.Minute))
	for _, p := range []domain.PendingIssue{c, a, b} {
		require.NoError(t, ps.Put(ctx, p))
	}
	require.ErrorIs(t, ps.Put(ctx, a), domain.ErrConflict, "an id is used once")
	require.ErrorIs(t, ps.Put(ctx, newPending(t, name(t, "ghost"), "g", "", created)), domain.ErrNotFound)

	got, err := ps.Get(ctx, n, a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.Title, got.Title)
	assert.Equal(t, "Alex", got.Author)
	assert.Equal(t, a.IPHash, got.IPHash)
	assert.True(t, got.CreatedAt.Equal(a.CreatedAt))
	assert.NotZero(t, got.Version)
	_, err = ps.Get(ctx, other, a.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound, "ids are per repository")

	all, err := ps.List(ctx, domain.RepoName{}, ports.ListPendingParams{})
	require.NoError(t, err)
	require.Len(t, all.Issues, 3)
	assert.Equal(t, []domain.PendingIssueID{a.ID, b.ID, c.ID}, []domain.PendingIssueID{all.Issues[0].ID, all.Issues[1].ID, all.Issues[2].ID}, "oldest first")

	mine, err := ps.List(ctx, n, ports.ListPendingParams{})
	require.NoError(t, err)
	assert.Len(t, mine.Issues, 2)

	p1, err := ps.List(ctx, domain.RepoName{}, ports.ListPendingParams{PageSize: 2})
	require.NoError(t, err)
	require.Len(t, p1.Issues, 2)
	require.NotEmpty(t, p1.NextPageToken)
	p2, err := ps.List(ctx, domain.RepoName{}, ports.ListPendingParams{PageSize: 2, PageToken: p1.NextPageToken})
	require.NoError(t, err)
	require.Len(t, p2.Issues, 1)
	assert.Equal(t, c.ID, p2.Issues[0].ID)
	assert.Empty(t, p2.NextPageToken)
	_, err = ps.List(ctx, domain.RepoName{}, ports.ListPendingParams{PageToken: "x"})
	assert.ErrorIs(t, err, domain.ErrInvalid)

	cnt, err := ps.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, cnt)

	require.NoError(t, ps.Delete(ctx, n, a.ID))
	assert.ErrorIs(t, ps.Delete(ctx, n, a.ID), domain.ErrNotFound)
	cnt, err = ps.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, cnt)
}

func TestPendingIssueCountIsCapped(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	ps := s.PendingIssues()
	for i := range domain.MaxPendingIssues + 5 {
		require.NoError(t, ps.Put(ctx, newPending(t, n, "t", "", created.Add(time.Duration(i)*time.Second))))
	}
	cnt, err := ps.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.MaxPendingIssues+1, cnt)
}

func TestPendingIssueApproveSharesNumbers(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)
	ps := s.PendingIssues()

	mr, err := mrs.Create(ctx, newMR(t, n, "mr"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), mr.Number)

	named := newPending(t, n, "named", "Alex", created)
	anon := newPending(t, n, "anon", "", created.Add(time.Second))
	require.NoError(t, ps.Put(ctx, named))
	require.NoError(t, ps.Put(ctx, anon))

	issue, err := ps.Approve(ctx, n, named.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), issue.Number)
	assert.Equal(t, domain.IssueStateOpen, issue.State)
	assert.Equal(t, "Alex", issue.Author)
	assert.Equal(t, "named", issue.Title.String())
	assert.True(t, issue.External, "an approved submission is external")

	_, err = ps.Get(ctx, n, named.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound, "the pending record is gone")
	_, err = ps.Approve(ctx, n, named.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound, "approved once")

	got, err := s.Issues().Get(ctx, n, 2)
	require.NoError(t, err)
	assert.Equal(t, issue.Title, got.Title)
	assert.True(t, got.External, "the flag is stored")
	open, err := s.Issues().ListOpen(ctx, n)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, int64(2), open[0].Number)

	second, err := ps.Approve(ctx, n, anon.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), second.Number)
	assert.Equal(t, "anonymous", second.Author)

	next, err := s.Issues().Create(ctx, newIssue(t, n, "owner"))
	require.NoError(t, err)
	assert.Equal(t, int64(4), next.Number, "issues and merge requests share the counter")
	assert.False(t, next.External, "an issue of the owner is not external")
}

func TestPendingIssueApproveConcurrent(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)
	ps := s.PendingIssues()
	const k = 8
	ids := make([]domain.PendingIssueID, k)
	for i := range k {
		p := newPending(t, n, "t", "", created.Add(time.Duration(i)*time.Second))
		ids[i] = p.ID
		require.NoError(t, ps.Put(ctx, p))
	}
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			is, err := ps.Approve(ctx, n, id)
			if assert.NoError(t, err) {
				mu.Lock()
				seen[is.Number] = true
				mu.Unlock()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		mr, err := mrs.Create(ctx, newMR(t, n, "mr"))
		if assert.NoError(t, err) {
			mu.Lock()
			seen[mr.Number] = true
			mu.Unlock()
		}
	}()
	wg.Wait()
	assert.Len(t, seen, k+1, "no number is given twice")
}

func TestPendingIssueApproveOnDeletedRepoFails(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	ps := s.PendingIssues()
	p := newPending(t, n, "t", "", created)
	require.NoError(t, ps.Put(ctx, p))
	require.NoError(t, s.Delete(ctx, n))

	_, err := ps.Approve(ctx, n, p.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.ErrorIs(t, ps.Put(ctx, newPending(t, n, "t", "", created)), domain.ErrNotFound)
	assert.Zero(t, countKeys(t, s, "is/"))
	assert.Zero(t, countKeys(t, s, "isopen/"))
}

func TestRepoDeleteRemovesPendingIssues(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	other := name(t, "other")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: other, CreatedAt: created, Visibility: domain.VisibilityPublic}))
	ps := s.PendingIssues()
	require.NoError(t, ps.Put(ctx, newPending(t, n, "t", "", created)))
	require.NoError(t, ps.Put(ctx, newPending(t, n, "u", "", created)))
	keep := newPending(t, other, "k", "", created)
	require.NoError(t, ps.Put(ctx, keep))

	require.NoError(t, s.Delete(ctx, n))
	assert.Zero(t, countKeys(t, s, "ispend/"+n.String()+"/"))
	_, err := ps.Get(ctx, other, keep.ID)
	assert.NoError(t, err, "another repository keeps its pending issues")
	cnt, err := ps.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, cnt)
}

func TestIssueBansExpire(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	bans := s.IssueBans()
	h := bytes.Repeat([]byte{1}, domain.IPHashLen)
	other := bytes.Repeat([]byte{2}, domain.IPHashLen)
	now := created

	banned, err := bans.Banned(ctx, h, now)
	require.NoError(t, err)
	assert.False(t, banned)

	require.NoError(t, bans.Ban(ctx, h, now.Add(domain.BanDuration)))
	require.ErrorIs(t, bans.Ban(ctx, []byte("short"), now), domain.ErrInvalid)
	banned, err = bans.Banned(ctx, h, now.Add(time.Hour))
	require.NoError(t, err)
	assert.True(t, banned)
	banned, err = bans.Banned(ctx, other, now.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, banned, "another hash is not banned")
	assert.Equal(t, 1, countKeys(t, s, "isban/"))

	banned, err = bans.Banned(ctx, h, now.Add(domain.BanDuration))
	require.NoError(t, err)
	assert.False(t, banned, "expired")
	assert.Zero(t, countKeys(t, s, "isban/"), "an expired ban is deleted")
}

func TestIssueKeyPersists(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openStore(t, dir)
	key, err := s.IssueKeys().Key(ctx)
	require.NoError(t, err)
	assert.Len(t, key, 32)
	again, err := s.IssueKeys().Key(ctx)
	require.NoError(t, err)
	assert.Equal(t, key, again)
	sess, err := s.Sessions().Key(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, sess, key, "not the session key")
	require.NoError(t, s.Close(ctx))

	s = openStore(t, dir)
	reopened, err := s.IssueKeys().Key(ctx)
	require.NoError(t, err)
	assert.Equal(t, key, reopened, "a restart keeps the key")
}
