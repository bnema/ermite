// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func newIssue(t *testing.T, repo domain.RepoName, title string) domain.Issue {
	t.Helper()
	ti, err := domain.ParseIssueTitle(title)
	require.NoError(t, err)
	body, err := domain.ParseIssueBody("what\nhappened")
	require.NoError(t, err)
	return domain.Issue{
		Repo: repo, Title: ti, Body: body, State: domain.IssueStateOpen,
		Author: "agent", CreatedAt: created, UpdatedAt: created,
	}
}

func newIssueComment(t *testing.T, id int64, text string) domain.IssueComment {
	t.Helper()
	body, err := domain.ParseIssueBody(text)
	require.NoError(t, err)
	return domain.IssueComment{ID: id, Author: "agent", Text: body, CreatedAt: created}
}

// countKeys counts the keys of the store under prefix.
func countKeys(t *testing.T, s *kvstore.Store, prefix string) int {
	t.Helper()
	return s.CountKeysForTest(context.Background(), prefix)
}

func TestIssueRoundTripAndPaging(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	is := s.Issues()

	first, err := is.Create(ctx, newIssue(t, n, "one"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), first.Number)
	assert.NotZero(t, first.Version)
	got, err := is.Get(ctx, n, 1)
	require.NoError(t, err)
	assert.Equal(t, first, got)
	_, err = is.Get(ctx, n, 2)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	for _, title := range []string{"two", "three", "four"} {
		_, err := is.Create(ctx, newIssue(t, n, title))
		require.NoError(t, err)
	}
	closed, err := is.Get(ctx, n, 2)
	require.NoError(t, err)
	closed.State = domain.IssueStateClosed
	require.NoError(t, is.Update(ctx, closed))

	page, err := is.List(ctx, n, ports.ListIssuesParams{PageSize: 3})
	require.NoError(t, err)
	require.Len(t, page.Issues, 3)
	assert.Equal(t, int64(4), page.Issues[0].Number)
	require.NotEmpty(t, page.NextPageToken)
	page, err = is.List(ctx, n, ports.ListIssuesParams{PageSize: 3, PageToken: page.NextPageToken})
	require.NoError(t, err)
	require.Len(t, page.Issues, 1)
	assert.Equal(t, int64(1), page.Issues[0].Number)
	assert.Empty(t, page.NextPageToken)

	page, err = is.List(ctx, n, ports.ListIssuesParams{State: domain.IssueStateClosed})
	require.NoError(t, err)
	require.Len(t, page.Issues, 1)
	assert.Equal(t, int64(2), page.Issues[0].Number)

	_, err = is.List(ctx, n, ports.ListIssuesParams{PageToken: "x"})
	assert.ErrorIs(t, err, domain.ErrInvalid)

	_, err = is.Create(ctx, newIssue(t, name(t, "ghost"), "x"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestIssueUpdateNeedsTheVersionItWasReadAt(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	is := s.Issues()
	first, err := is.Create(ctx, newIssue(t, n, "t"))
	require.NoError(t, err)
	a, b := first, first
	a.State = domain.IssueStateClosed
	require.NoError(t, is.Update(ctx, a))
	b.State = domain.IssueStateClosed
	assert.ErrorIs(t, is.Update(ctx, b), domain.ErrConflict)
}

func TestIssueCommentsAndSaveCommentPreconditions(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	is := s.Issues()
	issue, err := is.Create(ctx, newIssue(t, n, "t"))
	require.NoError(t, err)

	issue.CommentSeq = 1
	require.NoError(t, is.SaveComment(ctx, issue, newIssueComment(t, 1, "first")))
	// The issue moved on: the same version is refused.
	issue.CommentSeq = 2
	assert.ErrorIs(t, is.SaveComment(ctx, issue, newIssueComment(t, 2, "second")), domain.ErrConflict)
	issue, err = is.Get(ctx, n, issue.Number)
	require.NoError(t, err)
	issue.CommentSeq = 2
	require.NoError(t, is.SaveComment(ctx, issue, newIssueComment(t, 2, "second")))

	comments, err := is.ListComments(ctx, n, issue.Number)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	assert.Equal(t, "first", comments[0].Text.String())
	assert.Equal(t, int64(2), comments[1].ID)

	require.NoError(t, s.Delete(ctx, n))
	assert.ErrorIs(t, is.SaveComment(ctx, issue, newIssueComment(t, 3, "late")), domain.ErrNotFound)
}

func TestConcurrentMergeRequestsAndIssuesNeverShareANumber(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)
	is := s.Issues()
	const workers = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		numbers []int64
	)
	for w := range workers {
		wg.Go(func() {
			var number int64
			if w%2 == 0 {
				got, err := mrs.Create(ctx, newMR(t, n, "t"))
				if !assert.NoError(t, err) {
					return
				}
				number = got.Number
			} else {
				got, err := is.Create(ctx, newIssue(t, n, "t"))
				if !assert.NoError(t, err) {
					return
				}
				number = got.Number
			}
			mu.Lock()
			numbers = append(numbers, number)
			mu.Unlock()
		})
	}
	wg.Wait()
	want := make([]int64, workers)
	for i := range want {
		want[i] = int64(i + 1)
	}
	assert.ElementsMatch(t, want, numbers, "every number is used once, by an issue or a merge request")
}

func TestDeleteDropsTheIssuesOfTheRepository(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	sib := name(t, "proj-2")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: sib, CreatedAt: created, Visibility: domain.VisibilityPublic}))
	is := s.Issues()

	for _, repo := range []domain.RepoName{n, sib} {
		issue, err := is.Create(ctx, newIssue(t, repo, "t"))
		require.NoError(t, err)
		issue.CommentSeq = 1
		require.NoError(t, is.SaveComment(ctx, issue, newIssueComment(t, 1, "note")))
	}
	require.NoError(t, s.Delete(ctx, n))

	page, err := is.List(ctx, n, ports.ListIssuesParams{})
	require.NoError(t, err)
	assert.Empty(t, page.Issues)
	comments, err := is.ListComments(ctx, n, 1)
	require.NoError(t, err)
	assert.Empty(t, comments)
	assert.Empty(t, openOf(t, is, n))
	assert.Zero(t, countKeys(t, s, "is/proj/"))
	assert.Zero(t, countKeys(t, s, "isc/proj/"))
	assert.Zero(t, countKeys(t, s, "isopen/proj/"))

	open := openOf(t, is, sib)
	assert.Len(t, open, 1, "a repository whose name starts like the deleted one keeps its issues")
	comments, err = is.ListComments(ctx, sib, 1)
	require.NoError(t, err)
	assert.Len(t, comments, 1)

	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	again, err := is.Create(ctx, newIssue(t, n, "fresh"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), again.Number)
}

