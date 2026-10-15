// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
)

func bySource(t *testing.T, mrs *kvstore.MergeRequests, repo domain.RepoName, branches ...string) []int64 {
	t.Helper()
	got, truncated, err := mrs.OpenBySource(context.Background(), repo, branches, 0)
	require.NoError(t, err)
	require.False(t, truncated, "no limit: never truncated")
	return got
}

func byTarget(t *testing.T, mrs *kvstore.MergeRequests, repo domain.RepoName, branches ...string) []int64 {
	t.Helper()
	got, truncated, err := mrs.OpenByTarget(context.Background(), repo, branches, 0)
	require.NoError(t, err)
	require.False(t, truncated)
	return got
}

func mrFromTo(t *testing.T, repo domain.RepoName, source, target string) domain.MergeRequest {
	mr := mrOn(t, repo, source)
	mr.Target = target
	return mr
}

func openNumbers(t *testing.T, mrs *kvstore.MergeRequests, repo domain.RepoName) []int64 {
	t.Helper()
	list, err := mrs.ListOpen(context.Background(), repo)
	require.NoError(t, err)
	var out []int64
	for _, mr := range list {
		out = append(out, mr.Number)
	}
	return out
}

func mrOn(t *testing.T, repo domain.RepoName, source string) domain.MergeRequest {
	mr := newMR(t, repo, "t")
	mr.Source = source
	return mr
}

// The index follows every write of a merge request: create, comment, source
// change, close and merge.
func TestOpenMergeRequestIndexFollowsTheWrites(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)

	a, err := mrs.Create(ctx, mrOn(t, n, "feat/a"))
	require.NoError(t, err)
	b, err := mrs.Create(ctx, mrOn(t, n, "feat/b"))
	require.NoError(t, err)
	c, err := mrs.Create(ctx, mrOn(t, n, "feat/a")) // a second open request on the same source
	require.NoError(t, err)
	assert.Equal(t, []int64{a.Number, c.Number}, bySource(t, mrs, n, "feat/a"))
	assert.Equal(t, []int64{b.Number}, bySource(t, mrs, n, "feat/b"))
	assert.Empty(t, bySource(t, mrs, n, "feat"), "a prefix of a branch is not the branch")
	assert.Empty(t, bySource(t, mrs, n, "feat/a/x"))
	assert.Equal(t, []int64{c.Number, b.Number, a.Number}, openNumbers(t, mrs, n), "highest number first")

	// A comment rewrites the request: the index stays.
	text, err := domain.ParsePlainText("hi")
	require.NoError(t, err)
	cm := domain.Comment{ID: 1, Author: "x", Text: text, CreatedAt: created}
	a.CommentSeq = 1
	require.NoError(t, mrs.SaveComment(ctx, a, cm))
	assert.Equal(t, []int64{a.Number, c.Number}, bySource(t, mrs, n, "feat/a"))

	// The source changes (a rewrite of the request): the key moves.
	b, err = mrs.Get(ctx, n, b.Number)
	require.NoError(t, err)
	b.Source = "feat/moved"
	require.NoError(t, mrs.Update(ctx, b))
	assert.Empty(t, bySource(t, mrs, n, "feat/b"))
	assert.Equal(t, []int64{b.Number}, bySource(t, mrs, n, "feat/moved"))

	// Close and merge remove the key.
	c, err = mrs.Get(ctx, n, c.Number)
	require.NoError(t, err)
	c.State = domain.MRStateClosed
	require.NoError(t, mrs.Update(ctx, c))
	assert.Equal(t, []int64{a.Number}, bySource(t, mrs, n, "feat/a"))
	a, err = mrs.Get(ctx, n, a.Number)
	require.NoError(t, err)
	a.State, a.MergeMode = domain.MRStateMerged, domain.MergeModeFastForward
	require.NoError(t, mrs.Update(ctx, a))
	assert.Empty(t, bySource(t, mrs, n, "feat/a"))
	assert.Equal(t, []int64{b.Number}, openNumbers(t, mrs, n))

	// Several branches in one lookup, each scanned on its own.
	assert.Equal(t, []int64{b.Number}, bySource(t, mrs, n, "feat/a", "feat/moved", "nothing"))

	// Another repository has its own.
	other := name(t, "other")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: other, CreatedAt: created, Visibility: domain.VisibilityPublic}))
	_, err = mrs.Create(ctx, mrOn(t, other, "feat/moved"))
	require.NoError(t, err)
	assert.Equal(t, []int64{b.Number}, bySource(t, mrs, n, "feat/moved"))
}

