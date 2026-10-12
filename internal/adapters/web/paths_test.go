// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// pathsGit is a Git client that resolves any revision to itself and counts
// the lists it builds.
type pathsGit struct {
	ermiteconnect.GitServiceClient
	lists atomic.Int32
	paths []string // the list; a.go and b/c.go when nil
}

func (g *pathsGit) ResolveRevision(_ context.Context, req *connect.Request[pb.ResolveRevisionRequest]) (*connect.Response[pb.ResolveRevisionResponse], error) {
	return connect.NewResponse(&pb.ResolveRevisionResponse{CommitId: req.Msg.GetRevision()}), nil
}

func (g *pathsGit) ListPaths(_ context.Context, req *connect.Request[pb.ListPathsRequest]) (*connect.Response[pb.ListPathsResponse], error) {
	g.lists.Add(1)
	paths := g.paths
	if paths == nil {
		paths = []string{"a.go", "b/c.go"}
	}
	return connect.NewResponse(&pb.ListPathsResponse{CommitId: req.Msg.GetRevision(), Paths: paths}), nil
}

// A revalidation is answered after the access check and before the list is
// loaded: it never builds. Each variant has its own validator.
func TestPathsRevalidationDoesNotBuild(t *testing.T) {
	git := &pathsGit{}
	srv, err := New(Config{
		Repos: failing{}, Git: git,
		PublicURL: "https://forge.example", SourceURL: "https://forge.example/ermite", Version: "v1",
	})
	require.NoError(t, err)
	sha := strings.Repeat("a", 40)
	get := func(hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/r/paths/"+sha, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	gzTag, plainTag := `"paths-`+sha+`-gz"`, `"paths-`+sha+`"`

	for _, tt := range []struct{ name, ae, inm string }{
		{"gzip", "gzip", gzTag},
		{"identity", "", plainTag},
		{"weak", "gzip", "W/" + gzTag},
		{"any", "", "*"},
	} {
		rec := get(map[string]string{"Accept-Encoding": tt.ae, "If-None-Match": tt.inm})
		assert.Equal(t, http.StatusNotModified, rec.Code, tt.name)
		assert.Empty(t, rec.Body.String(), tt.name)
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"), tt.name)
		assert.Equal(t, "Accept-Encoding", rec.Header().Get("Vary"), tt.name)
		assert.NotEmpty(t, rec.Header().Get("ETag"), tt.name)
	}
	assert.Zero(t, git.lists.Load(), "a 304 never builds")

	// The validator of the other variant does not match.
	assert.Equal(t, http.StatusOK, get(map[string]string{"Accept-Encoding": "gzip", "If-None-Match": plainTag}).Code)
	assert.Equal(t, http.StatusOK, get(map[string]string{"If-None-Match": gzTag}).Code)
	assert.EqualValues(t, 1, git.lists.Load(), "built once, then cached")

	rec := get(map[string]string{"Accept-Encoding": "gzip"})
	assert.Equal(t, gzTag, rec.Header().Get("ETag"))
	assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	zr, err := gzip.NewReader(rec.Body)
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.JSONEq(t, `{"paths":["a.go","b/c.go"],"truncated":false}`, string(out))

	rec = get(nil)
	assert.Equal(t, plainTag, rec.Header().Get("ETag"))
	assert.Empty(t, rec.Header().Get("Content-Encoding"))
	assert.JSONEq(t, `{"paths":["a.go","b/c.go"],"truncated":false}`, rec.Body.String())
}

// The cached list holds no spare capacity.
func TestBuildPathsCachesATightBody(t *testing.T) {
	srv, err := New(Config{Repos: failing{}, Git: &pathsGit{}, PublicURL: "https://forge.example"})
	require.NoError(t, err)
	body, err := srv.buildPaths(context.Background(), "r", strings.Repeat("a", 40))
	require.NoError(t, err)
	assert.Equal(t, len(body), cap(body))
}

func TestAcceptsGzip(t *testing.T) {
	for _, tt := range []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"GZip", true},
		{"deflate, gzip", true},
		{"deflate, br", false},
		{"gzip;q=0", false},
		{"gzip; q=0", false},
		{"gzip;q=0.0", false},
		{"gzip ; q = 0", false},
		{"gzip;q=0.5", true},
		{"gzip;q=1", true},
		{"identity, gzip;q=0", false},
		{"gzip;q=0, deflate", false},
		{"gzip;q=bad", true},
		{"x-gzip", false},
		// A wildcard is not read: the finder asks for gzip by name, and the
		// plain body is always right.
		{"*", false},
	} {
		assert.Equal(t, tt.want, acceptsGzip(tt.header), "%q", tt.header)
	}
}

func TestEtagMatches(t *testing.T) {
	const tag = `"paths-x"`
	for _, tt := range []struct {
		header string
		want   bool
	}{
		{"", false},
		{tag, true},
		{"W/" + tag, true},
		{`"other"`, false},
		{`"a", ` + tag, true},
		{`"a",` + tag + `,"b"`, true},
		{`"a", W/` + tag, true},
		{`"a", "b"`, false},
		{"*", true},
		{`"a", *`, true},
		{`"paths-x-gz"`, false},
		{`w/` + tag, false},
	} {
		assert.Equal(t, tt.want, etagMatches(tt.header, tag), "%q", tt.header)
	}
}

// A path is not HTML: "<", ">" and "&" are not escaped in the JSON.
func TestBuildPathsDoesNotEscapeHTML(t *testing.T) {
	git := &pathsGit{paths: []string{"a&b/<c>.go"}}
	srv, err := New(Config{Repos: failing{}, Git: git, PublicURL: "https://forge.example"})
	require.NoError(t, err)
	body, err := srv.buildPaths(context.Background(), "r", strings.Repeat("a", 40))
	require.NoError(t, err)
	zr, err := gzip.NewReader(bytes.NewReader(body))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"a&b/<c>.go"`)
}
