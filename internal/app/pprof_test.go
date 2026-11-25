// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServePprof(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- servePprof(ctx, ln) }()

	base := "http://" + ln.Addr().String()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap?debug=1", "/debug/pprof/cmdline"} {
		resp, err := http.Get(base + path)
		require.NoError(t, err, path)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode, path)
	}

	resp, err := http.Get(base + "/")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	cancel()
	require.NoError(t, <-done)
}
