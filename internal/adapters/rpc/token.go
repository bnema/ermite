// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// tokenService implements ermiteconnect.TokenServiceHandler.
type tokenService struct{ tokens ports.TokenService }

func (s *tokenService) CreateToken(ctx context.Context, req *pb.CreateTokenRequest) (*pb.CreateTokenResponse, error) {
	params := ports.CreateTokenParams{
		Label:     req.GetLabel(),
		ExpiresAt: goTime(req.GetExpiresAt()),
	}
	for _, sc := range req.GetScopes() {
		scope, err := scopeFromPB(sc)
		if err != nil {
			return nil, connectError(ctx, err)
		}
		params.Scopes = append(params.Scopes, scope)
	}
	for _, r := range req.GetRepos() {
		name, err := domain.ParseRepoName(r)
		if err != nil {
			return nil, connectError(ctx, err)
		}
		params.Repos = append(params.Repos, name)
	}
	params.BranchPrefixes = req.GetBranchPrefixes()
	created, err := s.tokens.Create(ctx, caller(ctx), params)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.CreateTokenResponse{Token: tokenToPB(created.Token), Secret: created.Secret}, nil
}

func (s *tokenService) ListTokens(ctx context.Context, req *pb.ListTokensRequest) (*pb.ListTokensResponse, error) {
	page, err := s.tokens.ListPage(ctx, caller(ctx), int(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListTokensResponse{NextPageToken: page.NextPageToken, Tokens: make([]*pb.Token, 0, len(page.Tokens))}
	for _, t := range page.Tokens {
		out.Tokens = append(out.Tokens, tokenToPB(t))
	}
	return out, nil
}

func (s *tokenService) RevokeToken(ctx context.Context, req *pb.RevokeTokenRequest) (*pb.RevokeTokenResponse, error) {
	if err := s.tokens.Revoke(ctx, caller(ctx), req.GetId()); err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.RevokeTokenResponse{}, nil
}