func TestOpenIssueIndexFollowsUpdates(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	is := s.Issues()
	a, err := is.Create(ctx, newIssue(t, n, "a"))
	require.NoError(t, err)
	b, err := is.Create(ctx, newIssue(t, n, "b"))
	require.NoError(t, err)

	numbers := func() []int64 {
		open := openOf(t, is, n)
		var out []int64
		for _, i := range open {
			out = append(out, i.Number)
		}
		return out
	}
	assert.Equal(t, []int64{2, 1}, numbers())
	assert.Equal(t, 2, countKeys(t, s, "isopen/proj/"))

	a.State = domain.IssueStateClosed
	require.NoError(t, is.Update(ctx, a))
	assert.Equal(t, []int64{2}, numbers())
	assert.Equal(t, 1, countKeys(t, s, "isopen/proj/"))

	a, err = is.Get(ctx, n, a.Number)
	require.NoError(t, err)
	a.State = domain.IssueStateOpen
	require.NoError(t, is.Update(ctx, a))
	assert.Equal(t, []int64{2, 1}, numbers())

	// A comment keeps the index as the issue says.
	b.State = domain.IssueStateClosed
	b.CommentSeq = 1
	require.NoError(t, is.SaveComment(ctx, b, newIssueComment(t, 1, "closing")))
	assert.Equal(t, []int64{1}, numbers())
	assert.Equal(t, 1, countKeys(t, s, "isopen/proj/"))
}

