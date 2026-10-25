// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func titles(es []domain.JournalEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Title
	}
	return out
}

// list is ListSince without a predicate.
func list(t *testing.T, j ports.JournalStore, since time.Time, repo domain.RepoName, limit int) []domain.JournalEntry {
	t.Helper()
	got, err := j.ListSince(context.Background(), since, "", repo, nil, limit)
	require.NoError(t, err)
	return got.Entries
}

// The journal lists newest first from the start of the range, keeps the
// order of entries of the same time, filters by repository and keeps every
// field through a restart.
func TestJournalOrderRangeAndFilter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openStore(t, dir)
	j := s.Journal()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a, b := name(t, "alpha"), name(t, "beta")
	head, err := domain.ParseObjectID(strings.Repeat("ab", 20))
	require.NoError(t, err)

	// Appended out of order on purpose: the key orders them.
	for _, e := range []domain.JournalEntry{
		{At: base.Add(2 * time.Hour), Repo: a, Kind: domain.JournalMergeRequestMerged, Title: "a2", Number: 4, Actor: "owner", Commit: head},
		{At: base, Repo: a, Kind: domain.JournalIssueOpened, Title: "a0"},
		{At: base.Add(time.Hour), Repo: b, Kind: domain.JournalTagCreated, Title: "b1", Ref: "v1.0.0"},
		{At: base.Add(time.Hour), Repo: a, Kind: domain.JournalDefaultBranchPush, Title: "a1-second", Commits: 3, More: true},
		{At: base.Add(-time.Hour), Repo: b, Kind: domain.JournalIssueClosed, Title: "old"},
	} {
		require.NoError(t, j.Append(ctx, e))
	}

	assert.Equal(t, []string{"a2", "a1-second", "b1", "a0"}, titles(list(t, j, base, domain.RepoName{}, 10)), "newest first; equal times by append order, latest first")
	assert.Equal(t, []string{"a2", "a1-second", "a0"}, titles(list(t, j, base, a, 10)))
	assert.Equal(t, []string{"a2", "a1-second"}, titles(list(t, j, base.Add(-24*time.Hour), domain.RepoName{}, 2)), "the limit keeps the newest")

	_, err = j.ListSince(ctx, base, "", domain.RepoName{}, nil, 0)
	assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
	assert.ErrorIs(t, j.Append(ctx, domain.JournalEntry{Repo: a}), domain.ErrInvalid, "no time")

	require.NoError(t, s.Close(ctx))
	s = openStore(t, dir)
	again := list(t, s.Journal(), base.Add(time.Hour), domain.RepoName{}, 10)
	require.Len(t, again, 3)
	got := again[0]
	assert.True(t, got.At.Equal(base.Add(2*time.Hour)))
	assert.Len(t, got.ID, domain.JournalIDLen)
	assert.Equal(t, domain.JournalEntry{
		ID: got.ID, At: got.At, Repo: a, Kind: domain.JournalMergeRequestMerged, Title: "a2", Number: 4, Actor: "owner", Commit: head,
	}, got)
	assert.Equal(t, 3, again[1].Commits)
	assert.True(t, again[1].More)
	assert.Equal(t, "v1.0.0", again[2].Ref)
}

// The sequence outlives the process: after a restart, an entry of the same
// time is ordered after those of the first process, whatever its repository.
func TestJournalOrderAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s := openStore(t, dir)
	for _, title := range []string{"z1", "z2", "z3"} {
		require.NoError(t, s.Journal().Append(ctx, domain.JournalEntry{At: at, Repo: name(t, "zeta"), Kind: domain.JournalIssueOpened, Title: title}))
	}
	require.NoError(t, s.Close(ctx))

	s = openStore(t, dir)
	require.NoError(t, s.Journal().Append(ctx, domain.JournalEntry{At: at, Repo: name(t, "alpha"), Kind: domain.JournalIssueOpened, Title: "a-after-restart"}))
	assert.Equal(t, []string{"a-after-restart", "z3", "z2", "z1"}, titles(list(t, s.Journal(), at, domain.RepoName{}, 10)))
}

// Many entries of one repository at one nanosecond, from several instances:
// none is refused, none overwrites another, the order is the append order.
func TestJournalManyEntriesAtOneTime(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	repo := name(t, "alpha")
	var want []string
	for i := range 12 {
		title := "e" + string(rune('a'+i))
		j := s.Journal() // a fresh instance each time, as after restarts
		require.NoError(t, j.Append(ctx, domain.JournalEntry{At: at, Repo: repo, Kind: domain.JournalIssueOpened, Title: title}))
		want = append([]string{title}, want...)
	}
	assert.Equal(t, want, titles(list(t, s.Journal(), at, repo, 20)))
}

