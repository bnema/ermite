// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// ownerCtx is the context of a call made by the owner, as the interceptor
// leaves it.
func ownerCtx() context.Context {
	return domain.ContextWithPrincipal(context.Background(), domain.Owner())
}

func mustRepoName(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}

func requireCode(t *testing.T, want connect.Code, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, want, connect.CodeOf(err), err.Error())
}

func TestRepoServiceGetRepo(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk on fire")
	tests := []struct {
		name     string
		repo     string
		usecase  error
		viewErr  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "ok", repo: "demo", wantCall: true},
		{name: "invalid name never reaches the use case", repo: "../etc", wantCode: connect.CodeInvalidArgument},
		{name: "not found", repo: "demo", usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound, wantCall: true},
		{name: "forbidden", repo: "demo", usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied, wantCall: true},
		{name: "unauthenticated", repo: "demo", usecase: domain.ErrUnauthenticated, wantCode: connect.CodeUnauthenticated, wantCall: true},
		{name: "unknown error is internal and opaque", repo: "demo", usecase: boom, wantCode: connect.CodeInternal, wantCall: true},
		{name: "view failure", repo: "demo", viewErr: domain.ErrNotFound, wantCode: connect.CodeNotFound, wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repos := mocks.NewRepoService(t)
			if tt.wantCall {
				repo := domain.Repo{Name: mustRepoName(t, "demo"), Description: "d", Visibility: domain.VisibilityPublic}
				repos.EXPECT().Get(mock.Anything, domain.Owner(), repo.Name).Return(repo, tt.usecase).Once()
				if tt.usecase == nil {
					repos.EXPECT().View(mock.Anything, repo).Return(ports.RepoView{Repo: repo, DefaultBranch: "main"}, tt.viewErr).Once()
				}
			}
			svc := &repoService{repos}
			resp, err := svc.GetRepo(ownerCtx(), connect.NewRequest(&pb.GetRepoRequest{Repo: tt.repo}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				if errors.Is(tt.usecase, boom) {
					assert.NotContains(t, err.Error(), "disk on fire")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "demo", resp.Msg.GetRepo().GetName())
			assert.Equal(t, "main", resp.Msg.GetRepo().GetDefaultBranch())
		})
	}
}

func TestRepoServiceCreateRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		req      *pb.CreateRepoRequest
		usecase  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "ok", req: &pb.CreateRepoRequest{Name: "demo", Topics: []string{"go", "cli"}, Visibility: pb.Visibility_VISIBILITY_PRIVATE}, wantCall: true},
		{name: "invalid name", req: &pb.CreateRepoRequest{Name: "Bad Name", Visibility: pb.Visibility_VISIBILITY_PRIVATE}, wantCode: connect.CodeInvalidArgument},
		{name: "unknown visibility", req: &pb.CreateRepoRequest{Name: "demo", Visibility: pb.Visibility(99)}, wantCode: connect.CodeInvalidArgument},
		{name: "already exists", req: &pb.CreateRepoRequest{Name: "demo"}, usecase: domain.ErrAlreadyExists, wantCode: connect.CodeAlreadyExists, wantCall: true},
		{name: "forbidden", req: &pb.CreateRepoRequest{Name: "demo"}, usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied, wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repos := mocks.NewRepoService(t)
			if tt.wantCall {
				repo := domain.Repo{Name: mustRepoName(t, "demo"), Visibility: domain.VisibilityPrivate}
				repos.EXPECT().Create(mock.Anything, domain.Owner(), mock.MatchedBy(func(p ports.CreateRepoParams) bool {
					return p.Name.String() == "demo" && slices.Equal(p.Topics, tt.req.GetTopics())
				})).Return(repo, tt.usecase).Once()
				if tt.usecase == nil {
					repos.EXPECT().View(mock.Anything, repo).Return(ports.RepoView{Repo: repo}, nil).Once()
				}
			}
			resp, err := (&repoService{repos}).CreateRepo(ownerCtx(), connect.NewRequest(tt.req))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "demo", resp.Msg.GetRepo().GetName())
			assert.Equal(t, pb.Visibility_VISIBILITY_PRIVATE, resp.Msg.GetRepo().GetVisibility())
		})
	}
}

