// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

func journalToken(scopes []domain.Scope, repos ...domain.RepoName) domain.Principal {
	return domain.TokenPrincipal(domain.Token{ID: "t", Label: "agent", Scopes: scopes, Repos: repos})
}

func journalID(n int) string { return strings.Repeat("0", 31) + string(rune('0'+n)) }

// Anonymous callers are unauthenticated, other callers without owner rights
// or journal:read forbidden; nothing is read for them.
func TestJournalAPIAccess(t *testing.T) {
	t.Parallel()
	r := newInboxJournalRig(t)
	ctx := context.Background()
	q := ports.ActivityQuery{Since: r.now.Add(-time.Hour)}
	_, err := r.in.ListActivity(ctx, domain.Anonymous(), q)
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	_, err = r.in.ListUnreleased(ctx, domain.Anonymous(), domain.RepoName{})
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	others := journalToken([]domain.Scope{domain.ScopeRepoRead, domain.ScopeReleaseWrite, domain.ScopeIssueWrite})
	_, err = r.in.ListActivity(ctx, others, q)
	assert.ErrorIs(t, err, domain.ErrForbidden)
	_, err = r.in.ListUnreleased(ctx, others, domain.RepoName{})
	assert.ErrorIs(t, err, domain.ErrForbidden)
	_, err = r.in.Journal(ctx, journalToken([]domain.Scope{domain.ScopeJournalRead}), time.Hour)
	assert.ErrorIs(t, err, domain.ErrNotFound, "the web inbox stays owner-only")
}

func TestJournalAPIValidation(t *testing.T) {
	t.Parallel()
	r := newInboxJournalRig(t) // nothing is read
	ctx := context.Background()
	for name, q := range map[string]ports.ActivityQuery{
		"no since":     {},
		"future since": {Since: r.now.Add(time.Second)},
		"unknown kind": {Since: r.now, Kinds: []domain.JournalKind{"merged"}},
		"negative":     {Since: r.now, PageSize: -1},
		"bad token":    {Since: r.now, PageToken: "nope"},
		"other token":  {Since: r.now, PageToken: base64.RawURLEncoding.EncodeToString([]byte("tp1:" + journalID(1)))},
		"bad id":       {Since: r.now, PageToken: base64.RawURLEncoding.EncodeToString([]byte("jr1:xyz"))},
	} {
		_, err := r.in.ListActivity(ctx, domain.Owner(), q)
		assert.True(t, domain.IsInvalid(err), "%s: %v", name, err)
	}
}

// The query reaches the store in one scan: since, the cursor, the repo, the
// page size (default 100, at most 500); the next token names the last entry
// and comes back as the cursor.
func TestJournalAPIPaging(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newInboxJournalRig(t)
	proj := name(t, "proj")
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: proj, CreatedAt: r.now.Add(-48 * time.Hour)}}, nil)
	since := r.now.Add(-40 * 24 * time.Hour) // beyond the retention: accepted
	first := []domain.JournalEntry{
		{ID: journalID(3), At: r.now.Add(-time.Minute), Repo: proj, Kind: domain.JournalTagCreated, Ref: "v2"},
		{ID: journalID(2), At: r.now.Add(-2 * time.Minute), Repo: proj, Kind: domain.JournalTagCreated, Ref: "v1"},
	}
	r.journal.EXPECT().ListSince(mock.Anything, since, "", domain.RepoName{}, mock.Anything, ports.DefaultActivityPageSize).
		Return(ports.JournalList{Entries: first, Truncated: true}, nil).Once()
	page, err := r.in.ListActivity(ctx, domain.Owner(), ports.ActivityQuery{Since: since})
	require.NoError(t, err)
	assert.Equal(t, first, page.Entries)
	require.NotEmpty(t, page.NextPageToken)

	r.journal.EXPECT().ListSince(mock.Anything, since, journalID(2), proj, mock.Anything, ports.MaxActivityPageSize).
		Return(ports.JournalList{Entries: first[:0]}, nil).Once()
	page, err = r.in.ListActivity(ctx, domain.Owner(), ports.ActivityQuery{Since: since, Repo: proj, PageSize: 5000, PageToken: page.NextPageToken})
	require.NoError(t, err)
	assert.Empty(t, page.Entries)
	assert.Empty(t, page.NextPageToken, "the end")
}

