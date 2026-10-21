// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/adapters/client"
)

type releaseAPI struct {
	ermiteconnect.UnimplementedReleaseServiceHandler
}

func (releaseAPI) GetRelease(_ context.Context, r *connect.Request[ermite.GetReleaseRequest]) (*connect.Response[ermite.GetReleaseResponse], error) {
	if r.Header().Get("Authorization") != "Bearer secret" || r.Header().Get("User-Agent") != "ermite/test" {
		return nil, connect.NewError(connect.CodeUnauthenticated, nil)
	}
	return connect.NewResponse(&ermite.GetReleaseResponse{Release: &ermite.Release{Id: r.Msg.GetId(), Repo: r.Msg.GetRepo()}}), nil
}
func TestReleasesAccessor(t *testing.T) {
	mux := http.NewServeMux()
	p, h := ermiteconnect.NewReleaseServiceHandler(releaseAPI{})
	mux.Handle("/-/api"+p, http.StripPrefix("/-/api", h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := client.New(client.Config{URL: srv.URL, Token: "secret", UserAgent: "ermite/test"})
	require.NoError(t, err)
	res, err := c.Releases().GetRelease(t.Context(), connect.NewRequest(&ermite.GetReleaseRequest{Repo: "proj", Selector: &ermite.GetReleaseRequest_Id{Id: 7}}))
	require.NoError(t, err)
	assert.Equal(t, int64(7), res.Msg.GetRelease().GetId())
	assert.Equal(t, "proj", res.Msg.GetRelease().GetRepo())
	assert.Contains(t, client.UsedFeatures(), client.FeatureReleases)
}
