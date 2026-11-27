// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// navDir is a repository by directory: its entries, and whether the listing
// is cut.
type navDir struct {
	entries   []domain.TreeEntry
	truncated bool
}

// navBrowse serves the refs, the directories of dirs without last commits,
// and any file as a Markdown heading.
func navBrowse(t *testing.T, dirs map[string]navDir) *mocks.BrowseService {
	t.Helper()
	browse := mocks.NewBrowseService(t)
	tip := objectID(t, mainTip)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, repoName(t, "r"), mock.Anything).Return(ports.RefPage{
		Refs: []domain.Ref{
			{Name: "refs/heads/main", ShortName: "main", Kind: domain.RefKindBranch, ID: tip, CommitID: tip},
			{Name: "refs/heads/feature/x", ShortName: "feature/x", Kind: domain.RefKindBranch, ID: tip, CommitID: tip},
		},
	}, nil).Maybe()
	browse.EXPECT().GetTree(mock.Anything, mock.Anything, repoName(t, "r"), mock.Anything, mock.Anything, false).RunAndReturn(
		func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ domain.Revision, p domain.TreePath, _ bool) (domain.Tree, error) {
			d, ok := dirs[p.String()]
			if !ok {
				return domain.Tree{}, domain.ErrNotFound
			}
			return domain.Tree{CommitID: tip, Entries: d.entries, Truncated: d.truncated}, nil
		}).Maybe()
	browse.EXPECT().OpenBlob(mock.Anything, mock.Anything, repoName(t, "r"), mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ domain.Revision, p domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
			body := "# Title\n"
			return domain.BlobInfo{ID: objectID(t, blobID), Path: p.String(), Size: int64(len(body)), MediaType: "text/plain"}, io.NopCloser(strings.NewReader(body)), nil
		}).Maybe()
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repoName(t, "r"), mock.Anything).Return(ports.HistoryPage{}, nil).Maybe()
	return browse
}

func dirEntry(p string) domain.TreeEntry {
	return domain.TreeEntry{Name: path.Base(p), Path: p, Kind: domain.EntryDirectory}
}

func fileEntry(p string) domain.TreeEntry {
	return domain.TreeEntry{Name: path.Base(p), Path: p, Kind: domain.EntryFile}
}

// serveNav answers url with the server of dirs.
func serveNav(t *testing.T, dirs map[string]navDir, url string) string {
	t.Helper()
	return serveWith(t, navBrowse(t, dirs), url)
}

// serveWith answers url, a page that must succeed, with browse.
func serveWith(t *testing.T, browse *mocks.BrowseService, url string) string {
	t.Helper()
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: browse})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.String()
}

// railOf is the rail of a page.
func railOf(t *testing.T, html string) string {
	t.Helper()
	_, rail, found := strings.Cut(html, `<aside class="rail"`)
	require.True(t, found, "the page has a rail")
	rail, _, _ = strings.Cut(rail, "</aside>")
	return rail
}

func TestBlobFileNavOpensAncestors(t *testing.T) {
	t.Parallel()
	dirs := map[string]navDir{
		"":        {entries: []domain.TreeEntry{dirEntry("docs"), dirEntry("src"), {Name: "lib", Path: "lib", Kind: domain.EntrySubmodule}, fileEntry("README.md")}},
		"src":     {entries: []domain.TreeEntry{dirEntry("src/lib"), fileEntry("src/app.html")}},
		"src/lib": {entries: []domain.TreeEntry{fileEntry("src/lib/x.ts"), fileEntry("src/lib/y.ts")}},
	}
	browse := navBrowse(t, dirs)
	rail := railOf(t, serveWith(t, browse, "/r/blob/main/src/lib/x.ts"))
	browse.AssertNumberOfCalls(t, "GetTree", 3) // the root, src, src/lib: one listing each
	assert.Contains(t, rail, `<section aria-labelledby="r-files-nav"><h2 id="r-files-nav">Files</h2><ul class="fnav">`)
	assert.Contains(t, rail, `<li data-kind="dir" data-path="src"><details open data-src="/r/tree/main/src?nav"><summary title="src/">src/</summary><ul class="fnav">`)
	assert.Contains(t, rail, `<details open data-src="/r/tree/main/src/lib?nav"><summary title="lib/">lib/</summary>`)
	assert.Contains(t, rail, `<details data-src="/r/tree/main/docs?nav"><summary title="docs/">docs/</summary><ul class="fnav"></ul></details>`, "an unopened directory is empty")
	assert.Contains(t, rail, `<a href="/r/blob/main/src/lib/x.ts" title="x.ts" aria-current="page">x.ts</a>`)
	assert.Contains(t, rail, `<a href="/r/blob/main/src/lib/y.ts" title="y.ts">y.ts</a>`)
	assert.Contains(t, rail, `<li data-kind="module" data-path="lib"><span title="lib (submodule)">lib</span></li>`)
	assert.Equal(t, 1, strings.Count(rail, `aria-current="page"`))
	assert.NotContains(t, rail, `id="tree-rows"`)
	assert.NotContains(t, rail, ` style=`)
	assert.NotContains(t, rail, "fnav-more")
}