// The repository filter and the predicate apply before the limit: the result
// is the newest limit accepted entries, and Truncated tells whether more
// were accepted, whatever was refused.
func TestJournalKeepLimitAndTruncated(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	j := s.Journal()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a, b := name(t, "alpha"), name(t, "alphab") // b's name starts with a's
	for i := range 7 {
		at := base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, j.Append(ctx, domain.JournalEntry{At: at, Repo: a, Kind: domain.JournalIssueOpened, Title: "a" + string(rune('0'+i))}))
		require.NoError(t, j.Append(ctx, domain.JournalEntry{At: at, Repo: b, Kind: domain.JournalIssueOpened, Title: "b" + string(rune('0'+i))}))
	}
	// Many refused entries newer than all the others.
	for i := range 50 {
		require.NoError(t, j.Append(ctx, domain.JournalEntry{At: base.Add(time.Hour + time.Duration(i)*time.Second), Repo: a, Kind: domain.JournalTagCreated, Title: "noise"}))
	}
	var seen int
	issuesOnly := func(e domain.JournalEntry) bool {
		seen++
		return e.Kind == domain.JournalIssueOpened
	}

	got, err := j.ListSince(ctx, base, "", a, issuesOnly, 3)
	require.NoError(t, err)
	assert.Equal(t, []string{"a6", "a5", "a4"}, titles(got.Entries), "the newest accepted; refused ones hide nothing")
	assert.True(t, got.Truncated)
	assert.Equal(t, len(got.Entries), cap(got.Entries), "a right-sized slice")
	assert.Equal(t, 57, seen, "each entry of the repository is weighed once")

	got, err = j.ListSince(ctx, base, "", a, issuesOnly, 7)
	require.NoError(t, err)
	assert.Len(t, got.Entries, 7)
	assert.False(t, got.Truncated, "exactly limit accepted")

	got, err = j.ListSince(ctx, base, "", domain.RepoName{}, issuesOnly, 20)
	require.NoError(t, err)
	assert.Len(t, got.Entries, 14)
	assert.False(t, got.Truncated)

	got, err = j.ListSince(ctx, base, "", domain.RepoName{}, nil, 64)
	require.NoError(t, err)
	assert.Len(t, got.Entries, 64)
	assert.Equal(t, "noise", got.Entries[0].Title)
	assert.False(t, got.Truncated, "no predicate keeps all")
	got, err = j.ListSince(ctx, base, "", domain.RepoName{}, nil, 63)
	require.NoError(t, err)
	assert.True(t, got.Truncated)
}

// A since before 1970, even outside the range of UnixNano, starts at the
// first entry; a since after 2262 is past the last.
func TestJournalSinceOutsideUnixNanoRange(t *testing.T) {
	ctx := context.Background()
	j := openStore(t, t.TempDir()).Journal()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i, title := range []string{"a", "b"} {
		require.NoError(t, j.Append(ctx, domain.JournalEntry{At: base.Add(time.Duration(i) * time.Hour), Repo: name(t, "alpha"), Kind: domain.JournalIssueOpened, Title: title}))
	}
	for _, since := range []time.Time{
		time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC),
	} {
		assert.Equal(t, []string{"b", "a"}, titles(list(t, j, since, domain.RepoName{}, 10)), "since %s", since)
	}
	assert.Empty(t, list(t, j, time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), domain.RepoName{}, 10))
}

func TestJournalPrune(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	j := s.Journal()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		require.NoError(t, j.Append(ctx, domain.JournalEntry{At: base.Add(time.Duration(i) * time.Hour), Repo: name(t, "alpha"), Kind: domain.JournalIssueOpened, Title: string(rune('a' + i))}))
	}
	n, err := j.Prune(ctx, base.Add(2*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 2, n, "strictly before")
	assert.Equal(t, []string{"e", "d", "c"}, titles(list(t, j, time.Time{}, domain.RepoName{}, 10)))

	n, err = j.Prune(ctx, base.Add(2*time.Hour))
	require.NoError(t, err)
	assert.Zero(t, n)
}

// A cancelled context stops the scans with its error.
func TestJournalCancelled(t *testing.T) {
	s := openStore(t, t.TempDir())
	j := s.Journal()
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, j.Append(ctx, domain.JournalEntry{At: time.Unix(1, 0), Repo: name(t, "alpha"), Kind: domain.JournalIssueOpened}))
	cancel()
	_, err := j.ListSince(ctx, time.Time{}, "", domain.RepoName{}, nil, 10)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = j.Prune(ctx, time.Unix(10, 0))
	assert.ErrorIs(t, err, context.Canceled)
}