// The target index follows the same writes as the source one: create, comment,
// target change, close and merge.
func TestOpenMergeRequestTargetIndexFollowsTheWrites(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)

	a, err := mrs.Create(ctx, mrFromTo(t, n, "feat/a", "main"))
	require.NoError(t, err)
	b, err := mrs.Create(ctx, mrFromTo(t, n, "feat/b", "release/1"))
	require.NoError(t, err)
	c, err := mrs.Create(ctx, mrFromTo(t, n, "feat/c", "main"))
	require.NoError(t, err)
	assert.Equal(t, []int64{a.Number, c.Number}, byTarget(t, mrs, n, "main"))
	assert.Equal(t, []int64{b.Number}, byTarget(t, mrs, n, "release/1"))
	assert.Empty(t, byTarget(t, mrs, n, "release"), "a prefix of a branch is not the branch")
	assert.Empty(t, byTarget(t, mrs, n, "feat/a"), "a source is not a target")
	assert.Equal(t, []int64{a.Number}, bySource(t, mrs, n, "feat/a"), "and the other way round")

	text, err := domain.ParsePlainText("hi")
	require.NoError(t, err)
	a.CommentSeq = 1
	require.NoError(t, mrs.SaveComment(ctx, a, domain.Comment{ID: 1, Author: "x", Text: text, CreatedAt: created}))
	assert.Equal(t, []int64{a.Number, c.Number}, byTarget(t, mrs, n, "main"), "a comment keeps the index")

	b, err = mrs.Get(ctx, n, b.Number)
	require.NoError(t, err)
	b.Target = "main"
	require.NoError(t, mrs.Update(ctx, b))
	assert.Empty(t, byTarget(t, mrs, n, "release/1"), "a changed target moves the key")
	assert.Equal(t, []int64{a.Number, b.Number, c.Number}, byTarget(t, mrs, n, "main"))

	c, err = mrs.Get(ctx, n, c.Number)
	require.NoError(t, err)
	c.State = domain.MRStateClosed
	require.NoError(t, mrs.Update(ctx, c))
	a, err = mrs.Get(ctx, n, a.Number)
	require.NoError(t, err)
	a.State, a.MergeMode = domain.MRStateMerged, domain.MergeModeFastForward
	require.NoError(t, mrs.Update(ctx, a))
	assert.Equal(t, []int64{b.Number}, byTarget(t, mrs, n, "main"), "close and merge remove the key")

	// The keys of a deleted repository go with it, both indexes.
	require.NoError(t, s.Delete(ctx, n))
	assert.Empty(t, byTarget(t, mrs, n, "main"))
	assert.Empty(t, bySource(t, mrs, n, "feat/b"))
}

// The lookup stops at limit+1 keys, whatever the number of open requests on the
// branch, and says it was cut without counting the rest.
func TestOpenLookupStopsAtTheLimit(t *testing.T) {
	ctx := context.Background()
	_, mrs, n := repoWithMRs(t)
	for range 40 {
		_, err := mrs.Create(ctx, mrFromTo(t, n, "busy", "main"))
		require.NoError(t, err)
	}
	_, err := mrs.Create(ctx, mrFromTo(t, n, "calm", "main"))
	require.NoError(t, err)

	got, truncated, err := mrs.OpenBySource(ctx, n, []string{"busy"}, 5)
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.Equal(t, []int64{1, 2, 3, 4, 5}, got, "the first limit numbers")

	got, truncated, err = mrs.OpenByTarget(ctx, n, []string{"main"}, 7)
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.Len(t, got, 7)

	// Exactly limit keys is not truncated: only a limit+1th key is.
	got, truncated, err = mrs.OpenBySource(ctx, n, []string{"calm"}, 1)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Len(t, got, 1)
	got, truncated, err = mrs.OpenBySource(ctx, n, []string{"busy"}, 40)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Len(t, got, 40)

	// The limit spans the branches of one lookup.
	got, truncated, err = mrs.OpenBySource(ctx, n, []string{"calm", "busy"}, 3)
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.Len(t, got, 3)
}

