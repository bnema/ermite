// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
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

type inboxJournalRig struct {
	in      *usecase.Inbox
	meta    *mocks.RepoMetaStore
	journal *mocks.JournalStore
	repos   *mocks.GitRepos
	reader  *mocks.RepoReader
	now     time.Time
}

func newInboxJournalRig(t *testing.T) *inboxJournalRig {
	t.Helper()
	r := &inboxJournalRig{
		meta: mocks.NewRepoMetaStore(t), journal: mocks.NewJournalStore(t), repos: mocks.NewGitRepos(t), reader: mocks.NewRepoReader(t),
		now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC),
	}
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(r.now).Maybe()
	r.in = usecase.NewInbox(usecase.InboxConfig{
		Meta: r.meta, MergeRequests: mocks.NewMergeRequestStore(t), Issues: mocks.NewIssueStore(t),
		Clock: clock, Journal: r.journal, Repos: r.repos, Reader: r.reader,
	})
	return r
}

// scan answers ListSince over entries (newest first) the way the store does:
// the predicate and the limit apply in one pass, oldest last.
func scan(entries []domain.JournalEntry) func(context.Context, time.Time, string, domain.RepoName, func(domain.JournalEntry) bool, int) (ports.JournalList, error) {
	return func(_ context.Context, _ time.Time, _ string, _ domain.RepoName, keep func(domain.JournalEntry) bool, limit int) (ports.JournalList, error) {
		var out ports.JournalList
		for _, e := range entries {
			if keep != nil && !keep(e) {
				continue
			}
			if len(out.Entries) == limit {
				out.Truncated = true
				break
			}
			out.Entries = append(out.Entries, e)
		}
		return out, nil
	}
}

func TestInboxJournalIsOwnerOnly(t *testing.T) {
	t.Parallel()
	r := newInboxJournalRig(t) // nothing is read
	for _, p := range []domain.Principal{domain.Anonymous(), issuePrincipal(domain.ScopeIssueWrite)} {
		_, err := r.in.Journal(context.Background(), p, domain.RepoName{}, 24*time.Hour)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}
}

// The window starts range before now, the filter goes to the store, and the
// entries of a repository that is gone or was created again are left out.
func TestInboxJournalRangeFilterAndStaleRepositories(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newInboxJournalRig(t)
	proj, gone, reborn := name(t, "proj"), name(t, "gone"), name(t, "reborn")
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{
		{Name: proj, CreatedAt: r.now.Add(-30 * 24 * time.Hour), Archived: true},
		{Name: reborn, CreatedAt: r.now.Add(-time.Hour)},
	}, nil)
	entries := []domain.JournalEntry{
		{At: r.now.Add(-10 * time.Minute), Repo: proj, Kind: domain.JournalMergeRequestMerged, Number: 3},
		{At: r.now.Add(-20 * time.Minute), Repo: gone, Kind: domain.JournalIssueOpened, Number: 1},
		{At: r.now.Add(-2 * time.Hour), Repo: reborn, Kind: domain.JournalTagCreated, Ref: "v1"},
	}
	r.journal.EXPECT().ListSince(mock.Anything, r.now.Add(-7*24*time.Hour), "", domain.RepoName{}, mock.Anything, ports.MaxJournalEntries).RunAndReturn(scan(entries)).Once()
	// reborn has no branch yet: nothing to measure. proj is archived.
	r.repos.EXPECT().HeadBranch(mock.Anything, reborn).Return("main", nil)
	r.reader.EXPECT().ResolveRevision(mock.Anything, reborn, domain.Revision{}).Return(domain.ResolvedRevision{}, domain.ErrNotFound)

	j, err := r.in.Journal(ctx, domain.Owner(), domain.RepoName{}, 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, entries[:1], j.Entries)
	assert.False(t, j.Truncated)
	assert.Empty(t, j.Unreleased)

	r.journal.EXPECT().ListSince(mock.Anything, r.now.Add(-24*time.Hour), "", proj, mock.Anything, ports.MaxJournalEntries).RunAndReturn(scan(entries[:1])).Once()
	j, err = r.in.Journal(ctx, domain.Owner(), proj, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, entries[:1], j.Entries)
}

