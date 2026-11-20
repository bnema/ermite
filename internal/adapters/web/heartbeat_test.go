// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeartbeat(t *testing.T) {
	t.Parallel()
	assert.Nil(t, heartbeatOf("x", nil, false), "no data, no curve")

	days := make([]int, 90)
	days[10], days[60], days[88] = 30, 4, 2
	h := heartbeatOf("proj", days, false)
	require.NotNil(t, h)
	assert.True(t, strings.HasPrefix(h.Line, "M0 "), h.Line)
	assert.Equal(t, 45, strings.Count(h.Line, " C"), "a point every two days, and today")
	assert.Less(t, len(h.Line)+len(h.Area), 4000, "light enough for every live update")
	assert.True(t, strings.HasPrefix(h.Area, h.Line) && strings.HasSuffix(h.Area, " Z"))
	assert.Equal(t, "hb-proj", h.GradID)
	assert.Equal(t, "Activity: 36 commits in the last 90 days", h.Label)
	assert.True(t, h.Live, "commits in the last week")
	assert.Equal(t, num(heartbeatEndX), h.X)

	// Every point stays inside the box: no overshoot below the base line or
	// above the top.
	for _, f := range strings.FieldsFunc(h.Line, func(r rune) bool { return r == 'M' || r == 'C' || r == ' ' }) {
		v, err := strconv.ParseFloat(f, 64)
		require.NoError(t, err, f)
		assert.GreaterOrEqual(t, v, 0.0)
		assert.LessOrEqual(t, v, heartbeatW)
	}

	quiet := heartbeatOf("q", make([]int, 90), false)
	require.NotNil(t, quiet)
	assert.False(t, quiet.Live)
	assert.Equal(t, num(heartbeatH-heartbeatPad), quiet.Y, "a flat line on the base")
	assert.Equal(t, 1, strings.Count(quiet.Line, " C"), "two points")
	assert.Equal(t, "Activity: 0 commits in the last 90 days", quiet.Label)

	assert.Equal(t, "Activity: at least 36 commits in the last 90 days", heartbeatOf("proj", days, true).Label)
}

func TestPulseCache(t *testing.T) {
	t.Parallel()
	var c pulseCache
	days := make([]int, 90)
	days[80] = 3
	a := c.get("proj", days, false)
	require.NotNil(t, a)
	assert.Same(t, a, c.get("proj", days, false), "same figures, same curve")
	assert.Equal(t, heartbeatOf("proj", days, false), a)

	assert.NotSame(t, a, c.get("other", days, false), "per repository: the gradient id differs")
	assert.NotSame(t, a, c.get("proj", days, true), "partial changes the label")
	days[89] = 1
	b := c.get("proj", days, false)
	assert.NotSame(t, a, b, "new figures, new curve")
	assert.True(t, b.Live)
	assert.Nil(t, c.get("proj", nil, false))
}

func BenchmarkPulseCache(b *testing.B) {
	var c pulseCache
	days := make([]int, 90)
	for i := range days {
		days[i] = i * 7 % 11
	}
	b.ReportAllocs()
	for b.Loop() {
		c.get("neferwl", days, false)
	}
}

func TestPulseVisitors(t *testing.T) {
	t.Parallel()
	var c pulseCache
	days := make([]int, 90)
	days[89] = 2
	visitors := make([]uint64, 90)
	visitors[10], visitors[59], visitors[60], visitors[89] = 30, 100, 1000, 4
	h := c.get("proj", days, false, visitors)
	assert.Equal(t, "Activity: 2 commits, 1134 visitors in the last 90 days", h.Label)
	assert.Equal(t, stripOf(visitors), h.Strip)
	assert.True(t, h.Visitors)
	assert.Equal(t, uint64(1004), h.RecentVisitors, "only the last 30 days")
	assert.Same(t, h, c.get("proj", days, false, visitors))
	visitors[89]++
	assert.NotSame(t, h, c.get("proj", days, false, visitors))
	assert.False(t, c.get("proj", days, false).Visitors)
	assert.False(t, c.get("proj", days, false, visitors[:89]).Visitors, "misaligned data is omitted")
}

func TestStripOf(t *testing.T) {
	t.Parallel()
	assert.Nil(t, stripOf([]uint64{5}))
	cells := stripOf([]uint64{0, 1, 50, 100})
	require.Len(t, cells, 3, "a quiet day has no cell")
	assert.Equal(t, StripCell{X: "98.3", W: "196.7", Level: "0.16"}, cells[0], "one visitor is faint but seen")
	assert.Equal(t, "0.57", cells[1].Level)
	assert.Equal(t, "1.00", cells[2].Level, "the busiest day of the row")
	at := func(s string) float64 {
		f, err := strconv.ParseFloat(s, 64)
		require.NoError(t, err)
		return f
	}
	for _, n := range []int{2, 13, 90, 365} {
		daily := make([]uint64, n)
		for i := range daily {
			daily[i] = 1
		}
		cells := stripOf(daily)
		require.Len(t, cells, n)
		assert.Zero(t, at(cells[0].X))
		for i := 1; i < n; i++ {
			assert.InDelta(t, at(cells[i].X), at(cells[i-1].X)+at(cells[i-1].W), 1e-9, "no seam: n=%d, cell %d", n, i)
		}
		assert.InDelta(t, heartbeatW, at(cells[n-1].X)+at(cells[n-1].W), 1e-9, "to the end of the box: n=%d", n)
	}
}

func TestPulseMarkup(t *testing.T) {
	for _, visitors := range [][]uint64{nil, {7, 0, 12}} {
		t.Run(strconv.Itoa(len(visitors)), func(t *testing.T) {
			var cache pulseCache
			h := cache.get("repo", []int{1, 0, 3}, false, visitors)
			h.End = "2026-11-14"
			var b bytes.Buffer
			require.NoError(t, pulse(h).Render(context.Background(), &b))
			assert.Contains(t, b.String(), `data-days-commits="1,0,3"`)
			assert.Contains(t, b.String(), `data-end="2026-11-14"`)
			assert.Contains(t, b.String(), `data-end-x="590"`)
			assert.NotContains(t, b.String(), `tabindex`)
			if visitors == nil {
				assert.NotContains(t, b.String(), "data-days-visitors")
				assert.NotContains(t, b.String(), "pulse-strip")
			} else {
				assert.Contains(t, b.String(), `data-days-visitors="7,0,12"`)
				assert.Contains(t, b.String(), `class="pulse-strip"`)
				assert.Equal(t, 1, strings.Count(b.String(), `<rect class="none"`))
			}
		})
	}
}