// A cancelled context stops the lookup: nothing is scanned, and the caller
// sees the error of the context and that the result is partial.
func TestOpenLookupStopsOnCancel(t *testing.T) {
	_, mrs, n := repoWithMRs(t)
	for range 10 {
		_, err := mrs.Create(context.Background(), mrFromTo(t, n, "busy", "main"))
		require.NoError(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, truncated, err := mrs.OpenBySource(ctx, n, []string{"busy"}, 0)
	assert.ErrorIs(t, err, context.Canceled)
	assert.True(t, truncated)
	assert.Empty(t, got)
	_, truncated, err = mrs.OpenByTarget(ctx, n, []string{"main"}, 5)
	assert.ErrorIs(t, err, context.Canceled)
	assert.True(t, truncated)
}

// A branch name with the characters of a key separator stays in its own prefix.
func TestOpenMergeRequestIndexEscapesBranchNames(t *testing.T) {
	ctx := context.Background()
	_, mrs, n := repoWithMRs(t)
	x, err := mrs.Create(ctx, mrOn(t, n, "a/b"))
	require.NoError(t, err)
	y, err := mrs.Create(ctx, mrOn(t, n, "a%2Fb"))
	require.NoError(t, err)
	assert.Equal(t, []int64{x.Number}, bySource(t, mrs, n, "a/b"))
	assert.Equal(t, []int64{y.Number}, bySource(t, mrs, n, "a%2Fb"))
}

// Many historical merge requests are not read by a lookup of a push: the index
// holds the keys of the open ones, and only those are fetched.
func TestOpenBySourceDoesNotReadHistoricalMergeRequests(t *testing.T) {
	ctx := context.Background()
	_, mrs, n := repoWithMRs(t)
	for range 200 {
		mr, err := mrs.Create(ctx, mrOn(t, n, "old/closed"))
		require.NoError(t, err)
		mr.State = domain.MRStateClosed
		require.NoError(t, mrs.Update(ctx, mr))
	}
	open, err := mrs.Create(ctx, mrOn(t, n, "feat/live"))
	require.NoError(t, err)
	assert.Empty(t, bySource(t, mrs, n, "old/closed"), "none of the 200 closed ones is indexed")
	assert.Equal(t, []int64{open.Number}, bySource(t, mrs, n, "feat/live"))
	assert.Equal(t, []int64{open.Number}, openNumbers(t, mrs, n))
}

// Data that predates the index (a schema 2 database, or one an older build wrote
// to) is indexed once at open, and the index is not built twice.
func TestOpenIndexIsBackfilledOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: 2}))
	putRaw(t, dir, "repo/old", mustProto(t, &storagepb.Repo{Name: "old", Visibility: storagepb.Visibility_VISIBILITY_PUBLIC}))
	mrRaw := func(number int64, source string, state storagepb.MergeRequestState) []byte {
		return mustProto(t, &storagepb.MergeRequest{
			Number: number, Title: "t", SourceBranch: source, TargetBranch: "main", State: state, Author: "a",
		})
	}
	putRaw(t, dir, "mr/old/0000000001", mrRaw(1, "feat/one", storagepb.MergeRequestState_MERGE_REQUEST_STATE_OPEN))
	putRaw(t, dir, "mr/old/0000000002", mrRaw(2, "feat/one", storagepb.MergeRequestState_MERGE_REQUEST_STATE_CLOSED))
	putRaw(t, dir, "mr/old/0000000003", mrRaw(3, "feat/two", storagepb.MergeRequestState_MERGE_REQUEST_STATE_MERGED))
	putRaw(t, dir, "mr/old/0000000004", mrRaw(4, "feat/two", storagepb.MergeRequestState_MERGE_REQUEST_STATE_OPEN))
	putRaw(t, dir, "mrseq/old", mustProto(t, &storagepb.Sequence{Last: 4}))

	s := openStore(t, dir)
	mrs := s.MergeRequests()
	old := name(t, "old")
	assert.Equal(t, []int64{1}, bySource(t, mrs, old, "feat/one"))
	assert.Equal(t, []int64{4}, bySource(t, mrs, old, "feat/two"))
	assert.Equal(t, []int64{4, 1}, openNumbers(t, mrs, old))
	assert.Equal(t, []int64{1, 4}, byTarget(t, mrs, old, "main"), "the target index is built by the same pass")
	require.NoError(t, s.Close(ctx))

	// Opened again: the marker is there, nothing is built, and the index kept
	// following the writes in between.
	s2, err := kvstore.Open(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close(ctx) })
	m2 := s2.MergeRequests()
	five, err := m2.Create(ctx, mrOn(t, old, "feat/one"))
	require.NoError(t, err)
	assert.Equal(t, int64(5), five.Number)
	assert.Equal(t, []int64{1, 5}, bySource(t, m2, old, "feat/one"))
}
