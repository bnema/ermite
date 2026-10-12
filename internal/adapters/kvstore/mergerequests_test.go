// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	kv "bnema.dev/ermite-kv"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
)

func newMR(t *testing.T, repo domain.RepoName, title string) domain.MergeRequest {
	t.Helper()
	ti, err := domain.ParseMRTitle(title)
	require.NoError(t, err)
	desc, err := domain.ParsePlainText("why\nand how")
	require.NoError(t, err)
	return domain.MergeRequest{
		Repo: repo, Title: ti, Description: desc, Source: "feat/x", Target: "main",
		State: domain.MRStateOpen, Author: "agent", CreatedAt: created, UpdatedAt: created,
	}
}

func oid(c byte) domain.ObjectID {
	id, err := domain.ParseObjectID(strings.Repeat(string(c), 40))
	if err != nil {
		panic(err)
	}
	return id
}

// repoWithMRs opens a store holding repository "proj".
func repoWithMRs(t *testing.T) (*kvstore.Store, *kvstore.MergeRequests, domain.RepoName) {
	t.Helper()
	s := openStore(t, t.TempDir())
	n := name(t, "proj")
	require.NoError(t, s.Create(context.Background(), domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
	return s, s.MergeRequests(), n
}

func TestMergeRequestNumbersAreSequentialPerRepository(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)
	other := name(t, "other")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: other, CreatedAt: created, Visibility: domain.VisibilityPublic}))

	for want := int64(1); want <= 3; want++ {
		got, err := mrs.Create(ctx, newMR(t, n, "t"))
		require.NoError(t, err)
		assert.Equal(t, want, got.Number)
	}
	got, err := mrs.Create(ctx, newMR(t, other, "t"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.Number, "each repository counts on its own")

	_, err = mrs.Create(ctx, newMR(t, name(t, "ghost"), "t"))
	assert.ErrorIs(t, err, domain.ErrNotFound, "no merge request for a missing repository")
}

func TestMergeRequestRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, mrs, n := repoWithMRs(t)
	in := newMR(t, n, "Ajoute « ça »")
	in.HeadID, in.BaseID, in.MergedCommitID = oid('a'), oid('b'), oid('c')
	in.MergeMode = domain.MergeModeSquash
	in.State = domain.MRStateMerged
	in.CommentSeq = 4
	created1, err := mrs.Create(ctx, in)
	require.NoError(t, err)

	got, err := mrs.Get(ctx, n, created1.Number)
	require.NoError(t, err)
	assert.NotZero(t, got.Version)
	in.Number, in.Version = 1, got.Version
	assert.Equal(t, in, got)

	_, err = mrs.Get(ctx, n, 2)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = mrs.Get(ctx, n, 0)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = mrs.Get(ctx, n, -3)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMergeRequestListIsNewestFirstAndPerRepository(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)
	// "proj-2" shares the prefix "proj": its keys must not leak into proj's.
	sib := name(t, "proj-2")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: sib, CreatedAt: created, Visibility: domain.VisibilityPublic}))
	for range 12 { // beyond 10: numbers sort numerically, not as text
		_, err := mrs.Create(ctx, newMR(t, n, "t"))
		require.NoError(t, err)
	}
	_, err := mrs.Create(ctx, newMR(t, sib, "sibling"))
	require.NoError(t, err)

	list, err := mrs.List(ctx, n)
	require.NoError(t, err)
	require.Len(t, list, 12)
	for i, mr := range list {
		assert.Equal(t, int64(12-i), mr.Number)
		assert.Equal(t, n, mr.Repo)
	}
	list, err = mrs.List(ctx, name(t, "none"))
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestConcurrentCreateNeverSharesANumber(t *testing.T) {
	ctx := context.Background()
	_, mrs, n := repoWithMRs(t)
	const workers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		numbers []int64
	)
	for range workers {
		wg.Go(func() {
			got, err := mrs.Create(ctx, newMR(t, n, "t"))
			if assert.NoError(t, err) {
				mu.Lock()
				numbers = append(numbers, got.Number)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	assert.ElementsMatch(t, []int64{1, 2, 3, 4, 5, 6, 7, 8}, numbers)
}

func TestMergeRequestUpdateNeedsTheVersionItWasReadAt(t *testing.T) {
	ctx := context.Background()
	_, mrs, n := repoWithMRs(t)
	first, err := mrs.Create(ctx, newMR(t, n, "t"))
	require.NoError(t, err)

	a, err := mrs.Get(ctx, n, first.Number)
	require.NoError(t, err)
	b := a
	a.State = domain.MRStateClosed
	require.NoError(t, mrs.Update(ctx, a))

	b.State = domain.MRStateMerged
	assert.ErrorIs(t, mrs.Update(ctx, b), domain.ErrConflict, "b was read before a wrote")

	got, err := mrs.Get(ctx, n, first.Number)
	require.NoError(t, err)
	assert.Equal(t, domain.MRStateClosed, got.State)

	gone := got
	gone.Number = 99
	assert.ErrorIs(t, mrs.Update(ctx, gone), domain.ErrNotFound)
	fresh := newMR(t, n, "never read")
	fresh.Number = first.Number
	assert.ErrorIs(t, mrs.Update(ctx, fresh), domain.ErrConflict, "an unread request has no version")
}

func newComment(t *testing.T, id int64, text string) domain.Comment {
	t.Helper()
	pt, err := domain.ParsePlainText(text)
	require.NoError(t, err)
	return domain.Comment{
		ID: id, Author: "root", Path: "a.txt", Line: 3, Side: domain.SideNew,
		CommitID: oid('d'), Text: pt, CreatedAt: created.Add(time.Duration(id) * time.Minute),
	}
}

func TestComments(t *testing.T) {
	ctx := context.Background()
	_, mrs, n := repoWithMRs(t)
	first, err := mrs.Create(ctx, newMR(t, n, "t"))
	require.NoError(t, err)

	// Twelve comments: ids sort numerically too.
	for id := int64(1); id <= 12; id++ {
		mr, err := mrs.Get(ctx, n, first.Number)
		require.NoError(t, err)
		mr.CommentSeq = id
		c := newComment(t, id, "note")
		if id == 2 {
			c.Path, c.Line, c.Side = "", 0, domain.SideNone
		}
		require.NoError(t, mrs.SaveComment(ctx, mr, c))
	}
	list, err := mrs.ListComments(ctx, n, first.Number)
	require.NoError(t, err)
	require.Len(t, list, 12)
	for i, c := range list {
		assert.Equal(t, int64(i+1), c.ID, "oldest first")
	}
	assert.True(t, list[1].IsGeneral())
	assert.Equal(t, domain.SideNone, list[1].Side)
	assert.Equal(t, domain.SideNew, list[0].Side)

	mr, err := mrs.Get(ctx, n, first.Number)
	require.NoError(t, err)
	assert.Equal(t, int64(12), mr.CommentSeq)

	c, err := mrs.GetComment(ctx, n, first.Number, 3)
	require.NoError(t, err)
	assert.NotZero(t, c.Version)
	assert.Equal(t, "note", c.Text.String())
	assert.Equal(t, oid('d'), c.CommitID)
	for _, id := range []int64{0, 13, -1} {
		_, err = mrs.GetComment(ctx, n, first.Number, id)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}

	// Another merge request's comments are not listed.
	second, err := mrs.Create(ctx, newMR(t, n, "t2"))
	require.NoError(t, err)
	list, err = mrs.ListComments(ctx, n, second.Number)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestSaveCommentPreconditions(t *testing.T) {
	ctx := context.Background()
	_, mrs, n := repoWithMRs(t)
	first, err := mrs.Create(ctx, newMR(t, n, "t"))
	require.NoError(t, err)
	mr, err := mrs.Get(ctx, n, first.Number)
	require.NoError(t, err)

	mr.CommentSeq = 1
	require.NoError(t, mrs.SaveComment(ctx, mr, newComment(t, 1, "one")))

	// The same id again, from the same stale read: the request moved on.
	assert.ErrorIs(t, mrs.SaveComment(ctx, mr, newComment(t, 1, "dup")), domain.ErrConflict)

	// A fresh read of the request but a new comment with an existing id.
	mr, err = mrs.Get(ctx, n, first.Number)
	require.NoError(t, err)
	assert.ErrorIs(t, mrs.SaveComment(ctx, mr, newComment(t, 1, "dup")), domain.ErrConflict, "a new comment must not overwrite")

	// Resolving needs the comment version.
	c, err := mrs.GetComment(ctx, n, first.Number, 1)
	require.NoError(t, err)
	stale := c
	c.Resolved = true
	require.NoError(t, mrs.SaveComment(ctx, mr, c))
	mr, err = mrs.Get(ctx, n, first.Number)
	require.NoError(t, err)
	stale.Resolved = false
	assert.ErrorIs(t, mrs.SaveComment(ctx, mr, stale), domain.ErrConflict)

	// An unread request cannot be saved with.
	assert.ErrorIs(t, mrs.SaveComment(ctx, newMR(t, n, "x"), newComment(t, 2, "n")), domain.ErrConflict)
}

func TestReadPosition(t *testing.T) {
	ctx := context.Background()
	_, mrs, n := repoWithMRs(t)
	got, err := mrs.GetRead(ctx, n, 1)
	require.NoError(t, err)
	assert.True(t, got.IsZero())

	require.NoError(t, mrs.SetRead(ctx, n, 1, oid('a')))
	require.NoError(t, mrs.SetRead(ctx, n, 1, oid('b')))
	got, err = mrs.GetRead(ctx, n, 1)
	require.NoError(t, err)
	assert.Equal(t, oid('b'), got)
	got, err = mrs.GetRead(ctx, n, 2)
	require.NoError(t, err)
	assert.True(t, got.IsZero())
	assert.Error(t, mrs.SetRead(ctx, n, 1, domain.ObjectID{}))
}

// TestDeleteDropsTheMergeRequestsOfTheRepository: deleting a repository drops
// its merge requests, comments, read positions and number sequence, and only
// its own; a repository created again under the name starts at !1.
func TestDeleteDropsTheMergeRequestsOfTheRepository(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)
	sib := name(t, "proj-2")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: sib, CreatedAt: created, Visibility: domain.VisibilityPublic}))

	for _, repo := range []domain.RepoName{n, sib} {
		mr, err := mrs.Create(ctx, newMR(t, repo, "t"))
		require.NoError(t, err)
		mr.CommentSeq = 1
		require.NoError(t, mrs.SaveComment(ctx, mr, newComment(t, 1, "note")))
		require.NoError(t, mrs.SetRead(ctx, repo, mr.Number, oid('a')))
	}

	require.NoError(t, s.Delete(ctx, n))

	list, err := mrs.List(ctx, n)
	require.NoError(t, err)
	assert.Empty(t, list)
	comments, err := mrs.ListComments(ctx, n, 1)
	require.NoError(t, err)
	assert.Empty(t, comments)
	read, err := mrs.GetRead(ctx, n, 1)
	require.NoError(t, err)
	assert.True(t, read.IsZero())

	list, err = mrs.List(ctx, sib)
	require.NoError(t, err)
	require.Len(t, list, 1, "a repository whose name starts like the deleted one keeps its requests")
	comments, err = mrs.ListComments(ctx, sib, 1)
	require.NoError(t, err)
	assert.Len(t, comments, 1)

	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	again, err := mrs.Create(ctx, newMR(t, n, "fresh"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), again.Number)
}

