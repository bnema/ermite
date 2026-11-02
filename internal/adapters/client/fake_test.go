// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"connectrpc.com/connect/v2/connecthttp"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect/v2"

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
	version func(context.Context, *ermite.GetVersionRequest) (*ermite.GetVersionResponse, error)
	watch   func(context.Context, *ermite.WatchRequest, ermiteconnect.EventServiceWatchServerStream) error
}

func (f *fake) record(header *connect.Header) {
	h := make(http.Header)
	for k, v := range header.All() {
		h[k] = v
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, h.Clone())
}

func (f *fake) GetVersion(ctx context.Context, req *ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	f.record(info.RequestHeader())
	return f.version(ctx, req)
}

func (f *fake) Watch(ctx context.Context, req *ermite.WatchRequest, st ermiteconnect.EventServiceWatchServerStream) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	f.record(info.RequestHeader())
	return f.watch(ctx, req, st)
}

// start serves f under /-/api/ and returns a client for it.
func start(t *testing.T, f *fake, cfg client.Config) *client.Client {
	t.Helper()
	mux := http.NewServeMux()
	{
		server := connect.NewServer()
		ermiteconnect.RegisterServerServiceHandler(server, f)
		sub := http.NewServeMux()
		connecthttp.Mount(sub, server, connecthttp.WithReadMaxBytes(0))
		mux.Handle("/-/api/ermite.ServerService/", http.StripPrefix("/-/api", sub))
	}
	{
		server := connect.NewServer()
		ermiteconnect.RegisterEventServiceHandler(server, f)
		sub := http.NewServeMux()
		connecthttp.Mount(sub, server, connecthttp.WithReadMaxBytes(0))
		mux.Handle("/-/api/ermite.EventService/", http.StripPrefix("/-/api", sub))
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg.URL = srv.URL
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
