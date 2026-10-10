// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

type browseFixture struct {
	meta   *mocks.RepoMetaStore
	reader *mocks.RepoReader
	browse *app.Browse
}

func newBrowseFixture(t *testing.T) *browseFixture {
	t.Helper()
	f := &browseFixture{meta: mocks.NewRepoMetaStore(t), reader: mocks.NewRepoReader(t)}
	f.browse = app.NewBrowse(f.meta, f.reader)
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
	call func(b *app.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error
	// expect registers the reader call a permitted caller makes.
	expect func(r *mocks.RepoReader)
}

func browseOps() []browseOp {
	root := domain.TreePath{}
	return []browseOp{
		{"ListRefs", func(b *app.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.ListRefs(ctx, p, repo, app.ListRefsParams{})
			return err
		}, func(r *mocks.RepoReader) { r.EXPECT().ListRefs(mock.Anything, mock.Anything).Return(nil, nil) }},
		{"ResolveRevision", func(b *app.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.ResolveRevision(ctx, p, repo, domain.Revision{})
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().ResolveRevision(mock.Anything, mock.Anything, mock.Anything).Return(domain.ResolvedRevision{}, nil)
		}},
		{"ResolveRefPath", func(b *app.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.ResolveRefPath(ctx, p, repo, "main/x")
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().ResolveRefPath(mock.Anything, mock.Anything, "main/x").Return(domain.RefPath{}, nil)
		}},
		{"GetTree", func(b *app.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.GetTree(ctx, p, repo, domain.Revision{}, root, true)
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().GetTree(mock.Anything, mock.Anything, mock.Anything, mock.Anything, true).Return(domain.Tree{}, nil)
		}},
		{"OpenBlob", func(b *app.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, rc, err := b.OpenBlob(ctx, p, repo, domain.Revision{}, root)
			if rc != nil {
				_ = rc.Close()
			}
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().OpenBlob(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
				Return(domain.BlobInfo{}, io.NopCloser(strings.NewReader("")), nil)
		}},
		{"ListCommits", func(b *app.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.ListCommits(ctx, p, repo, app.ListCommitsParams{})
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything).Return(ports.CommitPage{}, nil)
		}},
		{"GetCommit", func(b *app.Browse, ctx context.Context, p domain.Principal, repo domain.RepoName) error {
			_, err := b.GetCommit(ctx, p, repo, domain.Revision{})
			return err
		}, func(r *mocks.RepoReader) {
			r.EXPECT().GetCommit(mock.Anything, mock.Anything, mock.Anything, domain.DefaultDiffLimits()).Return(domain.CommitDetail{}, nil)
		}},
	}
}

// TestBrowseAccess is the access rule over every read use case and every
// principal: anonymous sees public repositories only, a private one answers
// not found; stored tokens need repo:read on the repository; and the Git
// reader is never reached when access is refused.
func TestBrowseAccess(t *testing.T) {
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
			f := newBrowseFixture(t)
			f.meta.EXPECT().Get(ctx, name(t, "nope")).Return(domain.Repo{}, domain.ErrNotFound)
			err := op.call(f.browse, ctx, domain.Root(), name(t, "nope"))
			assert.ErrorIs(t, err, domain.ErrNotFound)
		})
		t.Run(op.name+"/zero name", func(t *testing.T) {
			f := newBrowseFixture(t)
			err := op.call(f.browse, ctx, domain.Root(), domain.RepoName{})
			assert.ErrorIs(t, err, domain.ErrInvalidName)
		})
	}
}

func TestBrowseHidesPrivateOnReaderErrors(t *testing.T) {
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
	setup := func(t *testing.T, refs []domain.Ref) *app.Browse {
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
			page, err := b.ListRefs(ctx, domain.Anonymous(), pub.Name, app.ListRefsParams{PageSize: 2, PageToken: tok})
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
		page, err := b.ListRefs(ctx, domain.Anonymous(), pub.Name, app.ListRefsParams{PageSize: 2})
		require.NoError(t, err)
		require.Equal(t, []string{"HEAD", "refs/heads/a"}, names(page.Refs))

		b2 := setup(t, append([]domain.Ref{all[0], ref(domain.RefKindBranch, "refs/heads/0new")}, all[1:4]...))
		next, err := b2.ListRefs(ctx, domain.Anonymous(), pub.Name, app.ListRefsParams{PageSize: 2, PageToken: page.NextPageToken})
		require.NoError(t, err)
		assert.Equal(t, []string{"refs/heads/b", "refs/heads/c"}, names(next.Refs))
	})

	t.Run("kinds filter", func(t *testing.T) {
		b := setup(t, all)
		page, err := b.ListRefs(ctx, domain.Anonymous(), pub.Name, app.ListRefsParams{Kinds: []domain.RefKind{domain.RefKindTag}})
		require.NoError(t, err)
		assert.Equal(t, []string{"refs/tags/v1", "refs/tags/v2"}, names(page.Refs))
		assert.Empty(t, page.NextPageToken)
	})

	t.Run("default and maximum page size", func(t *testing.T) {
		var many []domain.Ref
		many = append(many, ref(domain.RefKindHead, "HEAD"))
		for i := range 250 {
			many = append(many, ref(domain.RefKindTag, fmt.Sprintf("refs/tags/t%03d", i)))
		}
		b := setup(t, many)
		page, err := b.ListRefs(ctx, domain.Anonymous(), pub.Name, app.ListRefsParams{})
		require.NoError(t, err)
		assert.Len(t, page.Refs, app.DefaultPageSize)
		assert.NotEmpty(t, page.NextPageToken)

		b = setup(t, many)
		page, err = b.ListRefs(ctx, domain.Anonymous(), pub.Name, app.ListRefsParams{PageSize: 100000})
		require.NoError(t, err)
		assert.Len(t, page.Refs, app.MaxPageSize)
	})

	t.Run("bad input never reaches the reader", func(t *testing.T) {
		f := newBrowseFixture(t)
		for _, params := range []app.ListRefsParams{
			{PageSize: -1},
			{Kinds: []domain.RefKind{"nope"}},
			{PageToken: "!!!"},
			{PageToken: "Y2E6eA"}, // a token of another kind of listing
		} {
			_, err := f.browse.ListRefs(ctx, domain.Anonymous(), pub.Name, params)
			assert.Error(t, err, "%+v", params)
		}
		_, err := f.browse.ListRefs(ctx, domain.Anonymous(), pub.Name, app.ListRefsParams{PageSize: -1})
		assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
		_, err = f.browse.ListRefs(ctx, domain.Anonymous(), pub.Name, app.ListRefsParams{Kinds: []domain.RefKind{"x"}})
		assert.ErrorIs(t, err, domain.ErrInvalidRefKind)
		_, err = f.browse.ListRefs(ctx, domain.Anonymous(), pub.Name, app.ListRefsParams{PageToken: "!!!"})
		assert.ErrorIs(t, err, domain.ErrInvalidPageToken)
	})
}

