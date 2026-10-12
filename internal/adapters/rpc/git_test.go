// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestGitServiceGetTree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		req      *pb.GetTreeRequest
		usecase  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "ok", req: &pb.GetTreeRequest{Repo: "demo", Revision: "main", Path: "src"}, wantCall: true},
		{name: "invalid repo", req: &pb.GetTreeRequest{Repo: "../x", Revision: "main"}, wantCode: connect.CodeInvalidArgument},
		{name: "invalid path", req: &pb.GetTreeRequest{Repo: "demo", Revision: "main", Path: "a/../b"}, wantCode: connect.CodeInvalidArgument},
		{name: "repo not visible", req: &pb.GetTreeRequest{Repo: "demo", Revision: "main"}, usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound, wantCall: true},
		{name: "path is a file", req: &pb.GetTreeRequest{Repo: "demo", Revision: "main", Path: "f"}, usecase: domain.ErrNotDirectory, wantCode: connect.CodeInvalidArgument, wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			browse := mocks.NewBrowseService(t)
			if tt.wantCall {
				browse.EXPECT().GetTree(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), mock.Anything, mock.Anything, false).
					Return(domain.Tree{}, tt.usecase).Once()
			}
			resp, err := (&gitService{browse}).GetTree(ownerCtx(), connect.NewRequest(tt.req))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, resp.Msg.GetEntries())
		})
	}
}

func TestGitServiceListRefs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		req      *pb.ListRefsRequest
		usecase  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "ok", req: &pb.ListRefsRequest{Repo: "demo", PageSize: 10}, wantCall: true},
		{name: "unknown kind", req: &pb.ListRefsRequest{Repo: "demo", Kinds: []pb.RefKind{pb.RefKind(99)}}, wantCode: connect.CodeInvalidArgument},
		{name: "invalid repo", req: &pb.ListRefsRequest{Repo: ""}, wantCode: connect.CodeInvalidArgument},
		{name: "bad page token", req: &pb.ListRefsRequest{Repo: "demo", PageToken: "zz"}, usecase: domain.ErrInvalidPageToken, wantCode: connect.CodeInvalidArgument, wantCall: true},
		{name: "not found", req: &pb.ListRefsRequest{Repo: "demo"}, usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound, wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			browse := mocks.NewBrowseService(t)
			if tt.wantCall {
				browse.EXPECT().ListRefs(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), mock.Anything).
					Return(ports.RefPage{NextPageToken: "n"}, tt.usecase).Once()
			}
			resp, err := (&gitService{browse}).ListRefs(ownerCtx(), connect.NewRequest(tt.req))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "n", resp.Msg.GetNextPageToken())
		})
	}
}

func TestGitServiceGetCommitAndResolveRevision(t *testing.T) {
	t.Parallel()
	invalid := []struct{ name, repo, rev string }{
		{"invalid repo", "../x", "main"},
		{"invalid revision", "demo", "a b"},
	}
	for _, tt := range invalid {
		t.Run("GetCommit "+tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := (&gitService{mocks.NewBrowseService(t)}).GetCommit(ownerCtx(), connect.NewRequest(&pb.GetCommitRequest{Repo: tt.repo, Revision: tt.rev}))
			requireCode(t, connect.CodeInvalidArgument, err)
		})
		t.Run("ResolveRevision "+tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := (&gitService{mocks.NewBrowseService(t)}).ResolveRevision(ownerCtx(), connect.NewRequest(&pb.ResolveRevisionRequest{Repo: tt.repo, Revision: tt.rev}))
			requireCode(t, connect.CodeInvalidArgument, err)
		})
	}

	t.Run("GetCommit not found", func(t *testing.T) {
		t.Parallel()
		browse := mocks.NewBrowseService(t)
		browse.EXPECT().GetCommit(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), mock.Anything).Return(domain.CommitDetail{}, domain.ErrNotFound).Once()
		_, err := (&gitService{browse}).GetCommit(ownerCtx(), connect.NewRequest(&pb.GetCommitRequest{Repo: "demo", Revision: "main"}))
		requireCode(t, connect.CodeNotFound, err)
	})
	t.Run("ResolveRevision ok", func(t *testing.T) {
		t.Parallel()
		browse := mocks.NewBrowseService(t)
		browse.EXPECT().ResolveRevision(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), mock.Anything).
			Return(domain.ResolvedRevision{Ref: "refs/heads/main"}, nil).Once()
		resp, err := (&gitService{browse}).ResolveRevision(ownerCtx(), connect.NewRequest(&pb.ResolveRevisionRequest{Repo: "demo", Revision: "main"}))
		require.NoError(t, err)
		assert.Equal(t, "refs/heads/main", resp.Msg.GetRef())
	})
}

func TestGitServiceListCommits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		req      *pb.ListCommitsRequest
		usecase  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "ok", req: &pb.ListCommitsRequest{Repo: "demo", Revision: "main"}, wantCall: true},
		{name: "invalid path", req: &pb.ListCommitsRequest{Repo: "demo", Revision: "main", Path: "/abs/../x"}, wantCode: connect.CodeInvalidArgument},
		{name: "bad page size", req: &pb.ListCommitsRequest{Repo: "demo", Revision: "main", PageSize: -1}, usecase: domain.ErrInvalidPageSize, wantCode: connect.CodeInvalidArgument, wantCall: true},
		{name: "not found", req: &pb.ListCommitsRequest{Repo: "demo", Revision: "nope"}, usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound, wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			browse := mocks.NewBrowseService(t)
			if tt.wantCall {
				browse.EXPECT().ListCommits(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), mock.Anything).
					Return(ports.HistoryPage{}, tt.usecase).Once()
			}
			_, err := (&gitService{browse}).ListCommits(ownerCtx(), connect.NewRequest(tt.req))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// GetBlob validates before it opens anything or touches the stream.
func TestGitServiceGetBlobRejectsBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  *pb.GetBlobRequest
	}{
		{"invalid repo", &pb.GetBlobRequest{Repo: "../x", Revision: "main", Path: "f"}},
		{"missing path", &pb.GetBlobRequest{Repo: "demo", Revision: "main"}},
		{"invalid path", &pb.GetBlobRequest{Repo: "demo", Revision: "main", Path: "a/../b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := (&gitService{mocks.NewBrowseService(t)}).GetBlob(ownerCtx(), connect.NewRequest(tt.req), nil)
			requireCode(t, connect.CodeInvalidArgument, err)
		})
	}
}
