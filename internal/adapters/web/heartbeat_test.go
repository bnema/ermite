// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
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