func TestInboxJournalSkipsTheIssuesOfARepositoryWithIssuesOff(t *testing.T) {
	t.Parallel()
	r := newInboxJournalRig(t)
	off := repoWithIssues(t, "quiet", domain.IssuesOff)
	off.Archived = true
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{off}, nil)
	entries := []domain.JournalEntry{
		{At: r.now.Add(-time.Minute), Repo: off.Name, Kind: domain.JournalIssueOpened, Title: "secret"},
		{At: r.now.Add(-2 * time.Minute), Repo: off.Name, Kind: domain.JournalIssueClosed, Title: "secret"},
		{At: r.now.Add(-3 * time.Minute), Repo: off.Name, Kind: domain.JournalTagCreated, Ref: "v1"},
	}
	r.journal.EXPECT().ListSince(mock.Anything, mock.Anything, "", mock.Anything, mock.Anything, mock.Anything).RunAndReturn(scan(entries)).Once()
	j, err := r.in.Journal(context.Background(), domain.Owner(), domain.RepoName{}, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, entries[2:], j.Entries)
}

// Entries left out (a repository gone, issues off) do not take the place of
// older ones: the filter goes to the store, read once.
func TestInboxJournalCapsAfterFiltering(t *testing.T) {
	t.Parallel()
	r := newInboxJournalRig(t)
	proj, gone := name(t, "proj"), name(t, "gone")
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: proj, Archived: true, CreatedAt: r.now.Add(-48 * time.Hour)}}, nil)
	at := func(i int) time.Time { return r.now.Add(-time.Duration(i) * time.Second) }

	// More than MaxJournalEntries newer entries of a deleted repository, then
	// the two that remain.
	var all []domain.JournalEntry
	for i := range 2 * ports.MaxJournalEntries {
		all = append(all, domain.JournalEntry{At: at(i), Repo: gone, Kind: domain.JournalTagCreated})
	}
	kept := []domain.JournalEntry{
		{At: at(len(all)), Repo: proj, Kind: domain.JournalTagCreated, Ref: "v2"},
		{At: at(len(all) + 1), Repo: proj, Kind: domain.JournalTagCreated, Ref: "v1"},
	}
	all = append(all, kept...)
	r.journal.EXPECT().ListSince(mock.Anything, mock.Anything, "", domain.RepoName{}, mock.Anything, ports.MaxJournalEntries).RunAndReturn(scan(all)).Once()

	j, err := r.in.Journal(context.Background(), domain.Owner(), domain.RepoName{}, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, kept, j.Entries)
	assert.False(t, j.Truncated, "only two entries are eligible")
}

// Truncated counts the eligible entries: exactly MaxJournalEntries of them is
// not truncated, whatever was left out.
func TestInboxJournalTruncatedCountsEligibleEntries(t *testing.T) {
	t.Parallel()
	r := newInboxJournalRig(t)
	proj, gone := name(t, "proj"), name(t, "gone")
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: proj, Archived: true, CreatedAt: r.now.Add(-48 * time.Hour)}}, nil)
	page := make([]domain.JournalEntry, 0, ports.MaxJournalEntries+1)
	for i := range ports.MaxJournalEntries + 1 {
		repo := proj
		if i == 7 {
			repo = gone
		}
		page = append(page, domain.JournalEntry{At: r.now.Add(-time.Duration(i) * time.Second), Repo: repo, Kind: domain.JournalTagCreated})
	}
	r.journal.EXPECT().ListSince(mock.Anything, mock.Anything, "", mock.Anything, mock.Anything, mock.Anything).RunAndReturn(scan(page)).Once()
	j, err := r.in.Journal(context.Background(), domain.Owner(), domain.RepoName{}, 24*time.Hour)
	require.NoError(t, err)
	assert.Len(t, j.Entries, ports.MaxJournalEntries)
	assert.False(t, j.Truncated)
}

