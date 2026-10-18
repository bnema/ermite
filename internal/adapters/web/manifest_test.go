// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The web app manifest makes the site installable: every page links it, and
// its icons are served assets.
func TestWebAppManifest(t *testing.T) {
	t.Parallel()
	srv := newBareServer(t)
	u := srv.assets.urls
	man, ok := srv.assets.byName[strings.TrimPrefix(u.Manifest, StaticPrefix)]
	require.True(t, ok)
	assert.Equal(t, "application/manifest+json", man.typ)

	var m webManifest
	require.NoError(t, json.Unmarshal(man.body, &m))
	assert.Equal(t, "standalone", m.Display)
	assert.Equal(t, "/", m.StartURL)
	assert.Equal(t, "/", m.Scope)
	// The id keeps the identity of the app when the hashed URL of the manifest
	// changes with a release.
	assert.Equal(t, "/", m.ID)
	assert.NotEmpty(t, m.Description)
	assert.NotContains(t, string(man.body), "prefer_related_applications")
	require.Len(t, m.Icons, len(appIcons))
	for _, ic := range m.Icons {
		a, ok := srv.assets.byName[strings.TrimPrefix(ic.Src, StaticPrefix)]
		require.True(t, ok, ic.Src)
		assert.Equal(t, "image/png", a.typ)
	}
	_, ok = srv.assets.byName[strings.TrimPrefix(u.AppleIcon, StaticPrefix)]
	assert.True(t, ok)

	// Chrome asks for a 192 and a 512 icon with purpose any.
	var any192, any512 bool
	for _, ic := range m.Icons {
		any192 = any192 || (ic.Sizes == "192x192" && ic.Purpose == "any")
		any512 = any512 || (ic.Sizes == "512x512" && ic.Purpose == "any")
	}
	assert.True(t, any192 && any512)

	page := renderToString(t, Shell(Layout{Assets: u}, nil))
	assert.Contains(t, page, `<link rel="manifest" href="`+u.Manifest+`">`)
	assert.Contains(t, page, `<link rel="apple-touch-icon" href="`+u.AppleIcon+`">`)
}

// The icons are exactly the size the manifest declares: Chrome does not
// install from an icon that is smaller than it says.
func TestAppIconSizes(t *testing.T) {
	t.Parallel()
	for _, ic := range appIcons {
		b, err := staticFS.ReadFile("static/app/" + ic.file)
		require.NoError(t, err)
		cfg, err := png.DecodeConfig(bytes.NewReader(b))
		require.NoError(t, err, ic.file)
		assert.Equal(t, ic.sizes, fmt.Sprintf("%dx%d", cfg.Width, cfg.Height), ic.file)
	}
}

// The service worker is served from the root scope, is revalidated on each
// check, and answers a conditional request with 304.
func TestServiceWorkerRoute(t *testing.T) {
	t.Parallel()
	srv := newBareServer(t)
	get := func(inm string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, ServiceWorkerPath, nil)
		if inm != "" {
			r.Header.Set("If-None-Match", inm)
		}
		srv.ServeHTTP(rec, r)
		return rec
	}
	rec := get("")
	require.Equal(t, http.StatusOK, rec.Code)
	h := rec.Header()
	assert.Equal(t, "text/javascript; charset=utf-8", h.Get("Content-Type"))
	assert.Equal(t, "no-cache", h.Get("Cache-Control"), "a cached worker would never update")
	assert.Equal(t, "/", h.Get("Service-Worker-Allowed"))
	assert.NotEmpty(t, h.Get("ETag"))
	assert.Contains(t, rec.Body.String(), "addEventListener('fetch'")
	assert.NotContains(t, rec.Body.String(), "caches.", "nothing authenticated is stored")

	again := get(h.Get("ETag"))
	assert.Equal(t, http.StatusNotModified, again.Code)

	// The registered script is the one served.
	assert.Contains(t, staticJS(t, "ermite.js"), "register('"+ServiceWorkerPath+"'")
}

func staticJS(t *testing.T, name string) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/js/" + name)
	require.NoError(t, err)
	return string(b)
}
