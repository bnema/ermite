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

func TestRawVideoTypes(t *testing.T) {
	for _, tc := range []struct {
		name, head, typ string
		attachment      bool
	}{
		{"mp4", "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom", "video/mp4", false},
		{"mov", "\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00qt  ", "video/quicktime", false},
		{"m4v", "\x00\x00\x00\x14ftypM4V \x00\x00\x00\x00M4V ", "video/mp4", false},
		{"webm", "\x1a\x45\xdf\xa3\x87\x42\x82\x84webm", "video/webm", false},
		{"ogg", "OggS\x00header\x80theora", "video/ogg", false},
		{"ogg audio", "OggS\x00headerOpusHead", "application/octet-stream", true},
		{"html", "<html><script>alert(1)</script>", "application/octet-stream", true},
		{"svg", `<svg onload="alert(1)">`, "application/octet-stream", true},
		{"unknown", "\x00unknown", "application/octet-stream", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typ, attachment := rawType(&blobStream{buf: []byte(tc.head), info: domain.BlobInfo{Binary: true}})
			assert.Equal(t, tc.typ, typ)
			assert.Equal(t, tc.attachment, attachment)
		})
	}
}

func TestRawVideoRanges(t *testing.T) {
	body := "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom" + strings.Repeat("0123456789", 100)
	for _, tc := range []struct {
		name, method, rng, ifRange string
		status                     int
		start, end                 int
		cr                         string
	}{
		{"whole", "GET", "", "", 200, 0, len(body), ""},
		{"prefix", "GET", "bytes=2-4", "", 206, 2, 5, "bytes 2-4/1024"},
		{"after sniff buffer", "GET", "bytes=700-709", "", 206, 700, 710, "bytes 700-709/1024"},
		{"suffix", "GET", "bytes=-4", "", 206, 1020, 1024, "bytes 1020-1023/1024"},
		{"open", "GET", "bytes=1020-", "", 206, 1020, 1024, "bytes 1020-1023/1024"},
		{"clipped end", "GET", "bytes=1020-9999", "", 206, 1020, 1024, "bytes 1020-1023/1024"},
		{"past size", "GET", "bytes=1024-", "", 416, 0, 0, "bytes */1024"},
		{"invalid", "GET", "bytes=a-b", "", 416, 0, 0, "bytes */1024"},
		{"multiple ignored", "GET", "bytes=0-1,4-5", "", 200, 0, len(body), ""},
		{"stale if-range", "GET", "bytes=2-4", `"old"`, 200, 0, len(body), ""},
		{"matched if-range", "GET", "bytes=2-4", `"` + blobID + `"`, 206, 2, 5, "bytes 2-4/1024"},
		{"head", "HEAD", "bytes=2-4", "", 200, 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			browse := mocks.NewBrowseService(t)
			browse.EXPECT().OpenBlob(mock.Anything, domain.Anonymous(), repoName(t, "r"), mock.Anything, mock.Anything).Return(
				domain.BlobInfo{ID: objectID(t, blobID), Path: "clip.mp4", Size: int64(len(body)), Binary: true}, io.NopCloser(strings.NewReader(body)), nil).Once()
			browse.EXPECT().ListRefs(mock.Anything, domain.Anonymous(), repoName(t, "r"), mock.Anything).Return(ports.RefPage{Refs: []domain.Ref{{Name: "refs/heads/main", ShortName: "main", Kind: domain.RefKindBranch, ID: objectID(t, mainTip), CommitID: objectID(t, mainTip)}}}, nil).Maybe()
			srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: browse})
			req := httptest.NewRequest(tc.method, "/r/raw/main/clip.mp4", nil)
			req.Header.Set("Range", tc.rng)
			req.Header.Set("If-Range", tc.ifRange)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, body[tc.start:tc.end], rec.Body.String())
			assert.Equal(t, tc.cr, rec.Header().Get("Content-Range"))
			assert.Equal(t, "bytes", rec.Header().Get("Accept-Ranges"))
			assert.Equal(t, "video/mp4", rec.Header().Get("Content-Type"))
			assert.Empty(t, rec.Header().Get("Content-Disposition"))
			assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
			assert.Equal(t, rawCSP, rec.Header().Get("Content-Security-Policy"))
		})
	}
}
