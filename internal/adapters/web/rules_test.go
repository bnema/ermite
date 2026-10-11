// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The speculation rules prefetch the light pages and leave the heavy ones
// (several Git reads each) to a click.
func TestSpeculationRulesSkipHeavyPages(t *testing.T) {
	raw, err := staticFS.ReadFile("static/rules.json")
	require.NoError(t, err)
	var rules struct {
		Prefetch []struct {
			Where struct {
				And []struct {
					Href string `json:"href_matches"`
					Not  *struct {
						Href string `json:"href_matches"`
					} `json:"not"`
				} `json:"and"`
			} `json:"where"`
			Eagerness string `json:"eagerness"`
		} `json:"prefetch"`
	}
	require.NoError(t, json.Unmarshal(raw, &rules))
	require.Len(t, rules.Prefetch, 1)
	assert.Equal(t, "moderate", rules.Prefetch[0].Eagerness)

	var allowed string
	excluded := map[string]bool{}
	for _, c := range rules.Prefetch[0].Where.And {
		if c.Href != "" {
			allowed = c.Href
		}
		if c.Not != nil && c.Not.Href != "" {
			excluded[c.Not.Href] = true
		}
	}
	assert.Equal(t, "/*", allowed)
	for _, p := range []string{
		"/-/*", "/*/raw/*", "/*/tree", "/*/tree/*", "/*/blob/*", "/*/commit/*",
		"/*/commits", "/*/commits/*", "/*/compare/*", "/*/refs", "/*/merge-requests/*/since",
	} {
		assert.True(t, excluded[p], "%s is not prefetched", p)
	}
	for _, p := range []string{"/", "/*", "/*/merge-requests", "/*/merge-requests/*"} {
		assert.False(t, excluded[p], "%s stays prefetched", p)
	}
}
