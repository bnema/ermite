// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/ports"
)

// FeatureCommitSignatures is listed by GetVersion when the server verifies
// commit signatures: Commit.verification and TrustedKeyService (ADR 0030).
const FeatureCommitSignatures = "commit_signatures"

// trustedKeyService implements ermiteconnect.TrustedKeyServiceHandler. The
// owner check of add and remove belongs to the use case.
type trustedKeyService struct {
	keys ports.TrustedKeyService
}

func (s *trustedKeyService) ListTrustedKeys(ctx context.Context, _ *pb.ListTrustedKeysRequest) (*pb.ListTrustedKeysResponse, error) {
	list, err := s.keys.ListTrustedKeys(ctx, caller(ctx))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := make([]*pb.TrustedKey, 0, len(list))
	for _, k := range list {
		out = append(out, trustedKeyToPB(k))
	}
	return &pb.ListTrustedKeysResponse{Keys: out}, nil
}

func (s *trustedKeyService) AddTrustedKey(ctx context.Context, req *pb.AddTrustedKeyRequest) (*pb.AddTrustedKeyResponse, error) {
	format, err := signatureFormatFromPB(req.GetFormat())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	k, err := s.keys.AddTrustedKey(ctx, caller(ctx), format, req.GetTitle(), req.GetPublicKey(), req.GetEmails())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.AddTrustedKeyResponse{Key: trustedKeyToPB(k)}, nil
}

func (s *trustedKeyService) RemoveTrustedKey(ctx context.Context, req *pb.RemoveTrustedKeyRequest) (*pb.RemoveTrustedKeyResponse, error) {
	if err := s.keys.RemoveTrustedKey(ctx, caller(ctx), req.GetId()); err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.RemoveTrustedKeyResponse{}, nil
}