// TestWritesAfterDeleteLeaveNothing: a comment or a read position written for
// a repository that was deleted meanwhile is refused, so that a repository
// created again under the name does not inherit it (GO-002).
func TestWritesAfterDeleteLeaveNothing(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)
	mr, err := mrs.Create(ctx, newMR(t, n, "t"))
	require.NoError(t, err)

	require.NoError(t, s.Delete(ctx, n))

	mr.CommentSeq = 1
	assert.ErrorIs(t, mrs.SaveComment(ctx, mr, newComment(t, 1, "late")), domain.ErrNotFound)
	assert.ErrorIs(t, mrs.SetRead(ctx, n, mr.Number, oid('a')), domain.ErrNotFound)
	_, err = mrs.Create(ctx, newMR(t, n, "late"))
	assert.ErrorIs(t, err, domain.ErrNotFound)

	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, CreatedAt: created, Visibility: domain.VisibilityPublic}))
	assertNoMergeRequestKeys(t, mrs, n)
}

func assertNoMergeRequestKeys(t *testing.T, mrs *kvstore.MergeRequests, n domain.RepoName) {
	t.Helper()
	ctx := context.Background()
	list, err := mrs.List(ctx, n)
	require.NoError(t, err)
	assert.Empty(t, list, "merge requests")
	comments, err := mrs.ListComments(ctx, n, 1)
	require.NoError(t, err)
	assert.Empty(t, comments, "comments")
	read, err := mrs.GetRead(ctx, n, 1)
	require.NoError(t, err)
	assert.True(t, read.IsZero(), "read position")
}

// TestDeleteRacingWithWrites: writes racing with Delete either land before it
// and are deleted, or are refused (not found after it, or a conflict when the
// retries run out under this contention); nothing is left for a repository
// created again under the name.
func TestDeleteRacingWithWrites(t *testing.T) {
	ctx := context.Background()
	for round := range 20 {
		s, mrs, n := repoWithMRs(t)
		mr, err := mrs.Create(ctx, newMR(t, n, "t"))
		require.NoError(t, err)

		var wg sync.WaitGroup
		for w := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range 30 {
					if w%2 == 0 {
						err := mrs.SetRead(ctx, n, mr.Number, oid('a'))
						if err != nil {
							assert.True(t, errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound), "round %d: %v", round, err)
							return
						}
						continue
					}
					cur, err := mrs.Get(ctx, n, mr.Number)
					if err != nil {
						return
					}
					cur.CommentSeq = int64(w*1000 + i + 1)
					err = mrs.SaveComment(ctx, cur, newComment(t, cur.CommentSeq, "note"))
					if err != nil && !assert.True(t, errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound), "round %d: %v", round, err) {
						return
					}
				}
			}()
		}
		delErr := s.Delete(ctx, n)
		wg.Wait()
		require.NoError(t, delErr)

		require.NoError(t, s.Create(ctx, domain.Repo{Name: n, CreatedAt: created, Visibility: domain.VisibilityPublic}))
		assertNoMergeRequestKeys(t, mrs, n)
		again, err := mrs.Create(ctx, newMR(t, n, "fresh"))
		require.NoError(t, err)
		assert.Equal(t, int64(1), again.Number)
	}
}

