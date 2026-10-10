// SPDX-License-Identifier: AGPL-3.0-or-later

package httpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/httpserver"
)

var securityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'self'; script-src 'self' 'unsafe-eval'; frame-ancestors 'none'",
	"X-Frame-Options":         "DENY",
	"X-Content-Type-Options":  "nosniff",
	"Referrer-Policy":         "same-origin",
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHealth(t *testing.T) {
	ok := httpserver.Check(func(context.Context) error { return nil })
	fail := httpserver.Check(func(context.Context) error { return errors.New("kv down: secret detail") })

	tests := []struct {
		name       string
		checks     []httpserver.Check
		method     string
		wantStatus int
		wantBody   string
	}{
		{"no checks", nil, "GET", 200, "ok\n"},
		{"all pass", []httpserver.Check{ok, ok}, "GET", 200, "ok\n"},
		{"head", []httpserver.Check{ok}, "HEAD", 200, ""}, // the real server drops the body
		{"one fails", []httpserver.Check{ok, fail}, "GET", 503, "unavailable\n"},
		{"head fails", []httpserver.Check{fail}, "HEAD", 503, ""},
		{"post", []httpserver.Check{ok}, "POST", 405, ""},
		{"delete", []httpserver.Check{ok}, "DELETE", 405, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := httpserver.New(httpserver.Config{HealthChecks: tt.checks}).Handler()
			rec := do(t, h, tt.method, "/-/health")
			assert.Equal(t, tt.wantStatus, rec.Code)
			if tt.method == "GET" {
				assert.Equal(t, tt.wantBody, rec.Body.String())
			}
			assert.NotContains(t, rec.Body.String(), "secret detail", "the failure cause stays in the log")
			if tt.wantStatus == 405 {
				assert.Contains(t, rec.Header().Get("Allow"), "GET")
			}
			for k, v := range securityHeaders {
				assert.Equal(t, v, rec.Header().Get(k), "%s on %s", k, tt.name)
			}
		})
	}
}

func TestHeadOverTheWire(t *testing.T) {
	ts := httptest.NewServer(httpserver.New(httpserver.Config{}).Handler())
	defer ts.Close()
	resp, err := http.Head(ts.URL + "/-/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, 200, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Empty(t, body)
}

func TestNotFoundCarriesSecurityHeaders(t *testing.T) {
	h := httpserver.New(httpserver.Config{}).Handler()
	for _, target := range []string{"/", "/nope", "/-/", "/-/health/x", "/-/healthz", "/some-repo.git/info/refs"} {
		rec := do(t, h, "GET", target)
		assert.Equal(t, 404, rec.Code, target)
		for k, v := range securityHeaders {
			assert.Equal(t, v, rec.Header().Get(k), "%s on %s", k, target)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRequestLog(t *testing.T) {
	var buf syncBuffer
	ctx := zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &buf}))
	h := httpserver.New(httpserver.Config{}).Handler()

	req := httptest.NewRequestWithContext(ctx, "GET", "/missing?token=hunter2", nil)
	req.Header.Set("Authorization", "Bearer hunter2")
	h.ServeHTTP(httptest.NewRecorder(), req)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace([]byte(buf.String())), &entry), buf.String())
	assert.Equal(t, "debug", entry["level"])
	assert.Equal(t, "GET", entry["method"])
	assert.Equal(t, "/missing", entry["path"])
	assert.EqualValues(t, 404, entry["status"])
	assert.Contains(t, entry, "duration")
	assert.NotContains(t, buf.String(), "hunter2")

	// At info level a request logs nothing.
	var quiet syncBuffer
	ctx = zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "info", Format: "json", Output: &quiet}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, "GET", "/missing", nil))
	assert.Empty(t, quiet.String())
}

func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	check := httpserver.DirWritable(dir)
	require.NoError(t, check(context.Background()))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the probe file is removed")

	assert.Error(t, httpserver.DirWritable(dir+"/missing")(context.Background()))

	if os.Geteuid() != 0 {
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		assert.Error(t, check(context.Background()))
	}
}

func TestServeAndGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	release := make(chan struct{})
	slow := httpserver.Check(func(context.Context) error {
		close(started)
		<-release
		return nil
	})
	srv := httpserver.New(httpserver.Config{HealthChecks: []httpserver.Check{slow}})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	// A request in flight when shutdown starts is allowed to finish.
	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/-/health")
		if err != nil {
			res <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		res <- result{body: string(b), err: err}
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Serve returned while a request was in flight: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)

	r := <-res
	require.NoError(t, r.err)
	assert.Equal(t, "ok\n", r.body)
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the request finished")
	}
	_, err = net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond)
	assert.Error(t, err, "the listener is closed")
}

func TestRunListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	err = httpserver.New(httpserver.Config{Addr: ln.Addr().String()}).Run(context.Background())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "listen on"), err.Error())
}
