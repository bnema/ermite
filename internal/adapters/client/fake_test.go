// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/adapters/client"
)

// fake is a Connect server with stub services.
type fake struct {
	ermiteconnect.UnimplementedServerServiceHandler
	ermiteconnect.UnimplementedEventServiceHandler

	mu      sync.Mutex
	headers []http.Header
	version func(context.Context, *connect.Request[ermite.GetVersionRequest]) (*connect.Response[ermite.GetVersionResponse], error)
	watch   func(context.Context, *connect.Request[ermite.WatchRequest], *connect.ServerStream[ermite.WatchResponse]) error
}

func (f *fake) record(h http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, h.Clone())
}

func (f *fake) GetVersion(ctx context.Context, req *connect.Request[ermite.GetVersionRequest]) (*connect.Response[ermite.GetVersionResponse], error) {
	f.record(req.Header())
	return f.version(ctx, req)
}

func (f *fake) Watch(ctx context.Context, req *connect.Request[ermite.WatchRequest], st *connect.ServerStream[ermite.WatchResponse]) error {
	f.record(req.Header())
	return f.watch(ctx, req, st)
}

// start serves f under /-/api/ and returns a client for it.
func start(t *testing.T, f *fake, cfg client.Config) *client.Client {
	t.Helper()
	mux := http.NewServeMux()
	p, h := ermiteconnect.NewServerServiceHandler(f)
	mux.Handle("/-/api"+p, http.StripPrefix("/-/api", h))
	p, h = ermiteconnect.NewEventServiceHandler(f)
	mux.Handle("/-/api"+p, http.StripPrefix("/-/api", h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg.URL = srv.URL
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