func TestDeleteManyMergeRequests(t *testing.T) {
	ctx := context.Background()
	s, mrs, n := repoWithMRs(t)
	for range 1100 {
		_, err := mrs.Create(ctx, newMR(t, n, "t"))
		require.NoError(t, err)
	}
	require.NoError(t, s.Delete(ctx, n))
	list, err := mrs.List(ctx, n)
	require.NoError(t, err)
	assert.Empty(t, list)
	_, err = s.Get(ctx, n)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestRepoSettings(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	n := name(t, "proj")

	// Created without settings: they are stored explicitly, with the defaults.
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	got, err := s.Get(ctx, n)
	require.NoError(t, err)
	assert.Equal(t, domain.DefaultRepoSettings(), got.Settings)

	got.Settings = domain.RepoSettings{AllowedMergeModes: []domain.MergeMode{domain.MergeModeSquash}, FailingChecksBlock: false}
	require.NoError(t, s.Update(ctx, got))
	again, err := s.Get(ctx, n)
	require.NoError(t, err)
	assert.Equal(t, got.Settings, again.Settings)

	got.Settings = domain.RepoSettings{AllowedMergeModes: []domain.MergeMode{"rebase"}}
	assert.ErrorIs(t, s.Update(ctx, got), domain.ErrInvalid)
}

// TestRepoWithoutStoredSettingsReadsTheDefaults: a repository stored before
// merge requests existed has no settings field.
func TestRepoWithoutStoredSettingsReadsTheDefaults(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := kvstore.Open(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, s.Close(ctx))

	// Write a legacy value straight into the database.
	db, err := kv.Open(ctx, dir, kv.Options{})
	require.NoError(t, err)
	legacy, err := proto.Marshal(&storagepb.Repo{Name: "legacy", Visibility: storagepb.Visibility_VISIBILITY_PUBLIC})
	require.NoError(t, err)
	var b kv.Batch
	b.Put([]byte("repo/legacy"), legacy)
	_, err = db.Apply(ctx, &b)
	require.NoError(t, err)
	require.NoError(t, db.Close(ctx))

	s = openStore(t, dir)
	got, err := s.Get(ctx, name(t, "legacy"))
	require.NoError(t, err)
	assert.Equal(t, domain.DefaultRepoSettings(), got.Settings)
	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, domain.DefaultRepoSettings(), list[0].Settings)
}

func TestSigningSeed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openStore(t, dir)
	calls := 0
	gen := func() ([]byte, error) {
		calls++
		return []byte(strings.Repeat(string(rune('a'+calls)), kvstore.SigningSeedLen)), nil
	}

	first, err := s.SigningSeed(ctx, gen, created)
	require.NoError(t, err)
	require.Len(t, first, kvstore.SigningSeedLen)
	second, err := s.SigningSeed(ctx, gen, created)
	require.NoError(t, err)
	assert.Equal(t, first, second, "generated once")
	assert.Equal(t, 1, calls)

	// It survives a restart.
	require.NoError(t, s.Close(ctx))
	s2 := openStore(t, dir)
	third, err := s2.SigningSeed(ctx, gen, created)
	require.NoError(t, err)
	assert.Equal(t, first, third)
	assert.Equal(t, 1, calls)
}

func TestSigningSeedConcurrentFirstStart(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		seeds [][]byte
		n     byte
	)
	for range 6 {
		wg.Go(func() {
			seed, err := s.SigningSeed(ctx, func() ([]byte, error) {
				mu.Lock()
				defer mu.Unlock()
				n++
				return []byte(strings.Repeat(string(rune('A'+n)), kvstore.SigningSeedLen)), nil
			}, created)
			if assert.NoError(t, err) {
				mu.Lock()
				seeds = append(seeds, seed)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for _, seed := range seeds {
		assert.Equal(t, seeds[0], seed, "every caller gets the winner's seed")
	}
}

func TestSigningSeedRejectsBadGenerators(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	_, err := s.SigningSeed(ctx, func() ([]byte, error) { return []byte("short"), nil }, created)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "short")
	_, err = s.SigningSeed(ctx, func() ([]byte, error) { return nil, assert.AnError }, created)
	require.ErrorIs(t, err, assert.AnError)
}
