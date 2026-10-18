// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

type statusCase struct {
	domain domain.SignatureStatus
	pb     pb.SignatureStatus
}

type formatCase struct {
	domain domain.SignatureFormat
	pb     pb.SignatureFormat
}

var (
	statusCases = []statusCase{
		{domain.StatusUnsigned, pb.SignatureStatus_SIGNATURE_STATUS_UNSIGNED},
		{domain.StatusVerified, pb.SignatureStatus_SIGNATURE_STATUS_VERIFIED},
		{domain.StatusUnknownKey, pb.SignatureStatus_SIGNATURE_STATUS_UNKNOWN_KEY},
		{domain.StatusBadSignature, pb.SignatureStatus_SIGNATURE_STATUS_BAD_SIGNATURE},
		{domain.StatusEmailMismatch, pb.SignatureStatus_SIGNATURE_STATUS_EMAIL_MISMATCH},
		{domain.StatusExpired, pb.SignatureStatus_SIGNATURE_STATUS_EXPIRED},
		{domain.StatusRevoked, pb.SignatureStatus_SIGNATURE_STATUS_REVOKED},
		{domain.StatusUnsupported, pb.SignatureStatus_SIGNATURE_STATUS_UNSUPPORTED},
	}
	formatCases = []formatCase{
		{domain.FormatSSH, pb.SignatureFormat_SIGNATURE_FORMAT_SSH},
		{domain.FormatGPG, pb.SignatureFormat_SIGNATURE_FORMAT_GPG},
		{domain.FormatX509, pb.SignatureFormat_SIGNATURE_FORMAT_X509},
	}
)

func TestSignatureEnumMappers(t *testing.T) {
	t.Parallel()
	for _, c := range statusCases {
		assert.Equal(t, c.pb, signatureStatusToPB(c.domain), c.domain)
	}
	assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_UNSPECIFIED, signatureStatusToPB(domain.StatusUnchecked))
	for _, c := range formatCases {
		assert.Equal(t, c.pb, signatureFormatToPB(c.domain), c.domain)
	}
	assert.Equal(t, pb.SignatureFormat_SIGNATURE_FORMAT_UNSPECIFIED, signatureFormatToPB(domain.FormatNone))

	// Nothing in the contract is unreachable.
	for v, n := range pb.SignatureStatus_name {
		if v != 0 {
			assert.True(t, slices.ContainsFunc(statusCases, func(c statusCase) bool {
				return c.pb == pb.SignatureStatus(v)
			}), "%s is never produced", n)
		}
	}
	for v, n := range pb.SignatureFormat_name {
		if v != 0 {
			assert.True(t, slices.ContainsFunc(formatCases, func(c formatCase) bool {
				return c.pb == pb.SignatureFormat(v)
			}), "%s is never produced", n)
		}
	}
}

func TestSignatureFormatFromPB(t *testing.T) {
	t.Parallel()
	f, err := signatureFormatFromPB(pb.SignatureFormat_SIGNATURE_FORMAT_SSH)
	require.NoError(t, err)
	assert.Equal(t, domain.FormatSSH, f)
	f, err = signatureFormatFromPB(pb.SignatureFormat_SIGNATURE_FORMAT_GPG)
	require.NoError(t, err)
	assert.Equal(t, domain.FormatGPG, f)
	for _, bad := range []pb.SignatureFormat{pb.SignatureFormat_SIGNATURE_FORMAT_UNSPECIFIED, pb.SignatureFormat_SIGNATURE_FORMAT_X509, pb.SignatureFormat(99)} {
		_, err = signatureFormatFromPB(bad)
		assert.ErrorIs(t, err, domain.ErrInvalid, bad)
	}
}

