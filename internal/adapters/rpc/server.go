// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"slices"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// serverService implements ermiteconnect.ServerServiceHandler.
type serverService struct{ version string }

func (s *serverService) GetVersion(context.Context, *connect.Request[pb.GetVersionRequest]) (*connect.Response[pb.GetVersionResponse], error) {
	return connect.NewResponse(&pb.GetVersionResponse{Version: s.version, Features: slices.Clone(Features)}), nil
}
