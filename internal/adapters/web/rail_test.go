// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRailDrawerAsset(t *testing.T) {
	assets, err := loadAssets()
	require.NoError(t, err)
	url := assets.urls.Rail
	require.NotEmpty(t, url)
	script, ok := assets.byName[strings.TrimPrefix(url, StaticPrefix)]
	require.True(t, ok)
	assert.Equal(t, "text/javascript; charset=utf-8", script.typ)
	layout := Layout{Assets: assets.urls, HasRail: true}
	html := renderToString(t, Shell(layout, summaryRail(SummaryPage{})))
	assert.Contains(t, html, `<script src="`+url+`" defer></script>`)
	assert.Contains(t, html, `<aside class="rail" aria-label="Context">`)
	assert.Contains(t, html, `aria-labelledby="r-clone"`)
	assert.NotContains(t, renderToString(t, Shell(layout, nil)), `<script src="`+url+`"`)
}
