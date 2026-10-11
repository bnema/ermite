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
	"bnema.dev/ermite/internal/adapters/web/mocks"
)

const blobID = "0123456789abcdef0123456789abcdef01234567"

// A revalidation of an unchanged file is answered from the tree entry, before
// the content is opened; anything else opens the file.
func TestRawChecksETagBeforeOpeningTheBlob(t *testing.T) {
	entry := func(kind pb.EntryKind, id string) *pb.TreeEntry {
		return &pb.TreeEntry{Name: "big.bin", Path: "docs/big.bin", Kind: kind, Id: id}
	}
	tests := []struct {
		name      string
		url       string
		ifNone    string
		wantDir   string
		tree      *pb.GetTreeResponse
		treeErr   error
		want      int
		wantOpens int // GetBlob calls
	}{
		{
			name: "unchanged file", url: "/r/raw/main/docs/big.bin", ifNone: `"` + blobID + `"`, wantDir: "docs",
			tree: &pb.GetTreeResponse{Entries: []*pb.TreeEntry{entry(pb.EntryKind_ENTRY_KIND_FILE, blobID)}},
			want: http.StatusNotModified,
		},
		{
			name: "unchanged executable", url: "/r/raw/main/docs/big.bin", ifNone: `"` + blobID + `"`, wantDir: "docs",
			tree: &pb.GetTreeResponse{Entries: []*pb.TreeEntry{entry(pb.EntryKind_ENTRY_KIND_EXECUTABLE, blobID)}},
			want: http.StatusNotModified,
		},
		{
			name: "file at the root", url: "/r/raw/main/big.bin", ifNone: `"` + blobID + `"`, wantDir: "",
			tree: &pb.GetTreeResponse{Entries: []*pb.TreeEntry{{Name: "big.bin", Path: "big.bin", Kind: pb.EntryKind_ENTRY_KIND_FILE, Id: blobID}}},
			want: http.StatusNotModified,
		},
		{
			name: "changed file", url: "/r/raw/main/docs/big.bin", ifNone: `"` + blobID + `"`, wantDir: "docs",
			tree: &pb.GetTreeResponse{Entries: []*pb.TreeEntry{entry(pb.EntryKind_ENTRY_KIND_FILE, "ffffffffffffffffffffffffffffffffffffffff")}},
			want: http.StatusNotFound, wantOpens: 1,
		},
		{
			name: "directory with that id", url: "/r/raw/main/docs/big.bin", ifNone: `"` + blobID + `"`, wantDir: "docs",
			tree: &pb.GetTreeResponse{Entries: []*pb.TreeEntry{entry(pb.EntryKind_ENTRY_KIND_DIRECTORY, blobID)}},
			want: http.StatusNotFound, wantOpens: 1,
		},
		{
			name: "entry not listed", url: "/r/raw/main/docs/big.bin", ifNone: `"` + blobID + `"`, wantDir: "docs",
			tree: &pb.GetTreeResponse{Truncated: true},
			want: http.StatusNotFound, wantOpens: 1,
		},
		{
			name: "tree lookup fails", url: "/r/raw/main/docs/big.bin", ifNone: `"` + blobID + `"`, wantDir: "docs",
			treeErr: connect.NewError(connect.CodeNotFound, assert.AnError),
			want:    http.StatusNotFound, wantOpens: 1,
		},
		{
			name: "weak validator is not a match", url: "/r/raw/main/docs/big.bin", ifNone: `W/"` + blobID + `"`, wantDir: "docs",
			tree: &pb.GetTreeResponse{Entries: []*pb.TreeEntry{entry(pb.EntryKind_ENTRY_KIND_FILE, blobID)}},
			want: http.StatusNotFound, wantOpens: 1,
		},
		{
			name: "no validator", url: "/r/raw/main/docs/big.bin", ifNone: "",
			want: http.StatusNotFound, wantOpens: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repos := mocks.NewRepoServiceClient(t)
			git := mocks.NewGitServiceClient(t)
			repos.EXPECT().GetRepo(mock.Anything, mock.Anything).
				Return(connect.NewResponse(&pb.GetRepoResponse{Repo: &pb.Repo{Name: "r", DefaultBranch: "main"}}), nil)
			git.EXPECT().ListRefs(mock.Anything, mock.Anything).
				Return(connect.NewResponse(&pb.ListRefsResponse{Refs: []*pb.Ref{
					{Name: "refs/heads/main", ShortName: "main", Kind: pb.RefKind_REF_KIND_BRANCH, CommitId: "c0ffee0"},
				}}), nil)
			if tt.ifNone != "" {
				git.EXPECT().GetTree(mock.Anything, mock.MatchedBy(func(req *connect.Request[pb.GetTreeRequest]) bool {
					m := req.Msg
					return m.GetRepo() == "r" && m.GetRevision() == "c0ffee0" && m.GetPath() == tt.wantDir && !m.GetIncludeLastCommit()
				})).Return(connect.NewResponse(tt.tree), tt.treeErr).Once()
			}
			opens := 0
			git.EXPECT().GetBlob(mock.Anything, mock.Anything).RunAndReturn(
				func(_ context.Context, _ *connect.Request[pb.GetBlobRequest]) (*connect.ServerStreamForClient[pb.GetBlobResponse], error) {
					opens++
					return nil, connect.NewError(connect.CodeNotFound, assert.AnError)
				}).Maybe()

			srv, err := New(Config{
				Repos: repos, Git: git,
				PublicURL: "https://forge.example", SourceURL: "https://forge.example/ermite", Version: "v1",
			})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			if tt.ifNone != "" {
				req.Header.Set("If-None-Match", tt.ifNone)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			assert.Equal(t, tt.want, rec.Code)
			assert.Equal(t, tt.wantOpens, opens, "GetBlob calls")
			if tt.want == http.StatusNotModified {
				assert.Equal(t, tt.ifNone, rec.Header().Get("ETag"))
				assert.Equal(t, rawCSP, rec.Header().Get("Content-Security-Policy"))
				assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
				assert.Empty(t, rec.Body.Bytes())
			}
		})
	}
}
