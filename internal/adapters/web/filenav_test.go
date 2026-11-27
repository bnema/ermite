// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		Refs: []domain.Ref{{Name: "refs/heads/main", ShortName: "main", Kind: domain.RefKindBranch, ID: tip, CommitID: tip}},
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
	return domain.TreeEntry{Name: p[strings.LastIndex(p, "/")+1:], Path: p, Kind: domain.EntryDirectory}
}

func fileEntry(p string) domain.TreeEntry {
	return domain.TreeEntry{Name: p[strings.LastIndex(p, "/")+1:], Path: p, Kind: domain.EntryFile}
}

// serveNav answers url with the server of dirs.
func serveNav(t *testing.T, dirs map[string]navDir, url string) string {
	t.Helper()
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: navBrowse(t, dirs)})
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
	rail := railOf(t, serveNav(t, dirs, "/r/blob/main/src/lib/x.ts"))
	assert.Contains(t, rail, `<section aria-labelledby="r-files-nav"><h2 id="r-files-nav">Files</h2><ul class="fnav">`)
	assert.Contains(t, rail, `<li data-kind="dir" data-path="src"><details open data-src="/r/tree/main/src?nav"><summary>src/</summary><ul class="fnav">`)
	assert.Contains(t, rail, `<details open data-src="/r/tree/main/src/lib?nav"><summary>lib/</summary>`)
	assert.Contains(t, rail, `<details data-src="/r/tree/main/docs?nav"><summary>docs/</summary><ul class="fnav"></ul></details>`, "an unopened directory is empty")
	assert.Contains(t, rail, `<a href="/r/blob/main/src/lib/x.ts" aria-current="page">x.ts</a>`)
	assert.Contains(t, rail, `<a href="/r/blob/main/src/lib/y.ts">y.ts</a>`)
	assert.Contains(t, rail, `<li data-kind="module" data-path="lib"><span title="submodule">lib</span></li>`)
	assert.Equal(t, 1, strings.Count(rail, `aria-current="page"`))
	assert.NotContains(t, rail, `id="tree-rows"`)
	assert.NotContains(t, rail, ` style=`)
	assert.NotContains(t, rail, "fnav-more")
}

func TestBlobFileNavRootFile(t *testing.T) {
	t.Parallel()
	dirs := map[string]navDir{"": {entries: []domain.TreeEntry{dirEntry("src"), fileEntry("README.md")}}}
	rail := railOf(t, serveNav(t, dirs, "/r/blob/main/README.md"))
	assert.Contains(t, rail, `<a href="/r/blob/main/README.md" aria-current="page">README.md</a>`)
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
	assert.Contains(t, rail, `<details open data-src="/r/tree/main/src?nav"><summary>src/</summary>`, "the missing ancestor is added, open")
	assert.Contains(t, rail, `<a href="/r/blob/main/src/z.txt" aria-current="page">z.txt</a>`, "the missing file is added, current")
	assert.Contains(t, rail, `<li class="fnav-more"><a href="/r/tree/main">more…</a></li>`)
	assert.Contains(t, rail, `<li class="fnav-more"><a href="/r/tree/main/src">more…</a></li>`)
}

func TestTreeNavItems(t *testing.T) {
	t.Parallel()
	dirs := map[string]navDir{"src": {entries: []domain.TreeEntry{dirEntry("src/lib"), fileEntry("src/app.html")}, truncated: true}}
	body := serveNav(t, dirs, "/r/tree/main/src?nav")
	assert.True(t, strings.HasPrefix(body, `<li data-kind="dir" data-path="src/lib"><details data-src="/r/tree/main/src/lib?nav"><summary>lib/</summary><ul class="fnav"></ul></details></li>`), body)
	assert.Contains(t, body, `<li data-kind="file" data-path="src/app.html"><a href="/r/blob/main/src/app.html">app.html</a></li>`)
	assert.Contains(t, body, `<li class="fnav-more"><a href="/r/tree/main/src">more…</a></li>`)
	assert.NotContains(t, body, "aria-current")
	assert.NotContains(t, body, "<html")
}