func TestCommitToPBVerification(t *testing.T) {
	t.Parallel()
	t.Run("unchecked sets nothing", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, commitToPB(domain.Commit{}).GetVerification())
	})
	t.Run("unsigned", func(t *testing.T) {
		t.Parallel()
		v := commitToPB(domain.Commit{Verification: domain.CommitVerification{Status: domain.StatusUnsigned}}).GetVerification()
		require.NotNil(t, v)
		assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_UNSIGNED, v.GetStatus())
		assert.Equal(t, pb.SignatureFormat_SIGNATURE_FORMAT_UNSPECIFIED, v.GetFormat())
	})
	t.Run("verified", func(t *testing.T) {
		t.Parallel()
		v := commitToPB(domain.Commit{Verification: domain.CommitVerification{
			Format: domain.FormatSSH, Status: domain.StatusVerified, Fingerprint: "SHA256:abc", KeyID: "SHA256:abc", KeyTitle: "laptop",
		}}).GetVerification()
		require.NotNil(t, v)
		assert.Equal(t, pb.SignatureFormat_SIGNATURE_FORMAT_SSH, v.GetFormat())
		assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_VERIFIED, v.GetStatus())
		assert.Equal(t, "SHA256:abc", v.GetFingerprint())
		assert.Equal(t, "SHA256:abc", v.GetKeyId())
		assert.Equal(t, "laptop", v.GetKeyTitle())
	})
	t.Run("every status", func(t *testing.T) {
		t.Parallel()
		for _, c := range statusCases {
			v := commitToPB(domain.Commit{Verification: domain.CommitVerification{Format: domain.FormatGPG, Status: c.domain}}).GetVerification()
			require.NotNil(t, v, c.domain)
			assert.Equal(t, c.pb, v.GetStatus())
		}
	})
}

func TestTrustedKeyServiceList(t *testing.T) {
	t.Parallel()
	created := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	keys := []domain.TrustedKey{
		{ID: "SHA256:builtin", Format: domain.FormatSSH, Title: "instance", PublicKey: "ssh-ed25519 AAAA", Emails: []string{"ermite@example.com"}, Builtin: true},
		{ID: "ABCD", Format: domain.FormatGPG, Title: "me", PublicKey: "-----BEGIN PGP PUBLIC KEY BLOCK-----", Emails: []string{"a@b.c", "d@e.f"}, CreatedAt: created},
	}
	tests := []struct {
		name     string
		list     []domain.TrustedKey
		usecase  error
		wantCode connect.Code
	}{
		{name: "ok", list: keys},
		{name: "empty"},
		{name: "anonymous", usecase: domain.ErrUnauthenticated, wantCode: connect.CodeUnauthenticated},
		{name: "unknown error", usecase: errors.New("disk on fire"), wantCode: connect.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := mocks.NewTrustedKeyService(t)
			svc.EXPECT().ListTrustedKeys(mock.Anything, domain.Owner()).Return(tt.list, tt.usecase).Once()
			resp, err := (&trustedKeyService{svc}).ListTrustedKeys(ownerCtx(), connect.NewRequest(&pb.ListTrustedKeysRequest{}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, resp.Msg.GetKeys(), len(tt.list))
			for i, want := range tt.list {
				got := resp.Msg.GetKeys()[i]
				assert.Equal(t, want.ID, got.GetId())
				assert.Equal(t, signatureFormatToPB(want.Format), got.GetFormat())
				assert.Equal(t, want.Title, got.GetTitle())
				assert.Equal(t, want.PublicKey, got.GetPublicKey())
				assert.Equal(t, want.Emails, got.GetEmails())
				assert.Equal(t, want.Builtin, got.GetBuiltin())
				assert.Equal(t, want.CreatedAt.IsZero(), got.GetCreatedAt() == nil)
				if !want.CreatedAt.IsZero() {
					assert.True(t, created.Equal(got.GetCreatedAt().AsTime()))
				}
			}
		})
	}
}

