// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"encoding/base64"
	"fmt"

	"connectrpc.com/connect/v2"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// errPasskeysUnavailable is what the passkey RPCs answer when the server runs
// without passkeys (no https public URL with a DNS host name).
var errPasskeysUnavailable = connect.NewError(connect.CodeFailedPrecondition, "passkeys are not available on this server")

// sessionService implements ermiteconnect.SessionServiceHandler. The browser
// never calls it: sessions come from the link or a passkey, and the API
// ignores cookies. passkeys is nil when passkeys are not available.
type sessionService struct {
	sessions ports.SessionService
	passkeys ports.PasskeyService
}

func (s *sessionService) CreateLoginLink(ctx context.Context, _ *pb.CreateLoginLinkRequest) (*pb.CreateLoginLinkResponse, error) {
	link, err := s.sessions.CreateLoginLink(ctx, caller(ctx))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.CreateLoginLinkResponse{Url: link.URL, ExpiresAt: timestamp(link.ExpiresAt)}, nil
}

func (s *sessionService) SignOutEverywhere(ctx context.Context, _ *pb.SignOutEverywhereRequest) (*pb.SignOutEverywhereResponse, error) {
	if err := s.sessions.SignOutEverywhere(ctx, caller(ctx)); err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.SignOutEverywhereResponse{}, nil
}

func (s *sessionService) ListPasskeys(ctx context.Context, _ *pb.ListPasskeysRequest) (*pb.ListPasskeysResponse, error) {
	if s.passkeys == nil {
		return nil, errPasskeysUnavailable
	}
	list, err := s.passkeys.List(ctx, caller(ctx))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.ListPasskeysResponse{Passkeys: passkeysToPB(list)}, nil
}

func (s *sessionService) RemovePasskey(ctx context.Context, req *pb.RemovePasskeyRequest) (*pb.RemovePasskeyResponse, error) {
	if s.passkeys == nil {
		return nil, errPasskeysUnavailable
	}
	id, err := base64.RawURLEncoding.DecodeString(req.GetId())
	if err != nil || len(id) == 0 {
		return nil, connectError(ctx, fmt.Errorf("%w: id is not a base64url credential id", domain.ErrInvalid))
	}
	if err := s.passkeys.Remove(ctx, caller(ctx), id); err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.RemovePasskeyResponse{}, nil
}
