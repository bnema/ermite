// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/rpc/mocks"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
)

func TestTokenServiceCreateToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		req      *pb.CreateTokenRequest
		usecase  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "ok", req: &pb.CreateTokenRequest{Label: "ci", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}, Repos: []string{"demo"}}, wantCall: true},
		{name: "unspecified scope", req: &pb.CreateTokenRequest{Label: "ci", Scopes: []pb.Scope{pb.Scope_SCOPE_UNSPECIFIED}}, wantCode: connect.CodeInvalidArgument},
		{name: "invalid repo name", req: &pb.CreateTokenRequest{Label: "ci", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}, Repos: []string{"../x"}}, wantCode: connect.CodeInvalidArgument},
		{name: "forbidden", req: &pb.CreateTokenRequest{Label: "ci"}, usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied, wantCall: true},
		{name: "anonymous", req: &pb.CreateTokenRequest{Label: "ci"}, usecase: domain.ErrUnauthenticated, wantCode: connect.CodeUnauthenticated, wantCall: true},
		{name: "invalid fields", req: &pb.CreateTokenRequest{}, usecase: domain.ErrInvalid, wantCode: connect.CodeInvalidArgument, wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tokens := mocks.NewTokens(t)
			if tt.wantCall {
				tokens.EXPECT().Create(mock.Anything, domain.Owner(), mock.Anything).
					Return(app.CreatedToken{Token: domain.Token{ID: "t1", Label: "ci"}, Secret: "s3cret"}, tt.usecase).Once()
			}
			resp, err := (&tokenService{tokens}).CreateToken(ownerCtx(), connect.NewRequest(tt.req))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "s3cret", resp.Msg.GetSecret())
			assert.Equal(t, "t1", resp.Msg.GetToken().GetId())
		})
	}
}

func TestTokenServiceCreateTokenPassesParams(t *testing.T) {
	t.Parallel()
	tokens := mocks.NewTokens(t)
	tokens.EXPECT().Create(mock.Anything, domain.Owner(), mock.MatchedBy(func(p app.CreateTokenParams) bool {
		return p.Label == "ci" && len(p.Scopes) == 1 && len(p.Repos) == 1 && p.Repos[0].String() == "demo"
	})).Return(app.CreatedToken{}, nil).Once()
	_, err := (&tokenService{tokens}).CreateToken(ownerCtx(), connect.NewRequest(&pb.CreateTokenRequest{
		Label: "ci", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}, Repos: []string{"demo"},
	}))
	require.NoError(t, err)
}

func TestTokenServiceRevokeToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		usecase  error
		wantCode connect.Code
	}{
		{name: "ok"},
		{name: "not found", usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound},
		{name: "forbidden", usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tokens := mocks.NewTokens(t)
			tokens.EXPECT().Revoke(mock.Anything, domain.Owner(), "t1").Return(tt.usecase).Once()
			_, err := (&tokenService{tokens}).RevokeToken(ownerCtx(), connect.NewRequest(&pb.RevokeTokenRequest{Id: "t1"}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestTokenServiceListTokens(t *testing.T) {
	t.Parallel()
	tokens := mocks.NewTokens(t)
	tokens.EXPECT().ListPage(mock.Anything, domain.Owner(), 5, "tok").
		Return(app.TokenPage{Tokens: []domain.Token{{ID: "a"}, {ID: "b"}}, NextPageToken: "n"}, nil).Once()
	resp, err := (&tokenService{tokens}).ListTokens(ownerCtx(), connect.NewRequest(&pb.ListTokensRequest{PageSize: 5, PageToken: "tok"}))
	require.NoError(t, err)
	assert.Len(t, resp.Msg.GetTokens(), 2)
	assert.Equal(t, "n", resp.Msg.GetNextPageToken())

	tokens.EXPECT().ListPage(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(app.TokenPage{}, domain.ErrInvalidPageToken).Once()
	_, err = (&tokenService{tokens}).ListTokens(ownerCtx(), connect.NewRequest(&pb.ListTokensRequest{}))
	requireCode(t, connect.CodeInvalidArgument, err)
}
