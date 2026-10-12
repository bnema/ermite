// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/ports"
)

// errPasskeysUnavailable is what the passkey RPCs answer: this server has no
// passkeys, which the contract reports as failed_precondition. The passkey
// feature replaces these two methods.
var errPasskeysUnavailable = connect.NewError(connect.CodeFailedPrecondition, errors.New("passkeys are not available on this server"))

// sessionService implements ermiteconnect.SessionServiceHandler. The browser
// never calls it: sessions come from the link, and the API ignores cookies.
type sessionService struct{ sessions ports.SessionService }

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

func (s *sessionService) ListPasskeys(context.Context, *connect.Request[pb.ListPasskeysRequest]) (*connect.Response[pb.ListPasskeysResponse], error) {
	return nil, errPasskeysUnavailable
}

func (s *sessionService) RemovePasskey(context.Context, *connect.Request[pb.RemovePasskeyRequest]) (*connect.Response[pb.RemovePasskeyResponse], error) {
	return nil, errPasskeysUnavailable
}
