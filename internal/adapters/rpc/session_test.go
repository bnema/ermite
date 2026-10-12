// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"encoding/base64"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestSessionServiceCreateLoginLink(t *testing.T) {
	t.Parallel()
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name     string
		usecase  error
		wantCode connect.Code
	}{
		{name: "ok"},
		{name: "not the owner", usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied},
		{name: "anonymous", usecase: domain.ErrUnauthenticated, wantCode: connect.CodeUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessions := mocks.NewSessionService(t)
			sessions.EXPECT().CreateLoginLink(mock.Anything, domain.Owner()).
				Return(ports.LoginLink{URL: "https://x/-/login/n", ExpiresAt: exp}, tt.usecase).Once()
			resp, err := (&sessionService{sessions: sessions}).CreateLoginLink(ownerCtx(), connect.NewRequest(&pb.CreateLoginLinkRequest{}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "https://x/-/login/n", resp.Msg.GetUrl())
			assert.True(t, exp.Equal(resp.Msg.GetExpiresAt().AsTime()))
		})
	}
}

func TestSessionServiceSignOutEverywhere(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		usecase  error
		wantCode connect.Code
	}{
		{name: "ok"},
		{name: "forbidden", usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessions := mocks.NewSessionService(t)
			sessions.EXPECT().SignOutEverywhere(mock.Anything, domain.Owner()).Return(tt.usecase).Once()
			_, err := (&sessionService{sessions: sessions}).SignOutEverywhere(ownerCtx(), connect.NewRequest(&pb.SignOutEverywhereRequest{}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestSessionServiceListPasskeys(t *testing.T) {
	t.Parallel()
	created := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	used := created.Add(time.Hour)
	tests := []struct {
		name     string
		list     []domain.Passkey
		usecase  error
		wantCode connect.Code
	}{
		{name: "none", list: nil},
		{name: "two", list: []domain.Passkey{
			{ID: []byte{0xfb, 0xff, 0x01}, Name: "phone", PublicKey: []byte("secret"), CreatedAt: created, LastUsedAt: used, BackupState: true},
			{ID: []byte("hw"), Name: "key", CreatedAt: created},
		}},
		{name: "not the owner", usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied},
		{name: "anonymous", usecase: domain.ErrUnauthenticated, wantCode: connect.CodeUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			passkeys := mocks.NewPasskeyService(t)
			passkeys.EXPECT().List(mock.Anything, domain.Owner()).Return(tt.list, tt.usecase).Once()
			resp, err := (&sessionService{passkeys: passkeys}).ListPasskeys(ownerCtx(), connect.NewRequest(&pb.ListPasskeysRequest{}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, resp.Msg.GetPasskeys(), len(tt.list))
			for i, want := range tt.list {
				got := resp.Msg.GetPasskeys()[i]
				assert.Equal(t, base64.RawURLEncoding.EncodeToString(want.ID), got.GetId())
				assert.Equal(t, want.Name, got.GetName())
				assert.Equal(t, want.BackupState, got.GetSynced())
				assert.True(t, want.CreatedAt.Equal(got.GetCreatedAt().AsTime()))
				if want.LastUsedAt.IsZero() {
					assert.Nil(t, got.GetLastUsedAt(), "never used")
				} else {
					assert.True(t, want.LastUsedAt.Equal(got.GetLastUsedAt().AsTime()))
				}
			}
		})
	}
}

func TestSessionServiceRemovePasskey(t *testing.T) {
	t.Parallel()
	id := []byte{0xfb, 0xff, 0x01, 'x'}
	enc := base64.RawURLEncoding.EncodeToString(id)
	tests := []struct {
		name     string
		id       string
		usecase  error
		called   bool
		wantCode connect.Code
	}{
		{name: "ok", id: enc, called: true},
		{name: "unknown id", id: enc, called: true, usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound},
		{name: "not the owner", id: enc, called: true, usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied},
		{name: "anonymous", id: enc, called: true, usecase: domain.ErrUnauthenticated, wantCode: connect.CodeUnauthenticated},
		{name: "not base64url", id: "not base64!", wantCode: connect.CodeInvalidArgument},
		{name: "padded base64", id: base64.URLEncoding.EncodeToString(id), wantCode: connect.CodeInvalidArgument},
		{name: "standard alphabet", id: "+/8B", wantCode: connect.CodeInvalidArgument},
		{name: "empty", id: "", wantCode: connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			passkeys := mocks.NewPasskeyService(t)
			if tt.called {
				passkeys.EXPECT().Remove(mock.Anything, domain.Owner(), id).Return(tt.usecase).Once()
			}
			_, err := (&sessionService{passkeys: passkeys}).RemovePasskey(ownerCtx(), connect.NewRequest(&pb.RemovePasskeyRequest{Id: tt.id}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// Without passkeys the RPCs answer failed_precondition and call nothing.
func TestSessionServicePasskeysUnavailable(t *testing.T) {
	t.Parallel()
	svc := &sessionService{sessions: mocks.NewSessionService(t)}
	_, err := svc.ListPasskeys(ownerCtx(), connect.NewRequest(&pb.ListPasskeysRequest{}))
	requireCode(t, connect.CodeFailedPrecondition, err)
	assert.Contains(t, err.Error(), "passkeys are not available on this server")
	_, err = svc.RemovePasskey(ownerCtx(), connect.NewRequest(&pb.RemovePasskeyRequest{Id: "AQ"}))
	requireCode(t, connect.CodeFailedPrecondition, err)
}