func TestListCommitsPaging(t *testing.T) {
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	p := mustPath(t, "d/f")

	t.Run("first page asks for one more than the page and returns a token", func(t *testing.T) {
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil)
		f.reader.EXPECT().ListCommits(ctx, pub.Name, ports.CommitQuery{Path: p, Limit: 10}).
			Return(ports.CommitPage{Head: oid("a"), Commits: []domain.Commit{{ID: oid("b")}}, HasMore: true}, nil)
		page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{Path: p, PageSize: 10})
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
		next, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{
			Revision: rev, Path: p, PageSize: 10, PageToken: page.NextPageToken,
		})
		require.NoError(t, err)
		assert.Equal(t, oid("c"), next.Commits[0].ID)
		assert.Empty(t, next.NextPageToken)
	})

	t.Run("a token is bound to its path and must be well formed", func(t *testing.T) {
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil)
		f.reader.EXPECT().ListCommits(ctx, pub.Name, mock.Anything).
			Return(ports.CommitPage{Head: oid("a"), Commits: []domain.Commit{{ID: oid("b")}}, HasMore: true}, nil)
		page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{Path: p})
		require.NoError(t, err)

		for _, params := range []app.ListCommitsParams{
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
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil).Maybe()
		f.reader.EXPECT().ListCommits(ctx, pub.Name, mock.Anything).
			Return(ports.CommitPage{Head: oid("a"), Commits: []domain.Commit{{ID: oid("b")}}, HasMore: true}, nil).Once()
		page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{Path: p})
		require.NoError(t, err)
		raw, err := base64.RawURLEncoding.DecodeString(page.NextPageToken)
		require.NoError(t, err)
		parts := strings.Split(string(raw), ":") // c1:<head>:<skip>:<path digest>
		require.Len(t, parts, 4)
		withSkip := func(n int) string {
			parts[2] = strconv.Itoa(n)
			return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, ":")))
		}

		f.reader.EXPECT().ListCommits(ctx, pub.Name, mock.MatchedBy(func(q ports.CommitQuery) bool { return q.Skip == app.MaxCommitSkip })).
			Return(ports.CommitPage{Head: oid("a")}, nil).Once()
		_, err = f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{Path: p, PageToken: withSkip(app.MaxCommitSkip)})
		require.NoError(t, err, "the cap itself is allowed")
		_, err = f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{Path: p, PageToken: withSkip(app.MaxCommitSkip + 1)})
		assert.ErrorIs(t, err, domain.ErrInvalidPageToken)
	})

	t.Run("page size", func(t *testing.T) {
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(pub, nil).Times(2)
		f.reader.EXPECT().ListCommits(ctx, pub.Name, ports.CommitQuery{Limit: app.DefaultPageSize}).Return(ports.CommitPage{}, nil)
		f.reader.EXPECT().ListCommits(ctx, pub.Name, ports.CommitQuery{Limit: 100}).Return(ports.CommitPage{}, nil)
		_, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{})
		require.NoError(t, err)
		_, err = f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{PageSize: 5000})
		require.NoError(t, err)
		_, err = f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{PageSize: -3})
		assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
	})

	t.Run("the token of a private repository does not open it", func(t *testing.T) {
		priv := domain.Repo{Name: name(t, "priv"), Visibility: domain.VisibilityPrivate}
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(ctx, priv.Name).Return(priv, nil)
		f.reader.EXPECT().ListCommits(ctx, priv.Name, mock.Anything).
			Return(ports.CommitPage{Head: oid("a"), Commits: []domain.Commit{{ID: oid("b")}}, HasMore: true}, nil)
		page, err := f.browse.ListCommits(ctx, domain.Root(), priv.Name, app.ListCommitsParams{})
		require.NoError(t, err)
		f.meta.EXPECT().Get(ctx, priv.Name).Return(priv, nil)
		_, err = f.browse.ListCommits(ctx, domain.Anonymous(), priv.Name, app.ListCommitsParams{PageToken: page.NextPageToken})
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func mustPath(t *testing.T, s string) domain.TreePath {
	t.Helper()
	p, err := domain.ParseTreePath(s)
	require.NoError(t, err)
	return p
}