func TestRepoSettingsKeepTheIssuesMode(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	n := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	got, err := s.Get(ctx, n)
	require.NoError(t, err)
	assert.Equal(t, domain.IssuesOff, got.Settings.Issues)
	for _, m := range []domain.IssuesMode{domain.IssuesOwner, domain.IssuesPublic, domain.IssuesOff} {
		got.Settings.Issues = m
		_, err = s.Mutate(ctx, n, func(r *domain.Repo) (bool, error) { r.Settings = got.Settings; return true, nil })
		require.NoError(t, err)
		again, err := s.Get(ctx, n)
		require.NoError(t, err)
		assert.Equal(t, m, again.Settings.Issues)
	}
	got.Settings.Issues = "everyone"
	_, err = s.Mutate(ctx, n, func(r *domain.Repo) (bool, error) { r.Settings = got.Settings; return true, nil })
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

// TestSchema4UpgradesTheTokensWithAllScopes: the tokens that held every
// original scope keep their owner rights, gaining mr:review and issue:write
// (each only if missing), then mr:force-merge in schema 5 and release:write in
// schema 6; no other token changes.
func TestSchema4UpgradesTheTokensWithAllScopes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: 3}))

	all9 := []string{
		"repo:read", "repo:write", "repo:admin", "mr:write", "mr:merge",
		"ci:write", "secrets:write", "runner:register", "token:admin",
	}
	tokens := map[string]*storagepb.Token{
		"full":      {Id: "full", Scopes: all9},
		"full10":    {Id: "full10", Scopes: append(slices.Clone(all9), "mr:review")},
		"reposmr":   {Id: "reposmr", Scopes: append(slices.Clone(all9), "mr:review"), Repos: []string{"proj"}},
		"missing":   {Id: "missing", Scopes: all9[:8]},
		"repos":     {Id: "repos", Scopes: all9, Repos: []string{"proj"}},
		"prefix":    {Id: "prefix", Scopes: all9, BranchPrefixes: []string{"agent/"}},
		"small":     {Id: "small", Scopes: []string{"repo:read"}},
		"otherfull": {Id: "otherfull", Scopes: append([]string{"token:admin"}, all9[:8]...)},
	}
	for id, tok := range tokens {
		tok.Hash = make([]byte, 32)
		tok.Hash[0] = byte(len(id))
		putRaw(t, dir, "token/"+id, mustProto(t, tok))
	}

	s, err := kvstore.Open(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	get := func(id string) domain.Token {
		tok, err := s.Tokens().Get(ctx, id)
		require.NoError(t, err)
		return tok
	}

	full := get("full")
	assert.Contains(t, full.Scopes, domain.ScopeIssueWrite)
	assert.Contains(t, full.Scopes, domain.ScopeMRReview)
	assert.Len(t, full.Scopes, 13)
	assert.Contains(t, full.Scopes, domain.ScopeReleaseWrite)
	full10 := get("full10")
	assert.Len(t, full10.Scopes, 13)
	assert.Contains(t, full10.Scopes, domain.ScopeReleaseWrite)
	for _, id := range []string{"repos", "prefix", "missing", "reposmr", "small"} {
		assert.NotContains(t, get(id).Scopes, domain.ScopeReleaseWrite)
	}
	assert.Equal(t, 1, countScope(full10.Scopes, domain.ScopeMRReview))
	assert.Contains(t, full10.Scopes, domain.ScopeIssueWrite)
	assert.True(t, domain.TokenPrincipal(full10).HasOwnerRights())
	assert.Len(t, get("reposmr").Scopes, 10)
	assert.NotContains(t, get("reposmr").Scopes, domain.ScopeIssueWrite)
	assert.True(t, domain.TokenPrincipal(full).HasOwnerRights())
	assert.True(t, domain.TokenPrincipal(get("otherfull")).HasOwnerRights())

	for _, id := range []string{"missing", "reposmr", "repos", "prefix", "small"} {
		assert.NotContains(t, get(id).Scopes, domain.ScopeMRForceMerge)
	}
	assert.NotContains(t, get("missing").Scopes, domain.ScopeIssueWrite)
	assert.NotContains(t, get("missing").Scopes, domain.ScopeMRReview)
	assert.NotContains(t, get("repos").Scopes, domain.ScopeMRReview)
	assert.NotContains(t, get("prefix").Scopes, domain.ScopeMRReview)
	assert.False(t, domain.TokenPrincipal(get("missing")).HasOwnerRights())
	assert.NotContains(t, get("repos").Scopes, domain.ScopeIssueWrite)
	assert.NotContains(t, get("prefix").Scopes, domain.ScopeIssueWrite)
	assert.Equal(t, []domain.Scope{domain.ScopeRepoRead}, get("small").Scopes)
	assert.Len(t, get("missing").Scopes, 8)
	assert.Len(t, get("repos").Scopes, 9)
	assert.Len(t, get("prefix").Scopes, 9)
	assert.Equal(t, []string{"agent/"}, get("prefix").BranchPrefixes)
	assert.Equal(t, "full", full.ID)

	require.NoError(t, s.Close(ctx))
	assert.Equal(t, uint32(kvstore.SchemaVersion), readSchema(t, dir))

	// A second open changes nothing.
	s2, err := kvstore.Open(ctx, dir)
	require.NoError(t, err)
	tok, err := s2.Tokens().Get(ctx, "full")
	require.NoError(t, err)
	assert.Equal(t, full.Scopes, tok.Scopes)
	tok10, err := s2.Tokens().Get(ctx, "full10")
	require.NoError(t, err)
	assert.Equal(t, full10.Scopes, tok10.Scopes)
	require.NoError(t, s2.Close(ctx))
}

