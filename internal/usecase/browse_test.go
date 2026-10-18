// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

type browseFixture struct {
	meta   *mocks.RepoMetaStore
	reader *mocks.RepoReader
	browse *usecase.Browse
}

func newBrowseFixture(t *testing.T) *browseFixture {
	t.Helper()
	f := &browseFixture{meta: mocks.NewRepoMetaStore(t), reader: mocks.NewRepoReader(t)}
	f.browse = usecase.NewBrowse(f.meta, f.reader)
	return f
}

func oid(c string) domain.ObjectID {
	id, err := domain.ParseObjectID(strings.Repeat(c, 40))
	if err != nil {
		panic(err)
	}
	return id
}

// browseOps are the read use cases, each as a call that returns its error.
// The reader expectation of each is what must (not) run.
type browseOp struct {
	name string
	call func(b *usecase.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error
	// expect registers the reader call a permitted caller makes.
	expect func(r *mocks.RepoReader)
}

func browseOps() []browseOp {
	root := domain.TreePath{}
	return []browseOp{
		{"ListRefs", func(b *usecase.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.ListRefs(ctx, p, repo, ports.ListRefsParams{})
			return err
		}, func(r *mocks.RepoReader) { r.EXPECT().ListRefs(mock.Anything, mock.Anything).Return(nil, nil) }},
		{"ResolveRevision", func(b *usecase.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.ResolveRevision(ctx, p, repo, domain.Revision{})
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().ResolveRevision(mock.Anything, mock.Anything, mock.Anything).Return(domain.ResolvedRevision{}, nil)
		}},
		{"ResolveRefPath", func(b *usecase.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.ResolveRefPath(ctx, p, repo, "main/x")
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().ResolveRefPath(mock.Anything, mock.Anything, "main/x").Return(domain.RefPath{}, nil)
		}},
		{"GetTree", func(b *usecase.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.GetTree(ctx, p, repo, domain.Revision{}, root, true)
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().GetTree(mock.Anything, mock.Anything, mock.Anything, mock.Anything, true).Return(domain.Tree{}, nil)
		}},
		{"ListPaths", func(b *usecase.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.ListPaths(ctx, p, repo, domain.Revision{})
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().ListPaths(mock.Anything, mock.Anything, mock.Anything, domain.MaxPathEntries, domain.MaxPathBytes).Return(domain.PathList{}, nil)
		}},
		{"OpenBlob", func(b *usecase.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, rc, err := b.OpenBlob(ctx, p, repo, domain.Revision{}, root)
			if rc != nil {
				_ = rc.Close()
			}
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().OpenBlob(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
				Return(domain.BlobInfo{}, io.NopCloser(strings.NewReader("")), nil)
		}},
		{"ListCommits", func(b *usecase.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.ListCommits(ctx, p, repo, ports.ListCommitsParams{})
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything).Return(ports.CommitPage{}, nil)
		}},
		{"GetCommit", func(b *usecase.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.GetCommit(ctx, p, repo, domain.Revision{})
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().GetCommit(mock.Anything, mock.Anything, mock.Anything, domain.DefaultDiffLimits()).Return(domain.CommitDetail{}, nil)
		}},
		{"RepoStats", func(b *usecase.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.RepoStats(ctx, p, repo, oid("a"), time.Now())
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().CountCommits(mock.Anything, mock.Anything, mock.Anything).Return(1, nil)
			r.EXPECT().CommitTimes(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, false, nil)
			r.EXPECT().LatestTag(mock.Anything, mock.Anything).Return(domain.TagInfo{}, false, nil)
		}},
	}
}

// RepoStats reads Git once per state of the repository (head and last push)
// and per 10 minutes; concurrent callers share the read, and a failure is
// not kept.
func TestRepoStatsCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBrowseFixture(t)
	repo := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic, CreatedAt: time.Unix(100, 0), PushedAt: time.Unix(200, 0)}
	var mu sync.Mutex
	f.meta.EXPECT().Get(mock.Anything, repo.Name).RunAndReturn(func(context.Context, domain.RepoName) (domain.Repo, error) {
		mu.Lock()
		defer mu.Unlock()
		return repo, nil
	})
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	var reads atomic.Int32
	gate := make(chan struct{})
	f.reader.EXPECT().CountCommits(mock.Anything, repo.Name, mock.Anything).RunAndReturn(
		func(context.Context, domain.RepoName, domain.ObjectID) (int, error) {
			reads.Add(1)
			<-gate
			return 900, nil
		})
	recent := make([]time.Time, 12)
	for i := range recent {
		recent[i] = now.Add(-time.Duration(i) * time.Hour)
	}
	f.reader.EXPECT().CommitTimes(mock.Anything, repo.Name, mock.Anything, mock.Anything).Return(recent, false, nil)
	tag := domain.TagInfo{Name: "v1", At: now, CommitID: oid("c")}
	f.reader.EXPECT().LatestTag(mock.Anything, repo.Name).Return(tag, true, nil)

	stats := func(head string, at time.Time) ports.RepoStats {
		st, err := f.browse.RepoStats(ctx, domain.Anonymous(), repo.Name, oid(head), at)
		require.NoError(t, err)
		return st
	}

	// Eight concurrent requests: one read.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			st := stats("a", now)
			assert.Equal(t, 900, st.Commits)
			assert.Equal(t, 12, st.Recent)
			assert.Equal(t, tag, *st.LatestTag)
			require.Len(t, st.Activity, ports.ActivityDays)
			assert.Equal(t, 12, st.Activity[ports.ActivityDays-1], "all in the last day")
		})
	}
	for reads.Load() == 0 {
		runtime.Gosched()
	}
	close(gate)
	wg.Wait()
	assert.EqualValues(t, 1, reads.Load(), "concurrent callers share the read")

	stats("a", now.Add(9*time.Minute))
	assert.EqualValues(t, 1, reads.Load(), "served from the cache")
	stats("a", now.Add(10*time.Minute))
	assert.EqualValues(t, 2, reads.Load(), "expired")
	stats("b", now.Add(10*time.Minute))
	assert.EqualValues(t, 3, reads.Load(), "another head")
	mu.Lock()
	repo.PushedAt = time.Unix(300, 0) // a push, a tag-only one included
	mu.Unlock()
	stats("b", now.Add(10*time.Minute))
	assert.EqualValues(t, 4, reads.Load(), "a push forgets the figures")
}

