// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

func TestCallsCarryTokenAndUserAgent(t *testing.T) {
	f := &fake{version: func(context.Context, *connect.Request[ermite.GetVersionRequest]) (*connect.Response[ermite.GetVersionResponse], error) {
		return connect.NewResponse(&ermite.GetVersionResponse{Version: "v1"}), nil
	}}
	c := start(t, f, client.Config{Token: "ermite_secret", UserAgent: "ermite/v9 (cli)"})
	res, err := c.Server().GetVersion(t.Context(), connect.NewRequest(&ermite.GetVersionRequest{}))
	require.NoError(t, err)
	assert.Equal(t, "v1", res.Msg.GetVersion())
	require.Len(t, f.headers, 1)
	assert.Equal(t, "Bearer ermite_secret", f.headers[0].Get("Authorization"))
	assert.Equal(t, "ermite/v9 (cli)", f.headers[0].Get("User-Agent"))
}

func TestAnonymousCallsSendNoAuthorization(t *testing.T) {
	f := &fake{version: func(context.Context, *connect.Request[ermite.GetVersionRequest]) (*connect.Response[ermite.GetVersionResponse], error) {
		return connect.NewResponse(&ermite.GetVersionResponse{}), nil
	}}
	c := start(t, f, client.Config{})
	_, err := c.Server().GetVersion(t.Context(), connect.NewRequest(&ermite.GetVersionRequest{}))
	require.NoError(t, err)
	assert.Empty(t, f.headers[0].Values("Authorization"))
}