func TestSchemaNewerThanThisBuildStopsIssuesData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: kvstore.SchemaVersion + 1}))
	_, err := kvstore.Open(ctx, dir)
	require.ErrorIs(t, err, kvstore.ErrSchemaTooNew)
	assert.Equal(t, uint32(6), uint32(kvstore.SchemaVersion))
}

// A reopen racing the delete of the repository either lands before the delete
// removes it or fails; it never leaves an isopen/ key of a repository that is
// gone.
func TestDeleteVersusReopenLeavesNoIssueKeys(t *testing.T) {
	ctx := context.Background()
	for range 30 {
		s, _, n := repoWithMRs(t)
		is := s.Issues()
		issue, err := is.Create(ctx, newIssue(t, n, "t"))
		require.NoError(t, err)
		issue.State = domain.IssueStateClosed
		require.NoError(t, is.Update(ctx, issue))
		closed, err := is.Get(ctx, n, issue.Number)
		require.NoError(t, err)
		closed.State = domain.IssueStateOpen

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _ = is.Update(ctx, closed) }()
		go func() { defer wg.Done(); <-start; _ = s.Delete(ctx, n) }()
		close(start)
		wg.Wait()

		for _, prefix := range []string{"is/proj/", "isc/proj/", "isopen/proj/"} {
			assert.Zero(t, countKeys(t, s, prefix), prefix)
		}
		assert.ErrorIs(t, is.Update(ctx, closed), domain.ErrNotFound)
	}
}