func TestBlobFileNavRootFile(t *testing.T) {
	t.Parallel()
	dirs := map[string]navDir{"": {entries: []domain.TreeEntry{dirEntry("src"), fileEntry("README.md")}}}
	rail := railOf(t, serveNav(t, dirs, "/r/blob/main/README.md"))
	assert.Contains(t, rail, `<a href="/r/blob/main/README.md" title="README.md" aria-current="page">README.md</a>`)
	assert.Contains(t, rail, `<details data-src="/r/tree/main/src?nav">`)
	assert.NotContains(t, rail, "<details open")
	// The Markdown outline comes after the file tree.
	files, outline := strings.Index(rail, `id="r-files-nav"`), strings.Index(rail, `id="r-outline"`)
	require.GreaterOrEqual(t, outline, 0, "a Markdown file keeps its outline")
	assert.Less(t, files, outline)
}

func TestBlobFileNavTruncatedKeepsTheWay(t *testing.T) {
	t.Parallel()
	dirs := map[string]navDir{
		"":    {entries: []domain.TreeEntry{fileEntry("a.txt")}, truncated: true},
		"src": {entries: []domain.TreeEntry{fileEntry("src/a.txt")}, truncated: true},
	}
	rail := railOf(t, serveNav(t, dirs, "/r/blob/main/src/z.txt"))
	assert.Contains(t, rail, `<details open data-src="/r/tree/main/src?nav"><summary title="src/">src/</summary>`, "the missing ancestor is added, open")
	assert.Contains(t, rail, `<a href="/r/blob/main/src/z.txt" title="z.txt" aria-current="page">z.txt</a>`, "the missing file is added, current")
	assert.Contains(t, rail, `<li class="fnav-more"><a href="/r/tree/main">more…</a></li>`)
	assert.Contains(t, rail, `<li class="fnav-more"><a href="/r/tree/main/src">more…</a></li>`)
}

func TestTreeNavItems(t *testing.T) {
	t.Parallel()
	dirs := map[string]navDir{"src": {entries: []domain.TreeEntry{dirEntry("src/lib"), fileEntry("src/app.html")}, truncated: true}}
	body := serveNav(t, dirs, "/r/tree/main/src?nav")
	assert.True(t, strings.HasPrefix(body, `<li data-kind="dir" data-path="src/lib"><details data-src="/r/tree/main/src/lib?nav"><summary title="lib/">lib/</summary><ul class="fnav"></ul></details></li>`), body)
	assert.Contains(t, body, `<li data-kind="file" data-path="src/app.html"><a href="/r/blob/main/src/app.html" title="app.html">app.html</a></li>`)
	assert.Contains(t, body, `<li class="fnav-more"><a href="/r/tree/main/src">more…</a></li>`)
	assert.NotContains(t, body, "aria-current")
	assert.NotContains(t, body, "<html")
}