// The activity spreads the commits over the last days, oldest first, and
// the recent count covers 30 days.
func TestRepoStatsActivity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBrowseFixture(t)
	repo := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	f.meta.EXPECT().Get(ctx, repo.Name).Return(repo, nil)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	times := []time.Time{
		now.Add(time.Hour),    // in the future: the last day
		now.Add(-time.Minute), // the last day
		now.Add(-day - 1),     // the day before
		now.Add(-29 * day),    // recent, day 29 ago
		now.Add(-31 * day),    // not recent
		now.Add(-89*day - 1),  // the first day of the span
	}
	f.reader.EXPECT().CountCommits(mock.Anything, repo.Name, oid("a")).Return(100, nil)
	f.reader.EXPECT().CommitTimes(mock.Anything, repo.Name, oid("a"), now.Add(-ports.ActivityDays*day)).Return(times, false, nil)
	f.reader.EXPECT().LatestTag(mock.Anything, repo.Name).Return(domain.TagInfo{}, false, nil)
	st, err := f.browse.RepoStats(ctx, domain.Anonymous(), repo.Name, oid("a"), now)
	require.NoError(t, err)
	assert.Equal(t, 4, st.Recent)
	last := ports.ActivityDays - 1
	assert.Equal(t, 2, st.Activity[last])
	assert.Equal(t, 1, st.Activity[last-1])
	assert.Equal(t, 1, st.Activity[last-29])
	assert.Equal(t, 1, st.Activity[last-31])
	assert.Equal(t, 1, st.Activity[0])
	sum := 0
	for _, n := range st.Activity {
		sum += n
	}
	assert.Equal(t, 6, sum)
	assert.False(t, st.Partial)
}