func TestListOpenAllSpansRepositoriesThroughTheIndex(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)
	other := name(t, "other")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: other, CreatedAt: created, Visibility: domain.VisibilityPublic}))
	is := s.Issues()

	i1, err := is.Create(ctx, newIssue(t, n, "one"))
	require.NoError(t, err)
	i2, err := is.Create(ctx, newIssue(t, other, "two"))
	require.NoError(t, err)
	i3, err := is.Create(ctx, newIssue(t, other, "three"))
	require.NoError(t, err)
	i3.State = domain.IssueStateClosed
	require.NoError(t, is.Update(ctx, i3))
	_, err = mrs.Create(ctx, newMR(t, n, "mr1"))
	require.NoError(t, err)
	m2, err := mrs.Create(ctx, newMR(t, other, "mr2"))
	require.NoError(t, err)
	m3, err := mrs.Create(ctx, newMR(t, other, "mr3"))
	require.NoError(t, err)
	m3.State = domain.MRStateClosed
	require.NoError(t, mrs.Update(ctx, m3))

	issues, err := is.ListOpenAll(ctx)
	require.NoError(t, err)
	var got []string
	for _, i := range issues {
		got = append(got, i.Repo.String()+"#"+strconv.FormatInt(i.Number, 10))
	}
	assert.ElementsMatch(t, []string{
		i1.Repo.String() + "#" + strconv.FormatInt(i1.Number, 10),
		i2.Repo.String() + "#" + strconv.FormatInt(i2.Number, 10),
	}, got)

	open, err := mrs.ListOpenAll(ctx)
	require.NoError(t, err)
	got = nil
	for _, m := range open {
		got = append(got, m.Repo.String()+"#"+strconv.FormatInt(m.Number, 10))
	}
	assert.ElementsMatch(t, []string{"proj#2", m2.Repo.String() + "#" + strconv.FormatInt(m2.Number, 10)}, got)
	assert.Len(t, open, 2)
}

func TestListOpenAllIsEmptyWithoutOpenItems(t *testing.T) {
	ctx := context.Background()
	s, mrs, _ := repoWithMRs(t)
	issues, err := s.Issues().ListOpenAll(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)
	open, err := mrs.ListOpenAll(ctx)
	require.NoError(t, err)
	assert.Empty(t, open)
}

func TestListOpenAllSkipsAStaleIndexEntry(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)
	is := s.Issues()
	keepI, err := is.Create(ctx, newIssue(t, n, "keep"))
	require.NoError(t, err)
	goneI, err := is.Create(ctx, newIssue(t, n, "gone"))
	require.NoError(t, err)
	keepM, err := mrs.Create(ctx, newMR(t, n, "keep"))
	require.NoError(t, err)
	goneM, err := mrs.Create(ctx, newMR(t, n, "gone"))
	require.NoError(t, err)

	require.NoError(t, is.DropIssueRecordForTest(ctx, n, goneI.Number))
	require.NoError(t, mrs.DropMRRecordForTest(ctx, n, goneM.Number))
	require.Positive(t, countKeys(t, s, "isopen/proj/"), "the index entries are still there")
	require.Positive(t, countKeys(t, s, "mropen/proj/"))

	issues, err := is.ListOpenAll(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, keepI.Number, issues[0].Number)
	open, err := mrs.ListOpenAll(ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, keepM.Number, open[0].Number)
}

func TestIssueListPagesThroughManyIssues(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	is := s.Issues()
	const total = 53
	for i := 1; i <= total; i++ {
		_, err := is.Create(ctx, newIssue(t, n, "issue "+strconv.Itoa(i)))
		require.NoError(t, err)
	}
	for i := 2; i <= total; i += 3 { // 2, 5, 8, ...
		is, err := is.Get(ctx, n, int64(i))
		require.NoError(t, err)
		is.State = domain.IssueStateClosed
		require.NoError(t, s.Issues().Update(ctx, is))
	}

	walk := func(state domain.IssueState, size int) []int64 {
		var got []int64
		token := ""
		for {
			page, err := s.Issues().List(ctx, n, ports.ListIssuesParams{State: state, PageSize: size, PageToken: token})
			require.NoError(t, err)
			require.LessOrEqual(t, len(page.Issues), size)
			for _, is := range page.Issues {
				got = append(got, is.Number)
			}
			if page.NextPageToken == "" {
				return got
			}
			token = page.NextPageToken
		}
	}
	for _, size := range []int{1, 7, 10, 53, 100} {
		var all, closed []int64
		for i := int64(total); i >= 1; i-- {
			all = append(all, i)
			if i%3 == 2 {
				closed = append(closed, i)
			}
		}
		assert.Equal(t, all, walk("", size), "size %d", size)
		assert.Equal(t, closed, walk(domain.IssueStateClosed, size), "closed, size %d", size)
	}
	page, err := s.Issues().List(ctx, n, ports.ListIssuesParams{PageSize: 10, PageToken: "1"})
	require.NoError(t, err)
	assert.Empty(t, page.Issues)
	assert.Empty(t, page.NextPageToken)
}

