// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/app"
)

// sessionService implements ermiteconnect.SessionServiceHandler. The browser
// never calls it: sessions come from the link, and the API ignores cookies.
type sessionService struct{ sessions *app.Sessions }

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
