// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/adapters/web/mocks"
)

const blobID = "0123456789abcdef0123456789abcdef01234567"

// blobAPI serves one text file, the only call raw makes besides the refs.
type blobAPI struct {
	ermiteconnect.UnimplementedGitServiceHandler
}

func (*blobAPI) GetBlob(_ context.Context, req *connect.Request[pb.GetBlobRequest], st *connect.ServerStream[pb.GetBlobResponse]) error {
	if req.Msg.GetPath() != "docs/a.txt" {
		return connect.NewError(connect.CodeNotFound, assert.AnError)
	}
	return st.Send(&pb.GetBlobResponse{
		Info:  &pb.BlobInfo{Id: blobID, Path: "docs/a.txt", Size: 5, MediaType: "text/plain"},
		Chunk: []byte("hello"),
	})
}

// A revalidation of an unchanged file ends with a 304 and no body. The blob is
// opened in every case: its id is only known from the stream.
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
		{"weak validator is not a match", "/r/raw/main/docs/a.txt", http.MethodGet, `W/"` + blobID + `"`, http.StatusOK, "hello"},
		{"head", "/r/raw/main/docs/a.txt", http.MethodHead, "", http.StatusOK, ""},
		{"missing file", "/r/raw/main/docs/none.txt", http.MethodGet, `"` + blobID + `"`, http.StatusNotFound, ""},
		{"directory", "/r/raw/main", http.MethodGet, `"` + blobID + `"`, http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repos := mocks.NewRepoServiceClient(t)
			repos.EXPECT().GetRepo(mock.Anything, mock.Anything).
				Return(connect.NewResponse(&pb.GetRepoResponse{Repo: &pb.Repo{Name: "r", DefaultBranch: "main"}}), nil)
			mux := http.NewServeMux()
			mux.Handle(ermiteconnect.NewGitServiceHandler(&refsGit{}))
			backend := httptest.NewServer(mux)
			t.Cleanup(backend.Close)

			srv, err := New(Config{
				Repos: repos, Git: ermiteconnect.NewGitServiceClient(backend.Client(), backend.URL),
				PublicURL: "https://forge.example", SourceURL: "https://forge.example/ermite", Version: "v1",
			})
			require.NoError(t, err)
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

// refsGit adds the refs of the repository to blobAPI.
type refsGit struct {
	blobAPI
}

func (g *refsGit) ListRefs(context.Context, *connect.Request[pb.ListRefsRequest]) (*connect.Response[pb.ListRefsResponse], error) {
	return connect.NewResponse(&pb.ListRefsResponse{Refs: []*pb.Ref{
		{Name: "refs/heads/main", ShortName: "main", Kind: pb.RefKind_REF_KIND_BRANCH, CommitId: "c0ffee0"},
	}}), nil
}