func TestFileNavScriptOnlyOnTheFilePage(t *testing.T) {
	t.Parallel()
	dirs := map[string]navDir{"": {entries: []domain.TreeEntry{fileEntry("README.md")}}}
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: navBrowse(t, dirs)})
	script := srv.assets.urls.FileNav
	require.NotEmpty(t, script)
	assert.Contains(t, serveNav(t, dirs, "/r/blob/main/README.md"), `<script src="`+script+`" defer>`)
	// A page with a rail and the real assets, but no file tree.
	other := renderToString(t, Shell(Layout{HasRail: true, Assets: srv.assets.urls}, summaryRail(SummaryPage{})))
	assert.Contains(t, other, srv.assets.urls.Rail, "the assets are in the page")
	assert.NotContains(t, other, script)
}

// manyFiles is n files of dir, named f000, f001, ….
func manyFiles(dir string, n int) []domain.TreeEntry {
	out := make([]domain.TreeEntry, n)
	for i := range out {
		out[i] = fileEntry(path.Join(dir, fmt.Sprintf("f%03d", i)))
	}
	return out
}

func TestFileNavCapsTheListing(t *testing.T) {
	t.Parallel()
	dirs := map[string]navDir{
		"":    {entries: append([]domain.TreeEntry{dirEntry("big")}, manyFiles("", navMaxItems+50)...)},
		"big": {entries: manyFiles("big", navMaxItems+100)},
	}
	t.Run("file page", func(t *testing.T) {
		t.Parallel()
		rail := railOf(t, serveNav(t, dirs, "/r/blob/main/big/f350"))
		// The root holds big/ and navMaxItems-1 files; big/ holds navMaxItems
		// files and the current file, added past the cap.
		assert.Equal(t, (navMaxItems-1)+(navMaxItems+1), strings.Count(rail, `<li data-kind="file"`))
		assert.Contains(t, rail, `<a href="/r/blob/main/big/f350" title="f350" aria-current="page">f350</a>`, "the current file past the cap is kept")
		assert.Contains(t, rail, `<details open data-src="/r/tree/main/big?nav">`)
		assert.Contains(t, rail, `<li class="fnav-more"><a href="/r/tree/main">more…</a></li>`)
		assert.Contains(t, rail, `<li class="fnav-more"><a href="/r/tree/main/big">more…</a></li>`)
		assert.NotContains(t, rail, `data-path="big/f301"`)
	})
	t.Run("nav", func(t *testing.T) {
		t.Parallel()
		body := serveNav(t, dirs, "/r/tree/main/big?nav")
		assert.Equal(t, navMaxItems, strings.Count(body, `<li data-kind="file"`))
		assert.True(t, strings.HasSuffix(body, `<li class="fnav-more"><a href="/r/tree/main/big">more…</a></li>`), body[len(body)-200:])
	})
}

// closeTracker is a blob body that records its Close.
type closeTracker struct {
	io.Reader
	closed *atomic.Int32
}

func (c closeTracker) Close() error { c.closed.Add(1); return nil }

// The content of the file is read before the listings of the rail: the blob
// stream is released first, and closed once.
func TestBlobClosesTheFileBeforeTheNav(t *testing.T) {
	t.Parallel()
	var closed atomic.Int32
	browse := mocks.NewBrowseService(t)
	tip := objectID(t, mainTip)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RefPage{
		Refs: []domain.Ref{{Name: "refs/heads/main", ShortName: "main", Kind: domain.RefKindBranch, ID: tip, CommitID: tip}},
	}, nil).Maybe()
	browse.EXPECT().OpenBlob(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		domain.BlobInfo{ID: objectID(t, blobID), Size: 5}, closeTracker{strings.NewReader("hello"), &closed}, nil).Once()
	browse.EXPECT().GetTree(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, false).RunAndReturn(
		func(context.Context, domain.Principal, domain.RepoName, domain.Revision, domain.TreePath, bool) (domain.Tree, error) {
			assert.Equal(t, int32(1), closed.Load(), "the file is closed before the listing")
			return domain.Tree{Entries: []domain.TreeEntry{fileEntry("a.txt")}}, nil
		}).Once()
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.HistoryPage{}, nil).Maybe()
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: browse})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/blob/main/a.txt", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `aria-current="page">a.txt</a>`)
	assert.Equal(t, int32(1), closed.Load(), "closed once")
}