func TestInboxJournalTruncates(t *testing.T) {
	t.Parallel()
	r := newInboxJournalRig(t)
	proj := name(t, "proj")
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: proj, Archived: true, CreatedAt: r.now.Add(-time.Hour)}}, nil)
	many := make([]domain.JournalEntry, ports.MaxJournalEntries+1)
	for i := range many {
		many[i] = domain.JournalEntry{At: r.now.Add(-time.Duration(i) * time.Second), Repo: proj, Kind: domain.JournalTagCreated}
	}
	r.journal.EXPECT().ListSince(mock.Anything, mock.Anything, "", mock.Anything, mock.Anything, mock.Anything).RunAndReturn(scan(many)).Once()
	j, err := r.in.Journal(context.Background(), domain.Owner(), domain.RepoName{}, 24*time.Hour)
	require.NoError(t, err)
	assert.Len(t, j.Entries, ports.MaxJournalEntries)
	assert.True(t, j.Truncated)
	assert.Equal(t, many[:ports.MaxJournalEntries], j.Entries, "the newest")
}

// Only repositories ahead of a tag are listed: not those without a tag, nor
// one whose head is tagged, nor one whose walk fails, which is skipped.
func TestInboxJournalUnreleased(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newInboxJournalRig(t)
	ahead, far, notag, tagged, broken, detached := name(t, "ahead"), name(t, "far"), name(t, "notag"), name(t, "tagged"), name(t, "broken"), name(t, "detached")
	var repos []domain.Repo
	for _, n := range []domain.RepoName{ahead, broken, detached, far, notag, tagged} {
		repos = append(repos, domain.Repo{Name: n})
	}
	r.meta.EXPECT().List(mock.Anything).Return(repos, nil)
	r.journal.EXPECT().ListSince(mock.Anything, mock.Anything, "", mock.Anything, mock.Anything, mock.Anything).Return(ports.JournalList{}, nil)
	tagAt := r.now.Add(-72 * time.Hour)
	for n, d := range map[domain.RepoName]struct {
		d  domain.TagDistance
		ok bool
	}{
		ahead:  {domain.TagDistance{Tag: domain.TagInfo{Name: "v0.12.0", At: tagAt}, Commits: 4}, true},
		far:    {domain.TagDistance{Commits: domain.MaxTagDistance, More: true}, true},
		notag:  {domain.TagDistance{}, false},
		tagged: {domain.TagDistance{Tag: domain.TagInfo{Name: "v2"}}, true},
	} {
		r.repos.EXPECT().HeadBranch(mock.Anything, n).Return("main", nil)
		r.reader.EXPECT().ResolveRevision(mock.Anything, n, domain.Revision{}).Return(domain.ResolvedRevision{CommitID: oid("a")}, nil)
		r.reader.EXPECT().TagDistance(mock.Anything, n, oid("a"), domain.MaxTagDistance).Return(d.d, d.ok, nil)
	}
	r.repos.EXPECT().HeadBranch(mock.Anything, broken).Return("trunk", nil)
	r.reader.EXPECT().ResolveRevision(mock.Anything, broken, domain.Revision{}).Return(domain.ResolvedRevision{CommitID: oid("b")}, nil)
	r.reader.EXPECT().TagDistance(mock.Anything, broken, oid("b"), domain.MaxTagDistance).Return(domain.TagDistance{}, false, errors.New("corrupt pack"))
	r.repos.EXPECT().HeadBranch(mock.Anything, detached).Return("", nil)

	j, err := r.in.Journal(ctx, domain.Owner(), domain.RepoName{}, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, []ports.UnreleasedRepo{
		{Repo: ahead, Branch: "main", Distance: domain.TagDistance{Tag: domain.TagInfo{Name: "v0.12.0", At: tagAt}, Commits: 4}},
		{Repo: far, Branch: "main", Distance: domain.TagDistance{Commits: domain.MaxTagDistance, More: true}},
	}, j.Unreleased, "by name, broken skipped")
}

func TestInboxJournalUnreleasedRespectsTheFilter(t *testing.T) {
	t.Parallel()
	r := newInboxJournalRig(t)
	a, b := name(t, "aaa"), name(t, "bbb")
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: a}, {Name: b}}, nil)
	r.journal.EXPECT().ListSince(mock.Anything, mock.Anything, "", b, mock.Anything, mock.Anything).Return(ports.JournalList{}, nil)
	r.repos.EXPECT().HeadBranch(mock.Anything, b).Return("main", nil).Once() // aaa is never read
	r.reader.EXPECT().ResolveRevision(mock.Anything, b, domain.Revision{}).Return(domain.ResolvedRevision{CommitID: oid("c")}, nil)
	r.reader.EXPECT().TagDistance(mock.Anything, b, oid("c"), domain.MaxTagDistance).Return(domain.TagDistance{Commits: 1, Tag: domain.TagInfo{Name: "v1"}}, true, nil)
	j, err := r.in.Journal(context.Background(), domain.Owner(), b, 24*time.Hour)
	require.NoError(t, err)
	require.Len(t, j.Unreleased, 1)
	assert.Equal(t, b, j.Unreleased[0].Repo)
}

