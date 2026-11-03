// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellRailOnlyWithLayoutAndComponent(t *testing.T) {
	for _, tc := range []struct {
		name          string
		hasRail, rail bool
	}{
		{"with rail", true, true},
		{"without rail component", true, false},
		{"without rail layout", false, true},
		{"without rail", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rail templ.Component
			if tc.rail {
				rail = summaryRail(SummaryPage{})
			}
			html := renderToString(t, Shell(Layout{HasRail: tc.hasRail}, rail))
			want := tc.hasRail && tc.rail
			assert.Equal(t, want, strings.Contains(html, `class="work has-rail"`))
			assert.Equal(t, want, strings.Contains(html, `<aside class="rail"`))
			assert.Contains(t, html, `<main id="main" tabindex="-1">`, "main stays on the canvas; only its primary block is a card")
		})
	}
}

func TestCommitsRailStats(t *testing.T) {
	for _, tc := range []struct {
		name string
		page CommitsPage
		want []string
		not  []string
	}{
		{
			name: "one commit, one day",
			page: CommitsPage{Ref: "main", Count: 1, Authors: 1, From: "2026-01-02", To: "2026-01-02"},
			want: []string{"<b>1</b> commit</li>", "<b>1</b> author</li>", `<p class="h-dates">2026-01-02</p>`, "no tag or branch head here"},
			not:  []string{"→"},
		},
		{
			name: "several commits, authors and days, with refs",
			page: CommitsPage{Ref: "main", Count: 3, Authors: 2, From: "2026-01-01", To: "2026-01-03", Refs: []PageRef{
				{Name: "v1.0.0", Tag: true, Href: "#c-abc1234", Short: "abc1234", When: "1d ago"},
				{Name: "main", Href: "#c-def5678", Short: "def5678", When: "now"},
			}},
			want: []string{"<b>3</b> commits</li>", "<b>2</b> authors</li>", "2026-01-01 → 2026-01-03", `<span class="count">2</span>`,
				`href="#c-abc1234"><span class="ico i-tag"`, `href="#c-def5678"><span class="ico i-branch"`, "<code>abc1234</code> · 1d ago"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			html := renderToString(t, commitsRail(tc.page))
			for _, w := range tc.want {
				assert.Contains(t, html, w)
			}
			for _, n := range tc.not {
				assert.NotContains(t, html, n)
			}
		})
	}
}

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
