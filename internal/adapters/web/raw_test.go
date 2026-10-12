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

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

const (
	blobID   = "0123456789abcdef0123456789abcdef01234567"
	mainTip  = "c0ffee0c0ffee0c0ffee0c0ffee0c0ffee0c0ffe"
	blobBody = "hello"
)

// rawBrowse serves one text file, the only call raw makes besides the refs.
func rawBrowse(t *testing.T) *mocks.BrowseService {
	t.Helper()
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListRefs(mock.Anything, domain.Anonymous(), repoName(t, "r"), mock.Anything).Return(ports.RefPage{
		Refs: []domain.Ref{{
			Name: "refs/heads/main", ShortName: "main", Kind: domain.RefKindBranch,
			ID: objectID(t, mainTip), CommitID: objectID(t, mainTip),
		}},
	}, nil).Maybe()
	browse.EXPECT().OpenBlob(mock.Anything, domain.Anonymous(), repoName(t, "r"), mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.Principal, _ domain.RepoName, rev domain.Revision, path domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
			if path.String() != "docs/a.txt" || rev.String() != mainTip {
				return domain.BlobInfo{}, nil, domain.ErrNotFound
			}
			return domain.BlobInfo{ID: objectID(t, blobID), Path: "docs/a.txt", Size: int64(len(blobBody)), MediaType: "text/plain"},
				io.NopCloser(strings.NewReader(blobBody)), nil
		}).Maybe()
	return browse
}

// A revalidation of an unchanged file ends with a 304 and no body. The blob is
// opened in every case: its id is only known from the metadata the use case
// returns with the content.
func TestRawRevalidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		url    string
		method string
		ifNone string
		want   int
		body   string
	}{
		{"no validator", "/r/raw/main/docs/a.txt", http.MethodGet, "", http.StatusOK, "hello"},
		{"matching validator", "/r/raw/main/docs/a.txt", http.MethodGet, `"` + blobID + `"`, http.StatusNotModified, ""},
		{"other validator", "/r/raw/main/docs/a.txt", http.MethodGet, `"ffffffffffffffffffffffffffffffffffffffff"`, http.StatusOK, "hello"},
		{"weak validator matches (weak comparison)", "/r/raw/main/docs/a.txt", http.MethodGet, `W/"` + blobID + `"`, http.StatusNotModified, ""},
		{"list of validators", "/r/raw/main/docs/a.txt", http.MethodGet, `"other", "` + blobID + `"`, http.StatusNotModified, ""},
		{"any validator", "/r/raw/main/docs/a.txt", http.MethodGet, "*", http.StatusNotModified, ""},
		{"list without a match", "/r/raw/main/docs/a.txt", http.MethodGet, `"other", "another"`, http.StatusOK, "hello"},
		{"head", "/r/raw/main/docs/a.txt", http.MethodHead, "", http.StatusOK, ""},
		{"missing file", "/r/raw/main/docs/none.txt", http.MethodGet, `"` + blobID + `"`, http.StatusNotFound, ""},
		{"directory", "/r/raw/main", http.MethodGet, `"` + blobID + `"`, http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repos := reposServing(t, publicRepo(t, "r"))
			srv := newTestServer(t, Config{Repos: repos, Browse: rawBrowse(t)})
			req := httptest.NewRequest(tt.method, tt.url, nil)
			if tt.ifNone != "" {
				req.Header.Set("If-None-Match", tt.ifNone)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			assert.Equal(t, tt.want, rec.Code)
			if tt.want != http.StatusNotFound {
				assert.Equal(t, tt.body, rec.Body.String())
			}
			if tt.want == http.StatusNotModified {
				assert.Equal(t, `"`+blobID+`"`, rec.Header().Get("ETag"))
				assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
			}
			if tt.want == http.StatusOK {
				assert.Equal(t, `"`+blobID+`"`, rec.Header().Get("ETag"))
				assert.Equal(t, rawCSP, rec.Header().Get("Content-Security-Policy"))
			}
		})
	}
}