// The tag warning of a repository is measured once: a refresh for something
// else (a comment, a run) reuses it; a push, a new head, a new default branch
// or the repository created again measures again. A failed walk is not kept.
func TestInboxJournalCachesTagWarnings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newInboxJournalRig(t)
	proj := name(t, "proj")
	repo := domain.Repo{Name: proj, CreatedAt: r.now.Add(-48 * time.Hour)}
	r.meta.EXPECT().List(mock.Anything).RunAndReturn(func(context.Context) ([]domain.Repo, error) { return []domain.Repo{repo}, nil })
	r.journal.EXPECT().ListSince(mock.Anything, mock.Anything, "", mock.Anything, mock.Anything, mock.Anything).Return(ports.JournalList{}, nil)
	branch, head := "main", oid("a")
	r.repos.EXPECT().HeadBranch(mock.Anything, proj).RunAndReturn(func(context.Context, domain.RepoName) (string, error) { return branch, nil })
	r.reader.EXPECT().ResolveRevision(mock.Anything, proj, domain.Revision{}).RunAndReturn(func(context.Context, domain.RepoName, domain.Revision) (domain.ResolvedRevision, error) {
		return domain.ResolvedRevision{CommitID: head}, nil
	})
	four := domain.TagDistance{Commits: 4, Tag: domain.TagInfo{Name: "v1"}}
	journal := func() ports.InboxJournal {
		t.Helper()
		j, err := r.in.Journal(ctx, domain.Owner(), domain.RepoName{}, 24*time.Hour)
		require.NoError(t, err)
		return j
	}

	r.reader.EXPECT().TagDistance(mock.Anything, proj, oid("a"), domain.MaxTagDistance).Return(four, true, nil).Once()
	require.Len(t, journal().Unreleased, 1)
	require.Len(t, journal().Unreleased, 1, "an unrelated refresh: no walk")

	r.in.RefsChanged(proj) // a push (a tag deleted: the head is the same)
	r.reader.EXPECT().TagDistance(mock.Anything, proj, oid("a"), domain.MaxTagDistance).Return(domain.TagDistance{}, false, nil).Once()
	assert.Empty(t, journal().Unreleased)
	assert.Empty(t, journal().Unreleased, "no tag is cached too")

	head = oid("b") // the head moved without a push (a merge)
	r.reader.EXPECT().TagDistance(mock.Anything, proj, oid("b"), domain.MaxTagDistance).Return(domain.TagDistance{}, false, errors.New("corrupt pack")).Once()
	assert.Empty(t, journal().Unreleased)
	r.reader.EXPECT().TagDistance(mock.Anything, proj, oid("b"), domain.MaxTagDistance).Return(four, true, nil).Once()
	assert.Len(t, journal().Unreleased, 1, "a failure is tried again")

	branch = "trunk"
	r.reader.EXPECT().TagDistance(mock.Anything, proj, oid("b"), domain.MaxTagDistance).Return(four, true, nil).Once()
	assert.Equal(t, "trunk", journal().Unreleased[0].Branch, "another default branch")

	repo.CreatedAt = r.now.Add(-time.Hour) // deleted and created again
	r.reader.EXPECT().TagDistance(mock.Anything, proj, oid("b"), domain.MaxTagDistance).Return(four, true, nil).Once()
	assert.Len(t, journal().Unreleased, 1)
	assert.Len(t, journal().Unreleased, 1)
}

func TestInboxJournalWithoutAJournalIsEmpty(t *testing.T) {
	t.Parallel()
	in, _, _, _ := newInboxUnit(t)
	j, err := in.Journal(context.Background(), domain.Owner(), domain.RepoName{}, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, ports.InboxJournal{}, j)
}
