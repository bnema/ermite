// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/rpc/mocks"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
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
			sessions := mocks.NewSessions(t)
			sessions.EXPECT().CreateLoginLink(mock.Anything, domain.Owner()).
				Return(app.LoginLink{URL: "https://x/-/login/n", ExpiresAt: exp}, tt.usecase).Once()
			resp, err := (&sessionService{sessions}).CreateLoginLink(ownerCtx(), connect.NewRequest(&pb.CreateLoginLinkRequest{}))
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
			sessions := mocks.NewSessions(t)
			sessions.EXPECT().SignOutEverywhere(mock.Anything, domain.Owner()).Return(tt.usecase).Once()
			_, err := (&sessionService{sessions}).SignOutEverywhere(ownerCtx(), connect.NewRequest(&pb.SignOutEverywhereRequest{}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
