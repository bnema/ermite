// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"slices"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// serverService implements ermiteconnect.ServerServiceHandler.
type serverService struct{ version string }

func (s *serverService) GetVersion(context.Context, *connect.Request[pb.GetVersionRequest]) (*connect.Response[pb.GetVersionResponse], error) {
	return connect.NewResponse(&pb.GetVersionResponse{Version: s.version, Features: slices.Clone(Features)}), nil
}

// GetSigningKey is not served yet: the server does not list "signing_key".
func (s *serverService) GetSigningKey(context.Context, *connect.Request[pb.GetSigningKeyRequest]) (*connect.Response[pb.GetSigningKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("no signing key"))
}
