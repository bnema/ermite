// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"connectrpc.com/connect/v2/connecthttp"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/adapters/client"
)

type releaseAPI struct {
	ermiteconnect.UnimplementedReleaseServiceHandler
}

func (releaseAPI) GetRelease(ctx context.Context, r *ermite.GetReleaseRequest) (*ermite.GetReleaseResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	if info.RequestHeader().Get("Authorization") != "Bearer secret" || info.RequestHeader().Get("User-Agent") != "ermite/test" {
		return nil, connect.NewError(connect.CodeUnauthenticated, "")
	}
	return &ermite.GetReleaseResponse{Release: &ermite.Release{Id: r.GetId(), Repo: r.GetRepo()}}, nil
}
func TestReleasesAccessor(t *testing.T) {
	mux := http.NewServeMux()
	{
		server := connect.NewServer()
		ermiteconnect.RegisterReleaseServiceHandler(server, releaseAPI{})
		sub := http.NewServeMux()
		connecthttp.Mount(sub, server, connecthttp.WithReadMaxBytes(0))
		mux.Handle("/-/api/ermite.ReleaseService/", http.StripPrefix("/-/api", sub))
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := client.New(client.Config{URL: srv.URL, Token: "secret", UserAgent: "ermite/test"})
	require.NoError(t, err)
	res, err := c.Releases().GetRelease(t.Context(), &ermite.GetReleaseRequest{Repo: "proj", Selector: &ermite.GetReleaseRequest_Id{Id: 7}})
	require.NoError(t, err)
	assert.Equal(t, int64(7), res.GetRelease().GetId())
	assert.Equal(t, "proj", res.GetRelease().GetRepo())
	assert.Contains(t, client.UsedFeatures(), client.FeatureReleases)
}