// The days are 24 hours whatever the zone of now: across a change of
// daylight saving time, the walk and the bins agree and no hour is lost.
func TestRepoStatsActivityAcrossDST(t *testing.T) {
	t.Parallel()
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	ctx := context.Background()
	f := newBrowseFixture(t)
	repo := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	f.meta.EXPECT().Get(ctx, repo.Name).Return(repo, nil)
	// 90 days of 24 hours before this noon cross the spring change (March 29).
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, paris)
	day := 24 * time.Hour
	since := now.Add(-ports.ActivityDays * day)
	require.NotEqual(t, now.AddDate(0, 0, -ports.ActivityDays), since, "the calendar would differ by an hour")
	times := []time.Time{since.Add(time.Minute), now.Add(-30*day + time.Minute)}
	f.reader.EXPECT().CommitTimes(mock.Anything, repo.Name, oid("a"), since).Return(times, true, nil)
	f.reader.EXPECT().CountCommits(mock.Anything, repo.Name, oid("a")).Return(200_000, nil)
	f.reader.EXPECT().LatestTag(mock.Anything, repo.Name).Return(domain.TagInfo{}, false, nil)
	st, err := f.browse.RepoStats(ctx, domain.Anonymous(), repo.Name, oid("a"), now)
	require.NoError(t, err)
	assert.Equal(t, 1, st.Activity[0], "the start of the span is in the first day")
	assert.Equal(t, 1, st.Recent, "30 days of 24 hours")
	assert.True(t, st.Partial, "the walk was truncated")
}

func TestRepoStatsDoesNotKeepAFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBrowseFixture(t)
	repo := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	f.meta.EXPECT().Get(ctx, repo.Name).Return(repo, nil)
	now := time.Now()
	f.reader.EXPECT().CountCommits(mock.Anything, repo.Name, oid("a")).Return(0, errors.New("boom")).Once()
	_, err := f.browse.RepoStats(ctx, domain.Anonymous(), repo.Name, oid("a"), now)
	require.Error(t, err)
	f.reader.EXPECT().CountCommits(mock.Anything, repo.Name, oid("a")).Return(3, nil).Once()
	f.reader.EXPECT().CommitTimes(mock.Anything, repo.Name, oid("a"), mock.Anything).Return([]time.Time{now}, false, nil).Once()
	f.reader.EXPECT().LatestTag(mock.Anything, repo.Name).Return(domain.TagInfo{}, false, nil).Once()
	st, err := f.browse.RepoStats(ctx, domain.Anonymous(), repo.Name, oid("a"), now)
	require.NoError(t, err)
	assert.Equal(t, 3, st.Commits)
	assert.Nil(t, st.LatestTag)
}

// TestBrowseAccess is the access rule over every read use case and every
// principal: anonymous sees public repositories only, a private one answers
// not found; stored tokens need repo:read on the repository; and the Git
// reader is never reached when access is refused.
func TestBrowseAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	priv := domain.Repo{Name: name(t, "priv"), Visibility: domain.VisibilityPrivate}

	principals := []struct {
		name string
		p    domain.Principal
		// who may read which repository
		pub, priv bool
	}{
		{"anonymous", domain.Anonymous(), true, false},
		{"root", domain.Root(), true, true},
		{"token repo:read", tokenPrincipal(domain.ScopeRepoRead), true, true},
		{"token repo:read on priv", tokenPrincipal(domain.ScopeRepoRead, "priv"), true, true},
		{"token repo:read on another repo", tokenPrincipal(domain.ScopeRepoRead, "other"), true, false},
		{"token repo:write only", tokenPrincipal(domain.ScopeRepoWrite), true, false},
		{"token repo:admin only", tokenPrincipal(domain.ScopeRepoAdmin, "priv"), true, false},
		{"token token:admin only", tokenPrincipal(domain.ScopeTokenAdmin), true, false},
	}
	for _, op := range browseOps() {
		for _, pr := range principals {
			for _, repo := range []domain.Repo{pub, priv} {
				t.Run(op.name+"/"+pr.name+"/"+repo.Name.String(), func(t *testing.T) {
					t.Parallel()
					f := newBrowseFixture(t)
					f.meta.EXPECT().Get(ctx, repo.Name).Return(repo, nil)
					allowed := pr.pub
					if repo.Visibility == domain.VisibilityPrivate {
						allowed = pr.priv
					}
					if allowed {
						op.expect(f.reader)
					}
					err := op.call(f.browse, ctx, pr.p, repo.Name)
					if allowed {
						assert.NoError(t, err)
					} else {
						assert.ErrorIs(t, err, domain.ErrNotFound)
						assert.NotErrorIs(t, err, domain.ErrForbidden)
					}
				})
			}
		}
		t.Run(op.name+"/missing repository", func(t *testing.T) {
			t.Parallel()
			f := newBrowseFixture(t)
			f.meta.EXPECT().Get(ctx, name(t, "nope")).Return(domain.Repo{}, domain.ErrNotFound)
			err := op.call(f.browse, ctx, domain.Root(), name(t, "nope"))
			assert.ErrorIs(t, err, domain.ErrNotFound)
		})
		t.Run(op.name+"/zero name", func(t *testing.T) {
			t.Parallel()
			f := newBrowseFixture(t)
			err := op.call(f.browse, ctx, domain.Root(), domain.RepoName{})
			assert.ErrorIs(t, err, domain.ErrInvalidName)
		})
	}
}