// openOf lists the open issues of one repository through the isopen/ index.
func openOf(t *testing.T, is *kvstore.Issues, repo domain.RepoName) []domain.Issue {
	t.Helper()
	all, err := is.ListOpenAll(context.Background())
	require.NoError(t, err)
	var out []domain.Issue
	for _, i := range all {
		if i.Repo == repo {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Number > out[b].Number })
	return out
}

func TestStoredPendingIssueStateIsCorrupt(t *testing.T) {
	ctx := context.Background()
	s, _, n := repoWithMRs(t)
	is := s.Issues()
	raw, err := proto.Marshal(&storagepb.Issue{Number: 1, Title: "x", State: storagepb.IssueState_ISSUE_STATE_PENDING})
	require.NoError(t, err)
	require.NoError(t, is.PutRawIssueForTest(ctx, n, 1, raw))
	_, err = is.Get(ctx, n, 1)
	assert.ErrorContains(t, err, "pending")
	_, err = is.List(ctx, n, ports.ListIssuesParams{})
	assert.ErrorContains(t, err, "pending")
	_, err = is.List(ctx, n, ports.ListIssuesParams{State: domain.IssueStateOpen})
	assert.ErrorContains(t, err, "pending")
}

func countScope(scopes []domain.Scope, want domain.Scope) int {
	n := 0
	for _, s := range scopes {
		if s == want {
			n++
		}
	}
	return n
}

func TestSchema5UpgradesOwnerEquivalentTokens(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: 4}))
	var previous []string
	for _, sc := range domain.AllScopes() {
		if sc != domain.ScopeMRForceMerge && sc != domain.ScopeReleaseWrite {
			previous = append(previous, sc.String())
		}
	}
	tokens := map[string]*storagepb.Token{
		"owner":   {Id: "owner", Scopes: previous},
		"repos":   {Id: "repos", Scopes: previous, Repos: []string{"proj"}},
		"branch":  {Id: "branch", Scopes: previous, BranchPrefixes: []string{"agent/"}},
		"partial": {Id: "partial", Scopes: previous[:10]},
	}
	for id, tok := range tokens {
		tok.Hash = make([]byte, 32)
		putRaw(t, dir, "token/"+id, mustProto(t, tok))
	}
	for range 2 {
		s, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		for id, original := range tokens {
			tok, err := s.Tokens().Get(ctx, id)
			require.NoError(t, err)
			if id == "owner" {
				assert.True(t, domain.TokenPrincipal(tok).HasOwnerRights())
				assert.Equal(t, 1, countScope(tok.Scopes, domain.ScopeMRForceMerge))
				assert.Equal(t, 1, countScope(tok.Scopes, domain.ScopeReleaseWrite))
				assert.Len(t, tok.Scopes, 13)
			} else {
				assert.NotContains(t, tok.Scopes, domain.ScopeMRForceMerge)
				assert.NotContains(t, tok.Scopes, domain.ScopeReleaseWrite)
				var got []string
				for _, sc := range tok.Scopes {
					got = append(got, sc.String())
				}
				assert.Equal(t, original.Scopes, got)
			}
		}
		require.NoError(t, s.Close(ctx))
		assert.Equal(t, uint32(kvstore.SchemaVersion), readSchema(t, dir))
	}
}
