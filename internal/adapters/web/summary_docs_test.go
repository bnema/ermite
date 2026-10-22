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

func TestSummaryDocs(t *testing.T) {
	t.Parallel()
	entries := []domain.TreeEntry{
		{Name: "readme.txt", Kind: domain.EntryFile},
		{Name: "SECURITY.md", Kind: domain.EntryFile},
		{Name: "LICENSE", Kind: domain.EntryExecutable},
		{Name: "README.md", Kind: domain.EntryFile},
		{Name: "CONTRIBUTING.md", Kind: domain.EntryDirectory},
		{Name: "COPYING.txt", Kind: domain.EntryFile},
		{Name: "code_of_conduct.MD", Kind: domain.EntryFile},
	}
	docs := summaryDocs(entries)
	var names []string
	for _, e := range docs {
		names = append(names, e.Name)
	}
	assert.Equal(t, []string{"README.md", "LICENSE", "COPYING.txt", "code_of_conduct.MD", "SECURITY.md"}, names)
	assert.Empty(t, summaryDocs(nil))
}

func TestSummaryDocSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		selected, want, body string
		markdown, noReadme   bool
	}{
		{"", "README.md", "# Read me", true, false},
		{"security.MD", "SECURITY.md", "# Reporting", true, false},
		{"LICENSE", "LICENSE", "<license>", false, false},
		{"../secret", "README.md", "# Read me", true, false},
		{"", "LICENSE", "<license>", false, true},
	} {
		t.Run(tc.selected+tc.want, func(t *testing.T) {
			t.Parallel()
			repo := repoName(t, "r")
			browse := mocks.NewBrowseService(t)
			entries := []domain.TreeEntry{{Name: "LICENSE", Path: "LICENSE", Kind: domain.EntryFile}, {Name: "SECURITY.md", Path: "SECURITY.md", Kind: domain.EntryFile}}
			if !tc.noReadme {
				entries = append(entries, domain.TreeEntry{Name: "README.md", Path: "README.md", Kind: domain.EntryFile})
			}
			browse.EXPECT().GetTree(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything, false).Return(domain.Tree{Entries: entries}, nil)
			browse.EXPECT().OpenBlob(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ domain.Revision, path domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
				assert.Equal(t, tc.want, path.String())
				return domain.BlobInfo{}, io.NopCloser(strings.NewReader(tc.body)), nil
			})
			s := newTestServer(t, Config{Repos: mocks.NewRepoService(t), Browse: browse})
			d := &repoData{repo: ports.RepoView{Name: repo}, head: &domain.Ref{CommitID: objectID(t, mainTip)}}
			rd, tabs, err := s.summaryDoc(t.Context(), d, "main", tc.selected)
			require.NoError(t, err)
			require.NotNil(t, rd)
			assert.Equal(t, tc.want, rd.readme.Name)
			if tc.markdown {
				assert.Contains(t, string(rd.readme.HTML), "<h1")
				assert.Len(t, rd.outline, 1)
			} else {
				assert.Equal(t, "<pre>&lt;license&gt;</pre>", string(rd.readme.HTML))
				assert.Empty(t, rd.outline)
			}
			current := 0
			for _, tab := range tabs {
				if tab.Current {
					current++
					assert.Equal(t, tc.want, tab.Name)
				}
			}
			assert.Equal(t, 1, current)
			out := renderToString(t, ReadmeCard(*rd.readme, tabs...))
			assert.Contains(t, out, `aria-label="Repository documents"`)
			assert.Contains(t, out, `aria-current="page"`)
			assert.Contains(t, out, `href="?doc=LICENSE"`)
		})
	}
}

func TestSummaryDocLimits(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, Config{Repos: mocks.NewRepoService(t), Browse: mocks.NewBrowseService(t)})
	rd, err := s.renderDoc(t.Context(), repoName(t, "r"), mainTip, "main", "", domain.TreeEntry{Name: "LICENSE", Size: maxReadmeBytes + 1})
	require.NoError(t, err)
	assert.Contains(t, rd.readme.Note, "too large")
	assert.Empty(t, rd.outline)
	out := renderToString(t, ReadmeCard(*rd.readme))
	assert.NotContains(t, out, "doc-tabs")
}

func TestSummaryArrivalHasNoRailCursor(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "r")
	browse := mocks.NewBrowseService(t)
	tip := objectID(t, mainTip)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.RefPage{Refs: []domain.Ref{{Name: "refs/heads/main", ShortName: "main", Kind: domain.RefKindBranch, CommitID: tip}}}, nil)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.HistoryPage{Commits: []domain.Commit{{ID: tip, Summary: "recent commit"}}}, nil)
	browse.EXPECT().GetTree(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything, false).Return(domain.Tree{}, nil)
	s := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: browse})
	req := httptest.NewRequest(http.MethodGet, "/r", nil)
	req.SetPathValue("repo", "r")
	page, status, err := s.summary(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	out := renderToString(t, page)
	assert.Contains(t, out, "recent commit")
	_, rail, found := strings.Cut(out, `<aside class="rail"`)
	require.True(t, found)
	rail, _, _ = strings.Cut(rail, "</aside>")
	assert.NotContains(t, rail, "data-cursor")
	assert.NotContains(t, rail, "autofocus")
}

func TestSummaryDocExactFilename(t *testing.T) {
	t.Parallel()
	for _, selected := range []string{"LICENSE", "license"} {
		t.Run(selected, func(t *testing.T) {
			t.Parallel()
			repo := repoName(t, "r")
			browse := mocks.NewBrowseService(t)
			entries := []domain.TreeEntry{{Name: "LICENSE", Path: "LICENSE", Kind: domain.EntryFile}, {Name: "license", Path: "license", Kind: domain.EntryFile}}
			browse.EXPECT().GetTree(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything, false).Return(domain.Tree{Entries: entries}, nil)
			browse.EXPECT().OpenBlob(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ domain.Revision, path domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
				return domain.BlobInfo{}, io.NopCloser(strings.NewReader("content of " + path.String())), nil
			})
			s := newTestServer(t, Config{Repos: mocks.NewRepoService(t), Browse: browse})
			d := &repoData{repo: ports.RepoView{Name: repo}, head: &domain.Ref{CommitID: objectID(t, mainTip)}}
			req := httptest.NewRequest(http.MethodGet, "/r?doc="+selected, nil)
			rd, tabs, err := s.summaryDoc(t.Context(), d, "main", req.URL.Query().Get("doc"))
			require.NoError(t, err)
			require.NotNil(t, rd)
			assert.Equal(t, selected, rd.readme.Name)
			assert.Equal(t, "<pre>content of "+selected+"</pre>", string(rd.readme.HTML))
			require.Len(t, tabs, 2)
			for _, tab := range tabs {
				assert.Equal(t, tab.Name == selected, tab.Current, tab.Name)
				assert.Equal(t, "?doc="+tab.Name, tab.Href)
			}
		})
	}
}