func TestTrustedKeyServiceAdd(t *testing.T) {
	t.Parallel()
	stored := domain.TrustedKey{ID: "SHA256:k", Format: domain.FormatSSH, Title: "laptop", PublicKey: "ssh-ed25519 AAAA", Emails: []string{"a@b.c"}}
	tests := []struct {
		name     string
		format   pb.SignatureFormat
		usecase  error
		callsUC  bool
		wantCode connect.Code
	}{
		{name: "ssh", format: pb.SignatureFormat_SIGNATURE_FORMAT_SSH, callsUC: true},
		{name: "unspecified format", format: pb.SignatureFormat_SIGNATURE_FORMAT_UNSPECIFIED, wantCode: connect.CodeInvalidArgument},
		{name: "x509 format", format: pb.SignatureFormat_SIGNATURE_FORMAT_X509, wantCode: connect.CodeInvalidArgument},
		{name: "not the owner", format: pb.SignatureFormat_SIGNATURE_FORMAT_SSH, callsUC: true, usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied},
		{name: "invalid key", format: pb.SignatureFormat_SIGNATURE_FORMAT_SSH, callsUC: true, usecase: domain.ErrInvalid, wantCode: connect.CodeInvalidArgument},
		{name: "already listed", format: pb.SignatureFormat_SIGNATURE_FORMAT_SSH, callsUC: true, usecase: domain.ErrAlreadyExists, wantCode: connect.CodeAlreadyExists},
		{name: "too many", format: pb.SignatureFormat_SIGNATURE_FORMAT_SSH, callsUC: true, usecase: domain.ErrResourceExhausted, wantCode: connect.CodeResourceExhausted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := mocks.NewTrustedKeyService(t)
			if tt.callsUC {
				ret := stored
				if tt.usecase != nil {
					ret = domain.TrustedKey{}
				}
				svc.EXPECT().AddTrustedKey(mock.Anything, domain.Owner(), domain.FormatSSH, "laptop", "ssh-ed25519 AAAA", []string{"a@b.c"}).Return(ret, tt.usecase).Once()
			}
			resp, err := (&trustedKeyService{svc}).AddTrustedKey(ownerCtx(), connect.NewRequest(&pb.AddTrustedKeyRequest{
				Format: tt.format, Title: "laptop", PublicKey: "ssh-ed25519 AAAA", Emails: []string{"a@b.c"},
			}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "SHA256:k", resp.Msg.GetKey().GetId())
			assert.Equal(t, pb.SignatureFormat_SIGNATURE_FORMAT_SSH, resp.Msg.GetKey().GetFormat())
		})
	}
}

func TestTrustedKeyServiceRemove(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		usecase  error
		wantCode connect.Code
	}{
		{name: "ok"},
		{name: "not the owner", usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied},
		{name: "unknown id", usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound},
		{name: "builtin", usecase: domain.ErrPreconditionFailed, wantCode: connect.CodeFailedPrecondition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := mocks.NewTrustedKeyService(t)
			svc.EXPECT().RemoveTrustedKey(mock.Anything, domain.Owner(), "SHA256:k").Return(tt.usecase).Once()
			_, err := (&trustedKeyService{svc}).RemoveTrustedKey(ownerCtx(), connect.NewRequest(&pb.RemoveTrustedKeyRequest{Id: "SHA256:k"}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// GetVersion lists "commit_signatures", and the service is mounted, only when
// the API is built with trusted keys.
func TestAPICommitSignaturesFeature(t *testing.T) {
	t.Parallel()
	serve := func(cfg Config) (features []string, keys ermiteconnect.TrustedKeyServiceClient) {
		cfg.SigningKey = mocks.NewSigningKeyService(t)
		srv := httptest.NewServer(New(cfg).Handler())
		t.Cleanup(srv.Close)
		c := ermiteconnect.NewServerServiceClient(srv.Client(), srv.URL)
		resp, err := c.GetVersion(context.Background(), connect.NewRequest(&pb.GetVersionRequest{}))
		require.NoError(t, err)
		return resp.Msg.GetFeatures(), ermiteconnect.NewTrustedKeyServiceClient(srv.Client(), srv.URL)
	}

	features, keys := serve(Config{})
	assert.NotContains(t, features, "commit_signatures")
	assert.NotContains(t, Features, "commit_signatures", "the shared list is not modified")
	_, err := keys.ListTrustedKeys(context.Background(), connect.NewRequest(&pb.ListTrustedKeysRequest{}))
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))

	svc := mocks.NewTrustedKeyService(t)
	features, keys = serve(Config{TrustedKeys: svc, Auth: mocks.NewAuthService(t)})
	assert.Contains(t, features, "commit_signatures")
	// An anonymous caller reaches the use case, which refuses it.
	svc.EXPECT().ListTrustedKeys(mock.Anything, domain.Anonymous()).Return(nil, domain.ErrUnauthenticated).Once()
	_, err = keys.ListTrustedKeys(context.Background(), connect.NewRequest(&pb.ListTrustedKeysRequest{}))
	assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
}