func TestRepoServiceUpdateRepoValidatesMask(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		req      *pb.UpdateRepoRequest
		usecase  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "empty mask", req: &pb.UpdateRepoRequest{Repo: "demo"}, wantCode: connect.CodeInvalidArgument},
		{
			name:     "unknown path",
			req:      &pb.UpdateRepoRequest{Repo: "demo", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}}},
			wantCode: connect.CodeInvalidArgument,
		},
		{
			name:     "unspecified visibility",
			req:      &pb.UpdateRepoRequest{Repo: "demo", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"visibility"}}},
			wantCode: connect.CodeInvalidArgument,
		},
		{
			name:     "invalid repo name",
			req:      &pb.UpdateRepoRequest{Repo: "", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}},
			wantCode: connect.CodeInvalidArgument,
		},
		{
			name:     "not found",
			req:      &pb.UpdateRepoRequest{Repo: "demo", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}},
			usecase:  domain.ErrNotFound,
			wantCode: connect.CodeNotFound, wantCall: true,
		},
		{
			name:     "precondition failed",
			req:      &pb.UpdateRepoRequest{Repo: "demo", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_branch"}}, DefaultBranch: "nope"},
			usecase:  domain.ErrPreconditionFailed,
			wantCode: connect.CodeFailedPrecondition, wantCall: true,
		},
		{
			name:     "ok",
			req:      &pb.UpdateRepoRequest{Repo: "demo", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}, Description: "new"},
			wantCall: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repos := mocks.NewRepoService(t)
			if tt.wantCall {
				repo := domain.Repo{Name: mustRepoName(t, "demo"), Description: "new"}
				repos.EXPECT().Update(mock.Anything, domain.Owner(), repo.Name, mock.Anything).Return(repo, tt.usecase).Once()
				if tt.usecase == nil {
					repos.EXPECT().View(mock.Anything, repo).Return(ports.RepoView{Repo: repo}, nil).Once()
				}
			}
			resp, err := (&repoService{repos}).UpdateRepo(ownerCtx(), connect.NewRequest(tt.req))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "new", resp.Msg.GetRepo().GetDescription())
		})
	}
}

func TestRepoServiceDeleteRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		repo     string
		usecase  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "ok", repo: "demo", wantCall: true},
		{name: "invalid name", repo: "a/../b", wantCode: connect.CodeInvalidArgument},
		{name: "not found", repo: "demo", usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound, wantCall: true},
		{name: "forbidden", repo: "demo", usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied, wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repos := mocks.NewRepoService(t)
			if tt.wantCall {
				repos.EXPECT().Delete(mock.Anything, domain.Owner(), mustRepoName(t, tt.repo)).Return(tt.usecase).Once()
			}
			_, err := (&repoService{repos}).DeleteRepo(ownerCtx(), connect.NewRequest(&pb.DeleteRepoRequest{Repo: tt.repo}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestRepoServiceListRepos(t *testing.T) {
	t.Parallel()
	t.Run("pages and views every repo", func(t *testing.T) {
		t.Parallel()
		repos := mocks.NewRepoService(t)
		a := domain.Repo{Name: mustRepoName(t, "a")}
		b := domain.Repo{Name: mustRepoName(t, "b")}
		repos.EXPECT().ListPage(mock.Anything, domain.Owner(), 2, "tok").Return(ports.RepoPage{Repos: []domain.Repo{a, b}, NextPageToken: "next"}, nil).Once()
		repos.EXPECT().View(mock.Anything, a).Return(ports.RepoView{Repo: a}, nil).Once()
		repos.EXPECT().View(mock.Anything, b).Return(ports.RepoView{Repo: b}, nil).Once()
		resp, err := (&repoService{repos}).ListRepos(ownerCtx(), connect.NewRequest(&pb.ListReposRequest{PageSize: 2, PageToken: "tok"}))
		require.NoError(t, err)
		require.Len(t, resp.Msg.GetRepos(), 2)
		assert.Equal(t, "next", resp.Msg.GetNextPageToken())
	})
	for name, tc := range map[string]struct {
		err  error
		code connect.Code
	}{
		"bad page token":  {domain.ErrInvalidPageToken, connect.CodeInvalidArgument},
		"bad page size":   {domain.ErrInvalidPageSize, connect.CodeInvalidArgument},
		"unauthenticated": {domain.ErrUnauthenticated, connect.CodeUnauthenticated},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repos := mocks.NewRepoService(t)
			repos.EXPECT().ListPage(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RepoPage{}, tc.err).Once()
			_, err := (&repoService{repos}).ListRepos(ownerCtx(), connect.NewRequest(&pb.ListReposRequest{}))
			requireCode(t, tc.code, err)
		})
	}
}

func TestUpdateParamsTopics(t *testing.T) {
	t.Parallel()
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

	params, err := updateParams(&pb.UpdateRepoRequest{UpdateMask: mask("topics"), Topics: []string{"go", "cli"}})
	require.NoError(t, err)
	require.NotNil(t, params.Topics)
	assert.Equal(t, []string{"go", "cli"}, *params.Topics)
	assert.Nil(t, params.Description, "only the masked fields are set")

	params, err = updateParams(&pb.UpdateRepoRequest{UpdateMask: mask("topics")})
	require.NoError(t, err)
	require.NotNil(t, params.Topics, "a masked empty list is a clear, not an absent field")
	assert.Empty(t, *params.Topics)

	params, err = updateParams(&pb.UpdateRepoRequest{UpdateMask: mask("description"), Topics: []string{"go"}})
	require.NoError(t, err)
	assert.Nil(t, params.Topics, "topics outside the mask are ignored")
}

func TestRepoToPBTopics(t *testing.T) {
	t.Parallel()
	got := repoToPB(ports.RepoView{Repo: domain.Repo{Name: mustRepoName(t, "demo"), Topics: []string{"cli", "go"}}})
	assert.Equal(t, []string{"cli", "go"}, got.GetTopics())
	assert.Empty(t, repoToPB(ports.RepoView{Repo: domain.Repo{Name: mustRepoName(t, "demo")}}).GetTopics())
}
