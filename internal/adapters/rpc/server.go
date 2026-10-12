// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"slices"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/ports"
)

// serverService implements ermiteconnect.ServerServiceHandler.
type serverService struct {
	version  string
	features []string
	key      ports.SigningKeyService
}

func (s *serverService) GetVersion(context.Context, *connect.Request[pb.GetVersionRequest]) (*connect.Response[pb.GetVersionResponse], error) {
	return connect.NewResponse(&pb.GetVersionResponse{Version: s.version, Features: slices.Clone(s.features)}), nil
}

// GetSigningKey returns the public key of the commits ermite signs. It needs
// no authentication (see the interceptor).
func (s *serverService) GetSigningKey(context.Context, *connect.Request[pb.GetSigningKeyRequest]) (*connect.Response[pb.GetSigningKeyResponse], error) {
	info := s.key.Info()
	return connect.NewResponse(&pb.GetSigningKeyResponse{
		PublicKey:      info.Key.AuthorizedKey,
		Fingerprint:    info.Key.Fingerprint,
		CommitterName:  info.Committer.Name,
		CommitterEmail: info.Committer.Email,
		AllowedSigners: info.AllowedSigners(),
	}), nil
}
