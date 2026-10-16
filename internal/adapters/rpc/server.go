// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"slices"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// serverService implements ermiteconnect.ServerServiceHandler.
type serverService struct {
	version  string
	features []string
	key      ports.SigningKeyService
	// oidc rotates the key of the ID tokens; nil when the server issues none.
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
// owner may; failed_precondition when the server issues no ID tokens.
func (s *serverService) RotateOIDCKey(ctx context.Context, _ *connect.Request[pb.RotateOIDCKeyRequest]) (*connect.Response[pb.RotateOIDCKeyResponse], error) {
	p := caller(ctx)
	if s.oidc == nil {
		if !p.HasOwnerRights() {
			return nil, connectError(ctx, domain.ErrForbidden)
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the server issues no OIDC ID tokens: it has no ERMITE_SECRETS_KEY"))
	}
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
