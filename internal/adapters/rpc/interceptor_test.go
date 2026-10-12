// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

const peer = "203.0.113.7:4242"

func bearerHeader(v string) http.Header {
	h := http.Header{}
	if v != "" {
		h.Set("Authorization", v)
	}
	return h
}

func TestInterceptorAuthenticate(t *testing.T) {
	t.Parallel()
	const procedure = ermiteconnect.RepoServiceListReposProcedure
	tests := []struct {
		name      string
		procedure string
		header    http.Header
		blocked   bool
		authErr   error
		wantCode  connect.Code // 0: success
		wantAuth  bool
		wantFail  bool
		wantLimit bool // the limiter is asked at all
		wantRetry string
		wantOwner bool
	}{
		{name: "no header is anonymous", procedure: procedure, header: bearerHeader(""), wantLimit: true},
		{name: "valid token", procedure: procedure, header: bearerHeader("Bearer good"), wantLimit: true, wantAuth: true, wantOwner: true},
		{
			name: "invalid token counts as a failure", procedure: procedure, header: bearerHeader("Bearer bad"),
			authErr: domain.ErrUnauthenticated, wantCode: connect.CodeUnauthenticated, wantLimit: true, wantAuth: true, wantFail: true,
		},
		{
			name: "non-bearer scheme is a failure", procedure: procedure, header: bearerHeader("Basic abc"),
			authErr: domain.ErrUnauthenticated, wantCode: connect.CodeUnauthenticated, wantLimit: true, wantAuth: true, wantFail: true,
		},
		{
			name: "authenticator outage is not a failure of the client", procedure: procedure, header: bearerHeader("Bearer good"),
			authErr: errors.New("store down"), wantCode: connect.CodeInternal, wantLimit: true, wantAuth: true,
		},
		{
			name: "blocked client is refused with Retry-After", procedure: procedure, header: bearerHeader("Bearer good"),
			blocked: true, wantCode: connect.CodeResourceExhausted, wantLimit: true, wantRetry: "30",
		},
		{name: "version needs no authentication", procedure: ermiteconnect.ServerServiceGetVersionProcedure, header: bearerHeader("Bearer expired")},
		{name: "signing key needs no authentication", procedure: ermiteconnect.ServerServiceGetSigningKeyProcedure, header: bearerHeader("Bearer expired")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			auth := mocks.NewAuthService(t)
			limiter := mocks.NewFailureLimiter(t)
			if tt.wantLimit {
				wait := time.Duration(0)
				if tt.blocked {
					wait = 29*time.Second + 500*time.Millisecond
				}
				limiter.EXPECT().Blocked(mock.Anything, "203.0.113.7").Return(tt.blocked, wait).Once()
			}
			if tt.wantAuth {
				p := domain.Principal{}
				if tt.wantOwner {
					p = domain.Owner()
				}
				auth.EXPECT().Authenticate(mock.Anything, mock.Anything).Return(p, tt.authErr).Once()
			}
			if tt.wantFail {
				limiter.EXPECT().Fail(mock.Anything, "203.0.113.7").Once()
			}
			i := newInterceptor(Config{Auth: auth, Limiter: limiter})
			ctx, err := i.authenticate(context.Background(), tt.procedure, peer, tt.header)
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				if tt.wantRetry != "" {
					var ce *connect.Error
					require.ErrorAs(t, err, &ce)
					assert.Equal(t, tt.wantRetry, ce.Meta().Get("Retry-After"))
				}
				return
			}
			require.NoError(t, err)
			p, ok := domain.PrincipalFromContext(ctx)
			require.True(t, ok)
			if tt.wantOwner {
				assert.Equal(t, domain.Owner(), p)
			} else {
				assert.Equal(t, domain.Anonymous(), p)
			}
		})
	}
}

func TestInterceptorIgnoresForwardedForFromUntrustedPeer(t *testing.T) {
	t.Parallel()
	limiter := mocks.NewFailureLimiter(t)
	limiter.EXPECT().Blocked(mock.Anything, "203.0.113.7").Return(false, time.Duration(0)).Once()
	h := bearerHeader("")
	h.Set("X-Forwarded-For", "198.51.100.1")
	i := newInterceptor(Config{Auth: mocks.NewAuthService(t), Limiter: limiter})
	_, err := i.authenticate(context.Background(), ermiteconnect.RepoServiceListReposProcedure, peer, h)
	require.NoError(t, err)
}
