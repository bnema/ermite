// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
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
	// oidc manages key rotation, including the disabled-provider response.
	oidc ports.OIDCKeyService
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

// RotateOIDCKey replaces the key that signs the ID tokens of CI jobs. Only the
// owner may; failed_precondition when the server issues no ID tokens. An API
// built without an OIDC key service answers unimplemented.
func (s *serverService) RotateOIDCKey(ctx context.Context, _ *connect.Request[pb.RotateOIDCKeyRequest]) (*connect.Response[pb.RotateOIDCKeyResponse], error) {
	if s.oidc == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this API serves no OIDC key service"))
	}
	p := caller(ctx)
	r, err := s.oidc.RotateKey(ctx, p)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.RotateOIDCKeyResponse{Issuer: r.Issuer, Kid: r.Kid, RetiredKid: r.RetiredKid}
	if r.RetiredKid != "" {
		out.RetiredUntil = timestamp(r.RetiredUntil)
	}
	return connect.NewResponse(out), nil
}
