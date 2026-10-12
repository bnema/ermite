// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"slices"
	"testing"

	"net/http/httptest"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestServerServiceGetVersion(t *testing.T) {
	t.Parallel()
	svc := &serverService{version: "v1.2.3", features: slices.Clone(Features), key: mocks.NewSigningKeyService(t)}
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
	key := mocks.NewSigningKeyService(t)
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

// GetVersion lists "passkeys" only when the API is built with passkeys.
func TestAPIListsPasskeysFeatureWhenAvailable(t *testing.T) {
	t.Parallel()
	list := func(cfg Config) []string {
		cfg.Sessions = mocks.NewSessionService(t)
		cfg.SigningKey = mocks.NewSigningKeyService(t)
		srv := httptest.NewServer(New(cfg).Handler())
		t.Cleanup(srv.Close)
		c := ermiteconnect.NewServerServiceClient(srv.Client(), srv.URL)
		resp, err := c.GetVersion(context.Background(), connect.NewRequest(&pb.GetVersionRequest{}))
		require.NoError(t, err)
		return resp.Msg.GetFeatures()
	}
	assert.Equal(t, Features, list(Config{}))
	assert.NotContains(t, list(Config{}), "passkeys")
	got := list(Config{Passkeys: mocks.NewPasskeyService(t)})
	assert.Equal(t, append(slices.Clone(Features), "passkeys"), got)
	assert.NotContains(t, Features, "passkeys", "the shared list is not modified")
}
