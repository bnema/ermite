// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
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
	require.Len(t, m.Icons, len(appIcons))
	for _, ic := range m.Icons {
		a, ok := srv.assets.byName[strings.TrimPrefix(ic.Src, StaticPrefix)]
		require.True(t, ok, ic.Src)
		assert.Equal(t, "image/png", a.typ)
	}
	_, ok = srv.assets.byName[strings.TrimPrefix(u.AppleIcon, StaticPrefix)]
	assert.True(t, ok)

	page := renderToString(t, Shell(Layout{Assets: u}, nil))
	assert.Contains(t, page, `<link rel="manifest" href="`+u.Manifest+`">`)
	assert.Contains(t, page, `<link rel="apple-touch-icon" href="`+u.AppleIcon+`">`)
}