// Over the real store, a since long before 1970 asks for all the retained
// activity and gets it.
func TestJournalAPISinceBeforeUnixEpochOverStore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := kvstore.Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close(ctx)) })
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	in := usecase.NewInbox(usecase.InboxConfig{
		Meta: s, MergeRequests: mocks.NewMergeRequestStore(t), Issues: mocks.NewIssueStore(t), Clock: clock, Journal: s.Journal(),
	})
	proj := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: proj, Visibility: domain.VisibilityPrivate, CreatedAt: now.Add(-48 * time.Hour)}))
	for i, ref := range []string{"v1", "v2"} {
		require.NoError(t, s.Journal().Append(ctx, domain.JournalEntry{At: now.Add(time.Duration(i-2) * time.Hour), Repo: proj, Kind: domain.JournalTagCreated, Ref: ref}))
	}

	page, err := in.ListActivity(ctx, domain.Owner(), ports.ActivityQuery{Since: time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	require.Len(t, page.Entries, 2)
	assert.Equal(t, "v2", page.Entries[0].Ref)
	assert.Equal(t, "v1", page.Entries[1].Ref)
	assert.Empty(t, page.NextPageToken)
}

// The predicate keeps the kinds asked, leaves out the issues of a repository
// whose issues are off and the repositories outside a token's list.
func TestJournalAPIFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newInboxJournalRig(t)
	created := r.now.Add(-48 * time.Hour)
	mine, other := repoWithIssues(t, "mine", domain.IssuesOwner), repoWithIssues(t, "other", domain.IssuesOwner)
	quiet := repoWithIssues(t, "quiet", domain.IssuesOff)
	for _, repo := range []*domain.Repo{&mine, &other, &quiet} {
		repo.CreatedAt = created
	}
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{mine, other, quiet}, nil)
	entries := []domain.JournalEntry{
		{ID: journalID(6), At: r.now.Add(-1 * time.Minute), Repo: mine.Name, Kind: domain.JournalMergeRequestMerged, Number: 4},
		{ID: journalID(5), At: r.now.Add(-2 * time.Minute), Repo: other.Name, Kind: domain.JournalMergeRequestMerged, Number: 5},
		{ID: journalID(4), At: r.now.Add(-3 * time.Minute), Repo: mine.Name, Kind: domain.JournalIssueOpened, Number: 6},
		{ID: journalID(3), At: r.now.Add(-4 * time.Minute), Repo: quiet.Name, Kind: domain.JournalIssueOpened, Number: 7},
		{ID: journalID(2), At: r.now.Add(-5 * time.Minute), Repo: quiet.Name, Kind: domain.JournalTagCreated, Ref: "v1"},
	}
	r.journal.EXPECT().ListSince(mock.Anything, mock.Anything, "", mock.Anything, mock.Anything, mock.Anything).RunAndReturn(scan(entries))

	page, err := r.in.ListActivity(ctx, domain.Owner(), ports.ActivityQuery{Since: r.now.Add(-time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, []domain.JournalEntry{entries[0], entries[1], entries[2], entries[4]}, page.Entries, "issues off left out")

	page, err = r.in.ListActivity(ctx, domain.Owner(), ports.ActivityQuery{Since: r.now.Add(-time.Hour), Kinds: []domain.JournalKind{domain.JournalIssueOpened, domain.JournalTagCreated}})
	require.NoError(t, err)
	assert.Equal(t, []domain.JournalEntry{entries[2], entries[4]}, page.Entries)

	reader := journalToken([]domain.Scope{domain.ScopeJournalRead})
	page, err = r.in.ListActivity(ctx, reader, ports.ActivityQuery{Since: r.now.Add(-time.Hour)})
	require.NoError(t, err)
	assert.Len(t, page.Entries, 4, "journal:read sees what the owner sees")

	limited := journalToken([]domain.Scope{domain.ScopeJournalRead}, mine.Name)
	page, err = r.in.ListActivity(ctx, limited, ports.ActivityQuery{Since: r.now.Add(-time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, []domain.JournalEntry{entries[0], entries[2]}, page.Entries, "only its repositories")

	_, err = r.in.ListActivity(ctx, limited, ports.ActivityQuery{Since: r.now.Add(-time.Hour), Repo: other.Name})
	assert.ErrorIs(t, err, domain.ErrNotFound, "a repository outside the list")
	_, err = r.in.ListActivity(ctx, domain.Owner(), ports.ActivityQuery{Since: r.now.Add(-time.Hour), Repo: name(t, "gone")})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// ListUnreleased reads the tag warnings the inbox caches, limited to a
// token's repositories.
func TestJournalAPIListUnreleased(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newInboxJournalRig(t)
	a, b := name(t, "aaa"), name(t, "bbb")
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: a}, {Name: b}}, nil)
	four := domain.TagDistance{Commits: 4, Tag: domain.TagInfo{Name: "v1"}}
	for _, n := range []domain.RepoName{a, b} {
		r.repos.EXPECT().HeadBranch(mock.Anything, n).Return("main", nil)
		r.reader.EXPECT().ResolveRevision(mock.Anything, n, domain.Revision{}).Return(domain.ResolvedRevision{CommitID: oid("a")}, nil)
		r.reader.EXPECT().TagDistance(mock.Anything, n, oid("a"), domain.MaxTagDistance).Return(four, true, nil).Once()
	}

	got, err := r.in.ListUnreleased(ctx, domain.Owner(), domain.RepoName{})
	require.NoError(t, err)
	assert.Equal(t, []ports.UnreleasedRepo{{Repo: a, Branch: "main", Distance: four}, {Repo: b, Branch: "main", Distance: four}}, got)

	got, err = r.in.ListUnreleased(ctx, journalToken([]domain.Scope{domain.ScopeJournalRead}, b), domain.RepoName{})
	require.NoError(t, err)
	assert.Equal(t, []ports.UnreleasedRepo{{Repo: b, Branch: "main", Distance: four}}, got, "cached (TagDistance once), and only its repositories")

	_, err = r.in.ListUnreleased(ctx, journalToken([]domain.Scope{domain.ScopeJournalRead}, b), a)
	assert.ErrorIs(t, err, domain.ErrNotFound, "a repository outside the list")
}