func TestUnaryCallsTimeOut(t *testing.T) {
	f := &fake{version: func(ctx context.Context, _ *connect.Request[ermite.GetVersionRequest]) (*connect.Response[ermite.GetVersionResponse], error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	c := start(t, f, client.Config{Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := c.Server().GetVersion(t.Context(), connect.NewRequest(&ermite.GetVersionRequest{}))
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, connect.CodeDeadlineExceeded, connect.CodeOf(err))
	assert.Contains(t, c.Describe(err).Error(), "timed out")
}

func TestRedirectIsNotFollowed(t *testing.T) {
	f := &fake{}
	c := start(t, f, client.Config{Token: "t"})
	_ = c // the redirect target below is a plain server
	redirect := redirectingServer(t)
	rc, err := client.New(client.Config{URL: redirect, Token: "t"})
	require.NoError(t, err)
	_, err = rc.Server().GetVersion(t.Context(), connect.NewRequest(&ermite.GetVersionRequest{}))
	require.Error(t, err)
	assert.Contains(t, rc.Describe(err).Error(), "redirected")
}

func TestNewRejectsBadURL(t *testing.T) {
	_, err := client.New(client.Config{URL: "ftp://x"})
	require.Error(t, err)
}

func TestDescribe(t *testing.T) {
	anon, err := client.New(client.Config{URL: "http://srv.test"})
	require.NoError(t, err)
	authed, err := client.New(client.Config{URL: "http://srv.test", Token: "t"})
	require.NoError(t, err)

	tests := []struct {
		name string
		c    *client.Client
		err  error
		want []string
	}{
		{"unauthenticated anonymous", anon, connect.NewError(connect.CodeUnauthenticated, errors.New("no token")),
			[]string{"authentication required", "ERMITE_TOKEN"}},
		{"unauthenticated bad token", authed, connect.NewError(connect.CodeUnauthenticated, errors.New("bad")),
			[]string{"not authenticated", "invalid, expired or revoked"}},
		{"not found", authed, connect.NewError(connect.CodeNotFound, errors.New("repository \"x\" not found")),
			[]string{"not found: repository \"x\" not found"}},
		{"permission denied", authed, connect.NewError(connect.CodePermissionDenied, errors.New("missing scope repo:admin")),
			[]string{"permission denied: missing scope repo:admin", "token list"}},
		{"already exists", authed, connect.NewError(connect.CodeAlreadyExists, errors.New("name taken")),
			[]string{"already exists: name taken"}},
		{"invalid", authed, connect.NewWireError(connect.CodeInvalidArgument, errors.New("bad name")),
			[]string{"invalid request: bad name"}},
		{"invalid raised by the client library", authed, connect.NewError(connect.CodeInvalidArgument, errors.New("protocol error: incomplete envelope")),
			[]string{"connection to http://srv.test broke", "incomplete envelope"}},
		{"precondition", authed, connect.NewError(connect.CodeFailedPrecondition, errors.New("no such branch")),
			[]string{"precondition failed: no such branch"}},
		{"unavailable", authed, connect.NewError(connect.CodeUnavailable, errors.New("connection refused")),
			[]string{"cannot reach the server at http://srv.test", "connection refused"}},
		{"unimplemented", authed, connect.NewError(connect.CodeUnimplemented, errors.New("x")),
			[]string{"does not implement", "ermite version"}},
		{"multi-line message", authed, connect.NewError(connect.CodeInternal, errors.New("line1\nline2\n\tline3")),
			[]string{"server error: line1 line2 line3"}},
		{"empty message", authed, connect.NewError(connect.CodeNotFound, errors.New("")),
			[]string{"not found"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.c.Describe(tt.err)
			require.Error(t, got)
			for _, w := range tt.want {
				assert.Contains(t, got.Error(), w)
			}
			assert.NotContains(t, got.Error(), "\n")
			assert.NotContains(t, got.Error(), "[", "no connect code prefix")
			assert.ErrorIs(t, got, tt.err, "the original error stays reachable")
		})
	}

	plain := errors.New("boom")
	assert.Same(t, plain, anon.Describe(plain))
	assert.NoError(t, anon.Describe(nil))
}

func TestDescribeBoundsLongMessages(t *testing.T) {
	c, err := client.New(client.Config{URL: "http://srv.test"})
	require.NoError(t, err)
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'a'
	}
	got := c.Describe(connect.NewError(connect.CodeInternal, errors.New(string(long))))
	assert.Less(t, len(got.Error()), 400)
}

func TestMissingFeatures(t *testing.T) {
	assert.Empty(t, client.MissingFeatures(client.UsedFeatures()))
	assert.Equal(t,
		[]string{client.FeatureEventsResume, client.FeatureMergeRequests, client.FeatureMergeDeleteBranch, client.FeatureSigningKey, client.FeatureTokenBranchPrefixes, client.FeatureCIRuns, client.FeatureCIRunners},
		client.MissingFeatures([]string{client.FeatureRepoUpdate, client.FeatureSession, "other"}))
	assert.Equal(t, []string{client.FeatureSigningKey}, client.MissingFeatures([]string{
		client.FeatureRepoUpdate, client.FeatureEventsResume, client.FeatureMergeRequests, client.FeatureMergeDeleteBranch, client.FeatureSession,
		client.FeatureTokenBranchPrefixes, client.FeatureCIRuns, client.FeatureCIRunners,
	}))
	assert.Equal(t, client.UsedFeatures(), client.MissingFeatures(nil))
}

func TestDescribeStripsTerminalControls(t *testing.T) {
	c, err := client.New(client.Config{URL: "http://srv.test", Token: "t"})
	require.NoError(t, err)
	msg := "bad\x1b]0;pwn\x07 name \u202egpj.exe \u2066x\u2069"
	got := c.Describe(connect.NewError(connect.CodeInvalidArgument, errors.New(msg))).Error()
	assert.NotContains(t, got, "\x1b")
	assert.NotContains(t, got, "\x07")
	assert.NotContains(t, got, "\u202e")
	assert.NotContains(t, got, "\u2066")
	assert.NotContains(t, got, "\u2069")
	assert.Contains(t, got, "bad?]0;pwn? name ?gpj.exe ?x?")
}