func TestBrowseHidesPrivateOnReaderErrors(t *testing.T) {
	t.Parallel()
	// A reader error is passed through for a caller who may read.
	ctx := context.Background()
	f := newBrowseFixture(t)
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil)
	f.reader.EXPECT().GetTree(ctx, pub.Name, domain.Revision{}, domain.TreePath{}, false).Return(domain.Tree{}, domain.ErrNotDirectory)
	_, err := f.browse.GetTree(ctx, domain.Anonymous(), pub.Name, domain.Revision{}, domain.TreePath{}, false)
	assert.ErrorIs(t, err, domain.ErrNotDirectory)
}

func ref(kind domain.RefKind, name string) domain.Ref {
	return domain.Ref{Name: name, Kind: kind, ShortName: name}
}

func TestListRefsPagingAndKinds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	all := []domain.Ref{
		ref(domain.RefKindHead, "HEAD"),
		ref(domain.RefKindBranch, "refs/heads/a"),
		ref(domain.RefKindBranch, "refs/heads/b"),
		ref(domain.RefKindBranch, "refs/heads/c"),
		ref(domain.RefKindTag, "refs/tags/v1"),
		ref(domain.RefKindTag, "refs/tags/v2"),
	}
	names := func(refs []domain.Ref) []string {
		var out []string
		for _, r := range refs {
			out = append(out, r.Name)
		}
		return out
	}
	setup := func(t *testing.T, refs []domain.Ref) *usecase.Browse {
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(mock.Anything, pub.Name).Return(pub, nil)
		f.reader.EXPECT().ListRefs(mock.Anything, pub.Name).Return(refs, nil)
		return f.browse
	}

	t.Run("pages follow each other without gap or repeat", func(t *testing.T) {
		b := setup(t, all)
		var got []string
		tok := ""
		for pages := 0; ; pages++ {
			require.Less(t, pages, 10)
			page, err := b.ListRefs(ctx, domain.Anonymous(), pub.Name, ports.ListRefsParams{PageSize: 2, PageToken: tok})
			require.NoError(t, err)
			got = append(got, names(page.Refs)...)
			if tok = page.NextPageToken; tok == "" {
				break
			}
		}
		assert.Equal(t, names(all), got)
	})

	t.Run("a ref that appears between pages does not shift the others", func(t *testing.T) {
		b := setup(t, all[:4])
		page, err := b.ListRefs(ctx, domain.Anonymous(), pub.Name, ports.ListRefsParams{PageSize: 2})
		require.NoError(t, err)
		require.Equal(t, []string{"HEAD", "refs/heads/a"}, names(page.Refs))

		b2 := setup(t, append([]domain.Ref{all[0], ref(domain.RefKindBranch, "refs/heads/0new")}, all[1:4]...))
		next, err := b2.ListRefs(ctx, domain.Anonymous(), pub.Name, ports.ListRefsParams{PageSize: 2, PageToken: page.NextPageToken})
		require.NoError(t, err)
		assert.Equal(t, []string{"refs/heads/b", "refs/heads/c"}, names(next.Refs))
	})

	t.Run("kinds filter", func(t *testing.T) {
		b := setup(t, all)
		page, err := b.ListRefs(ctx, domain.Anonymous(), pub.Name, ports.ListRefsParams{Kinds: []domain.RefKind{domain.RefKindTag}})
		require.NoError(t, err)
		assert.Equal(t, []string{"refs/tags/v1", "refs/tags/v2"}, names(page.Refs))
		assert.Empty(t, page.NextPageToken)
	})

	t.Run("default and maximum page size", func(t *testing.T) {
		var many []domain.Ref
		many = append(many, ref(domain.RefKindHead, "HEAD"))
		for i := range usecase.MaxRefPageSize + 100 {
			many = append(many, ref(domain.RefKindTag, fmt.Sprintf("refs/tags/t%04d", i)))
		}
		b := setup(t, many)
		page, err := b.ListRefs(ctx, domain.Anonymous(), pub.Name, ports.ListRefsParams{})
		require.NoError(t, err)
		assert.Len(t, page.Refs, usecase.DefaultPageSize)
		assert.NotEmpty(t, page.NextPageToken)

		b = setup(t, many)
		page, err = b.ListRefs(ctx, domain.Anonymous(), pub.Name, ports.ListRefsParams{PageSize: 100000})
		require.NoError(t, err)
		assert.Len(t, page.Refs, usecase.MaxRefPageSize, "a page holds up to MaxRefPageSize refs")
		assert.NotEmpty(t, page.NextPageToken)
	})

	t.Run("bad input never reaches the reader", func(t *testing.T) {
		t.Parallel()
		f := newBrowseFixture(t)
		for _, params := range []ports.ListRefsParams{
			{PageSize: -1},
			{Kinds: []domain.RefKind{"nope"}},
			{PageToken: "!!!"},
			{PageToken: "Y2E6eA"}, // a token of another kind of listing
		} {
			_, err := f.browse.ListRefs(ctx, domain.Anonymous(), pub.Name, params)
			assert.Error(t, err, "%+v", params)
		}
		_, err := f.browse.ListRefs(ctx, domain.Anonymous(), pub.Name, ports.ListRefsParams{PageSize: -1})
		assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
		_, err = f.browse.ListRefs(ctx, domain.Anonymous(), pub.Name, ports.ListRefsParams{Kinds: []domain.RefKind{"x"}})
		assert.ErrorIs(t, err, domain.ErrInvalidRefKind)
		_, err = f.browse.ListRefs(ctx, domain.Anonymous(), pub.Name, ports.ListRefsParams{PageToken: "!!!"})
		assert.ErrorIs(t, err, domain.ErrInvalidPageToken)
	})
}