// Without a listing the file page still answers: no file tree, no script;
// a Markdown file keeps its outline as the only section of the rail.
func TestBlobWithoutNav(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: navBrowse(t, nil)})
	script := srv.assets.urls.FileNav
	for _, tc := range []struct {
		url  string
		rail bool
	}{{"/r/blob/main/a.txt", false}, {"/r/blob/main/README.md", true}} {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			body := serveNav(t, nil, tc.url)
			assert.NotContains(t, body, "r-files-nav")
			assert.NotContains(t, body, script)
			assert.Equal(t, tc.rail, strings.Contains(body, `<aside class="rail"`))
			if tc.rail {
				assert.Contains(t, railOf(t, body), `id="r-outline"`)
			}
		})
	}
}

// Names keep the escaping of the tree page: a URL-escaped path, quotes
// escaped in attributes, invisible characters shown as markers.
func TestFileNavNamesAreEscaped(t *testing.T) {
	t.Parallel()
	odd, bidi := "d/a\"b%c?d.txt", "d/x\u202Eevil\x01.txt"
	dirs := map[string]navDir{
		"":  {entries: []domain.TreeEntry{dirEntry("d")}},
		"d": {entries: []domain.TreeEntry{fileEntry(odd), fileEntry(bidi)}},
	}
	rail := railOf(t, serveNav(t, dirs, "/r/blob/main/d/a%22b%25c%3Fd.txt"))
	assert.Contains(t, rail, `<li data-kind="file" data-path="d/a%22b%25c%3Fd.txt"><a href="/r/blob/main/d/a%22b%25c%3Fd.txt" title="a&#34;b%c?d.txt" aria-current="page">a&#34;b%c?d.txt</a></li>`)
	assert.Contains(t, rail, `<li data-kind="file" data-path="d/x%E2%80%AEevil%01.txt"><a href="/r/blob/main/d/x%E2%80%AEevil%01.txt" title="x&lt;U+202E&gt;evil&lt;U+0001&gt;.txt">x&lt;U+202E&gt;evil&lt;U+0001&gt;.txt</a></li>`)
	assert.NotContains(t, rail, "\u202E")
	assert.NotContains(t, rail, "\x01")
}

// A ref with a slash names the ref in every link, whole.
func TestFileNavRefWithSlash(t *testing.T) {
	t.Parallel()
	dirs := map[string]navDir{
		"":    {entries: []domain.TreeEntry{dirEntry("src"), dirEntry("docs")}},
		"src": {entries: []domain.TreeEntry{fileEntry("src/a.go")}},
	}
	rail := railOf(t, serveNav(t, dirs, "/r/blob/feature/x/src/a.go"))
	assert.Contains(t, rail, `<details open data-src="/r/tree/feature/x/src?nav">`)
	assert.Contains(t, rail, `<details data-src="/r/tree/feature/x/docs?nav">`)
	assert.Contains(t, rail, `<a href="/r/blob/feature/x/src/a.go" title="a.go" aria-current="page">a.go</a>`)
	body := serveNav(t, dirs, "/r/tree/feature/x/src?nav")
	assert.Contains(t, body, `<a href="/r/blob/feature/x/src/a.go" title="a.go">a.go</a>`)
}

// The entries of a private repository are as missing to an anonymous caller
// as the repository: ?nav answers the 404 page, without listing anything.
func TestTreeNavPrivateRepoIsMissing(t *testing.T) {
	t.Parallel()
	repos := mocks.NewRepoService(t)
	// Only the owner sees the repository; the request is anonymous.
	repos.EXPECT().Get(mock.Anything, domain.Anonymous(), repoName(t, "secret")).RunAndReturn(
		func(_ context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
			if !p.IsRoot() {
				return domain.Repo{}, domain.ErrNotFound
			}
			return domain.Repo{Name: name, Visibility: domain.VisibilityPrivate}, nil
		}).Once()
	srv := newTestServer(t, Config{Repos: repos, Browse: mocks.NewBrowseService(t)})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/secret/tree/main/src?nav", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "fnav")
}
