// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/rpc/mocks"
	"bnema.dev/ermite/internal/domain"
)

func TestServerServiceGetVersion(t *testing.T) {
	t.Parallel()
	svc := &serverService{version: "v1.2.3", key: mocks.NewSigningKey(t)}
	resp, err := svc.GetVersion(context.Background(), connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err)
	assert.Equal(t, "v1.2.3", resp.Msg.GetVersion())
	assert.Equal(t, Features, resp.Msg.GetFeatures())

	// The caller gets a copy: changing it does not change what is advertised.
	resp.Msg.Features[0] = "tampered"
	assert.NotEqual(t, "tampered", Features[0])
}

func TestServerServiceGetSigningKey(t *testing.T) {
	t.Parallel()
	key := mocks.NewSigningKey(t)
	key.EXPECT().Info().Return(domain.SigningKeyInfo{
		Key:       domain.SigningPublicKey{AuthorizedKey: "ssh-ed25519 AAAA", Fingerprint: "SHA256:xyz"},
		Committer: domain.Identity{Name: "ermite", Email: "ermite@example.com"},
	}).Once()
	resp, err := (&serverService{key: key}).GetSigningKey(context.Background(), connect.NewRequest(&pb.GetSigningKeyRequest{}))
	require.NoError(t, err)
	assert.Equal(t, "ssh-ed25519 AAAA", resp.Msg.GetPublicKey())
	assert.Equal(t, "SHA256:xyz", resp.Msg.GetFingerprint())
	assert.Equal(t, "ermite", resp.Msg.GetCommitterName())
	assert.Equal(t, "ermite@example.com", resp.Msg.GetCommitterEmail())
	assert.Equal(t, "ermite@example.com ssh-ed25519 AAAA", resp.Msg.GetAllowedSigners())
}
