// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// errPasskeysUnavailable is what the passkey RPCs answer when the server runs
// without passkeys (no https public URL with a DNS host name).
var errPasskeysUnavailable = connect.NewError(connect.CodeFailedPrecondition, errors.New("passkeys are not available on this server"))

// sessionService implements ermiteconnect.SessionServiceHandler. The browser
// never calls it: sessions come from the link or a passkey, and the API
// ignores cookies. passkeys is nil when passkeys are not available.
type sessionService struct {
	sessions ports.SessionService
	passkeys ports.PasskeyService
}

func (s *sessionService) CreateLoginLink(ctx context.Context, _ *connect.Request[pb.CreateLoginLinkRequest]) (*connect.Response[pb.CreateLoginLinkResponse], error) {
	link, err := s.sessions.CreateLoginLink(ctx, caller(ctx))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.CreateLoginLinkResponse{Url: link.URL, ExpiresAt: timestamp(link.ExpiresAt)}), nil
}

func (s *sessionService) SignOutEverywhere(ctx context.Context, _ *connect.Request[pb.SignOutEverywhereRequest]) (*connect.Response[pb.SignOutEverywhereResponse], error) {
	if err := s.sessions.SignOutEverywhere(ctx, caller(ctx)); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.SignOutEverywhereResponse{}), nil
}

func (s *sessionService) ListPasskeys(ctx context.Context, _ *connect.Request[pb.ListPasskeysRequest]) (*connect.Response[pb.ListPasskeysResponse], error) {
	if s.passkeys == nil {
		return nil, errPasskeysUnavailable
	}
	list, err := s.passkeys.List(ctx, caller(ctx))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.ListPasskeysResponse{Passkeys: passkeysToPB(list)}), nil
}

func (s *sessionService) RemovePasskey(ctx context.Context, req *connect.Request[pb.RemovePasskeyRequest]) (*connect.Response[pb.RemovePasskeyResponse], error) {
	if s.passkeys == nil {
		return nil, errPasskeysUnavailable
	}
	id, err := base64.RawURLEncoding.DecodeString(req.Msg.GetId())
	if err != nil || len(id) == 0 {
		return nil, connectError(ctx, fmt.Errorf("%w: id is not a base64url credential id", domain.ErrInvalid))
	}
	if err := s.passkeys.Remove(ctx, caller(ctx), id); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.RemovePasskeyResponse{}), nil
}