func TestListCommitsPaging(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	p := mustPath(t, "d/f")

	t.Run("first page asks for one more than the page and returns a token", func(t *testing.T) {
		t.Parallel()
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil)
		f.reader.EXPECT().ListCommits(ctx, pub.Name, ports.CommitQuery{Path: p, Limit: 10}).
			Return(ports.CommitPage{Head: oid("a"), Commits: []domain.Commit{{ID: oid("b")}}, HasMore: true}, nil)
		page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{Path: p, PageSize: 10})
		require.NoError(t, err)
		require.Len(t, page.Commits, 1)
		require.NotEmpty(t, page.NextPageToken)

		// The next page starts from the pinned head, after the commits read.
		f.reader.EXPECT().ListCommits(ctx, pub.Name, ports.CommitQuery{
			Revision: domain.RevisionOf(oid("a")), Path: p, Skip: 1, Limit: 10,
		}).Return(ports.CommitPage{Head: oid("a"), Commits: []domain.Commit{{ID: oid("c")}}}, nil)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil)
		// The revision of the request is ignored once there is a token.
		rev, _ := domain.ParseRevision("moved")
		next, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{
			Revision: rev, Path: p, PageSize: 10, PageToken: page.NextPageToken,
		})
		require.NoError(t, err)
		assert.Equal(t, oid("c"), next.Commits[0].ID)
		assert.Empty(t, next.NextPageToken)
	})

	t.Run("a token is bound to its path and must be well formed", func(t *testing.T) {
		t.Parallel()
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil)
		f.reader.EXPECT().ListCommits(ctx, pub.Name, mock.Anything).
			Return(ports.CommitPage{Head: oid("a"), Commits: []domain.Commit{{ID: oid("b")}}, HasMore: true}, nil)
		page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{Path: p})
		require.NoError(t, err)

		for _, params := range []ports.ListCommitsParams{
			{PageToken: page.NextPageToken},                             // no path
			{Path: mustPath(t, "other"), PageToken: page.NextPageToken}, // another path
			{Path: p, PageToken: "garbage"},
			{Path: p, PageToken: "cjE6eDp5OnQ"}, // c1:x:y:t is not a token
			{Path: p, PageToken: strings.Repeat("A", 2000)},
		} {
			_, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, params)
			assert.ErrorIs(t, err, domain.ErrInvalidPageToken, "%+v", params)
		}
	})

	t.Run("a token cannot skip deeper than the cap", func(t *testing.T) {
		t.Parallel()
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil).Maybe()
		f.reader.EXPECT().ListCommits(ctx, pub.Name, mock.Anything).
			Return(ports.CommitPage{Head: oid("a"), Commits: []domain.Commit{{ID: oid("b")}}, HasMore: true}, nil).Once()
		page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{Path: p})
		require.NoError(t, err)
		raw, err := base64.RawURLEncoding.DecodeString(page.NextPageToken)
		require.NoError(t, err)
		parts := strings.Split(string(raw), ":") // c1:<head>:<skip>:<path digest>
		require.Len(t, parts, 4)
		withSkip := func(n int) string {
			parts[2] = strconv.Itoa(n)
			return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, ":")))
		}

		f.reader.EXPECT().ListCommits(ctx, pub.Name, mock.MatchedBy(func(q ports.CommitQuery) bool { return q.Skip == usecase.MaxCommitSkip })).
			Return(ports.CommitPage{Head: oid("a")}, nil).Once()
		_, err = f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{Path: p, PageToken: withSkip(usecase.MaxCommitSkip)})
		require.NoError(t, err, "the cap itself is allowed")
		_, err = f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{Path: p, PageToken: withSkip(usecase.MaxCommitSkip + 1)})
		assert.ErrorIs(t, err, domain.ErrInvalidPageToken)
	})

	t.Run("page size", func(t *testing.T) {
		t.Parallel()
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil).Times(2)
		f.reader.EXPECT().ListCommits(ctx, pub.Name, ports.CommitQuery{Limit: usecase.DefaultPageSize}).Return(ports.CommitPage{}, nil)
		f.reader.EXPECT().ListCommits(ctx, pub.Name, ports.CommitQuery{Limit: 100}).Return(ports.CommitPage{}, nil)
		_, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{})
		require.NoError(t, err)
		_, err = f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{PageSize: 5000})
		require.NoError(t, err)
		_, err = f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{PageSize: -3})
		assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
	})

	t.Run("the token of a private repository does not open it", func(t *testing.T) {
		priv := domain.Repo{Name: name(t, "priv"), Visibility: domain.VisibilityPrivate}
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, priv.Name).Return(priv, nil)
		f.reader.EXPECT().ListCommits(ctx, priv.Name, mock.Anything).
			Return(ports.CommitPage{Head: oid("a"), Commits: []domain.Commit{{ID: oid("b")}}, HasMore: true}, nil)
		page, err := f.browse.ListCommits(ctx, domain.Root(), priv.Name, ports.ListCommitsParams{})
		require.NoError(t, err)
		f.meta.EXPECT().Get(ctx, priv.Name).Return(priv, nil)
		_, err = f.browse.ListCommits(ctx, domain.Anonymous(), priv.Name, ports.ListCommitsParams{PageToken: page.NextPageToken})
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func mustPath(t *testing.T, s string) domain.TreePath {
	t.Helper()
	p, err := domain.ParseTreePath(s)
	require.NoError(t, err)
	return p
}

func TestBrowseVerifiesCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	verified := domain.CommitVerification{Format: domain.FormatSSH, Status: domain.StatusVerified, KeyID: "SHA256:k"}
	unsigned := domain.CommitVerification{Status: domain.StatusUnsigned}

	newFixture := func(t *testing.T) (*browseFixture, *mocks.CommitVerifier) {
		f := newBrowseFixture(t)
		v := mocks.NewCommitVerifier(t)
		f.browse.WithVerifier(v)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil)
		return f, v
	}

	t.Run("ListCommits fills the page with one call and leaves unchecked commits alone", func(t *testing.T) {
		t.Parallel()
		f, v := newFixture(t)
		cs := []domain.Commit{{ID: oid("a")}, {ID: oid("b")}, {ID: oid("c")}}
		f.reader.EXPECT().ListCommits(ctx, pub.Name, mock.Anything).Return(ports.CommitPage{Head: oid("a"), Commits: cs}, nil)
		v.EXPECT().VerifyCommits(ctx, pub.Name, cs).Return(map[domain.ObjectID]domain.CommitVerification{
			oid("a"): verified, oid("b"): unsigned,
		}).Once()
		page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{})
		require.NoError(t, err)
		require.Len(t, page.Commits, 3)
		assert.Equal(t, verified, page.Commits[0].Verification)
		assert.Equal(t, unsigned, page.Commits[1].Verification)
		assert.Equal(t, domain.CommitVerification{}, page.Commits[2].Verification)
	})

	t.Run("a cached page is not mutated", func(t *testing.T) {
		t.Parallel()
		f, v := newFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil).Maybe()
		full := oid("a")
		f.reader.EXPECT().ListCommits(ctx, pub.Name, mock.Anything).
			Return(ports.CommitPage{Head: full, Commits: []domain.Commit{{ID: full}}}, nil).Once()
		v.EXPECT().VerifyCommits(ctx, pub.Name, mock.Anything).
			Return(map[domain.ObjectID]domain.CommitVerification{full: verified}).Twice()
		params := ports.ListCommitsParams{Revision: domain.RevisionOf(full), PageSize: 1}
		for range 2 { // the second answer comes from the history cache
			page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, params)
			require.NoError(t, err)
			assert.Equal(t, verified, page.Commits[0].Verification)
		}
		// With the verifier off, the cached entry is still unchecked.
		f.browse.WithVerifier(nil)
		page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, params)
		require.NoError(t, err)
		assert.Equal(t, domain.CommitVerification{}, page.Commits[0].Verification)
	})

	t.Run("GetCommit fills the commit", func(t *testing.T) {
		t.Parallel()
		f, v := newFixture(t)
		c := domain.Commit{ID: oid("a")}
		f.reader.EXPECT().GetCommit(ctx, pub.Name, mock.Anything, domain.DefaultDiffLimits()).
			Return(domain.CommitDetail{Commit: c, Stats: domain.DiffStats{FilesChanged: 2}}, nil)
		v.EXPECT().VerifyCommits(ctx, pub.Name, []domain.Commit{c}).
			Return(map[domain.ObjectID]domain.CommitVerification{oid("a"): verified})
		d, err := f.browse.GetCommit(ctx, domain.Anonymous(), pub.Name, domain.Revision{})
		require.NoError(t, err)
		assert.Equal(t, verified, d.Commit.Verification)
		assert.Equal(t, 2, d.Stats.FilesChanged)
	})

	t.Run("nothing is verified for an empty page or a failed read", func(t *testing.T) {
		t.Parallel()
		f, _ := newFixture(t) // the verifier mock fails the test if called
		f.reader.EXPECT().ListCommits(ctx, pub.Name, mock.Anything).Return(ports.CommitPage{}, nil)
		_, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{})
		require.NoError(t, err)
		f.reader.EXPECT().GetCommit(ctx, pub.Name, mock.Anything, mock.Anything).Return(domain.CommitDetail{}, domain.ErrNotFound)
		_, err = f.browse.GetCommit(ctx, domain.Anonymous(), pub.Name, domain.Revision{})
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("without a verifier the commits are unchanged", func(t *testing.T) {
		t.Parallel()
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil)
		f.reader.EXPECT().ListCommits(ctx, pub.Name, mock.Anything).
			Return(ports.CommitPage{Commits: []domain.Commit{{ID: oid("a")}}}, nil)
		f.reader.EXPECT().GetCommit(ctx, pub.Name, mock.Anything, mock.Anything).
			Return(domain.CommitDetail{Commit: domain.Commit{ID: oid("a")}}, nil)
		page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, ports.ListCommitsParams{})
		require.NoError(t, err)
		assert.Equal(t, domain.CommitVerification{}, page.Commits[0].Verification)
		d, err := f.browse.GetCommit(ctx, domain.Anonymous(), pub.Name, domain.Revision{})
		require.NoError(t, err)
		assert.Equal(t, domain.CommitVerification{}, d.Commit.Verification)
	})
}
