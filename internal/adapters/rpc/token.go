// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
)

// tokenService implements ermiteconnect.TokenServiceHandler.
type tokenService struct{ tokens *app.Tokens }

func (s *tokenService) CreateToken(ctx context.Context, req *connect.Request[pb.CreateTokenRequest]) (*connect.Response[pb.CreateTokenResponse], error) {
	params := app.CreateTokenParams{
		Label:     req.Msg.GetLabel(),
		ExpiresAt: goTime(req.Msg.GetExpiresAt()),
	}
	for _, sc := range req.Msg.GetScopes() {
		scope, err := scopeFromPB(sc)
		if err != nil {
			return nil, connectError(ctx, err)
		}
		params.Scopes = append(params.Scopes, scope)
	}
	for _, r := range req.Msg.GetRepos() {
		name, err := domain.ParseRepoName(r)
		if err != nil {
			return nil, connectError(ctx, err)
		}
		params.Repos = append(params.Repos, name)
	}
	created, err := s.tokens.Create(ctx, caller(ctx), params)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.CreateTokenResponse{Token: tokenToPB(created.Token), Secret: created.Secret}), nil
}

func (s *tokenService) ListTokens(ctx context.Context, req *connect.Request[pb.ListTokensRequest]) (*connect.Response[pb.ListTokensResponse], error) {
	page, err := s.tokens.ListPage(ctx, caller(ctx), int(req.Msg.GetPageSize()), req.Msg.GetPageToken())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListTokensResponse{NextPageToken: page.NextPageToken, Tokens: make([]*pb.Token, 0, len(page.Tokens))}
	for _, t := range page.Tokens {
		out.Tokens = append(out.Tokens, tokenToPB(t))
	}
	return connect.NewResponse(out), nil
}

func (s *tokenService) RevokeToken(ctx context.Context, req *connect.Request[pb.RevokeTokenRequest]) (*connect.Response[pb.RevokeTokenResponse], error) {
	if err := s.tokens.Revoke(ctx, caller(ctx), req.Msg.GetId()); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.RevokeTokenResponse{}), nil
}
