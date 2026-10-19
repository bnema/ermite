// SPDX-License-Identifier: AGPL-3.0-or-later

package clientlimit_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
)

func newSubs() (*clientlimit.SubmissionLimiter, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	return clientlimit.NewSubmissionLimiter(clk.now), clk
}

func TestSubmissionHourWindow(t *testing.T) {
	t.Parallel()
	l, clk := newSubs()
	for i := range domain.SubmitPerHour {
		ok, _ := l.Allow(bg, "1.2.3.4")
		require.True(t, ok, "submission %d", i)
		l.Record(bg, "1.2.3.4")
		clk.t = clk.t.Add(time.Minute)
	}
	ok, wait := l.Allow(bg, "1.2.3.4")
	assert.False(t, ok)
	assert.Equal(t, time.Hour-3*time.Minute, wait, "until the first one leaves the hour")

	other, _ := l.Allow(bg, "5.6.7.8")
	assert.True(t, other, "another client is not affected")

	clk.t = clk.t.Add(wait)
	ok, _ = l.Allow(bg, "1.2.3.4")
	assert.True(t, ok, "the window slides")
}

func TestSubmissionDayWindow(t *testing.T) {
	t.Parallel()
	l, clk := newSubs()
	start := clk.t
	// 3 per hour, spaced so the hourly limit never bites: one every 2 hours.
	for range domain.SubmitPerDay {
		ok, _ := l.Allow(bg, "1.2.3.4")
		require.True(t, ok)
		l.Record(bg, "1.2.3.4")
		clk.t = clk.t.Add(2 * time.Hour)
	}
	// 20h after start: hourly window empty, day window full.
	ok, wait := l.Allow(bg, "1.2.3.4")
	assert.False(t, ok)
	assert.Equal(t, start.Add(clientlimit.DayWindow).Sub(clk.t), wait)

	clk.t = start.Add(clientlimit.DayWindow)
	ok, _ = l.Allow(bg, "1.2.3.4")
	assert.True(t, ok)
}

func TestSubmissionIPv6GroupsBy64(t *testing.T) {
	t.Parallel()
	l, _ := newSubs()
	for i := range domain.SubmitPerHour {
		l.Record(bg, fmt.Sprintf("2001:db8:1:2::%x", i+1)) // same /64
	}
	ok, _ := l.Allow(bg, "2001:db8:1:2:ffff::1")
	assert.False(t, ok, "the whole /64 shares the budget")
	ok, _ = l.Allow(bg, "2001:db8:1:3::1")
	assert.True(t, ok, "another /64 does not")
	ok, _ = l.Allow(bg, "::ffff:1.2.3.4")
	assert.True(t, ok)
	l.Record(bg, "::ffff:1.2.3.4")
	perAddr, _ := l.Recent(bg, "1.2.3.4")
	assert.Equal(t, 1, perAddr, "a mapped address counts as its IPv4")
}

func TestSubmissionRecent(t *testing.T) {
	t.Parallel()
	l, clk := newSubs()
	l.Record(bg, "1.1.1.1")
	l.Record(bg, "1.1.1.1")
	l.Record(bg, "2.2.2.2")
	perAddr, global := l.Recent(bg, "1.1.1.1")
	assert.Equal(t, 2, perAddr)
	assert.Equal(t, 3, global)

	clk.t = clk.t.Add(time.Hour)
	perAddr, global = l.Recent(bg, "1.1.1.1")
	assert.Zero(t, perAddr, "Recent looks at the last hour")
	assert.Zero(t, global)
}

func TestSubmissionEvictsOldest(t *testing.T) {
	t.Parallel()
	l, clk := newSubs()
	for i := range clientlimit.MaxTracked + 10 {
		clk.t = clk.t.Add(time.Millisecond)
		l.Record(bg, fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	assert.LessOrEqual(t, l.Tracked(), clientlimit.MaxTracked, "bounded")
	ok, _ := l.Allow(bg, "10.0.0.0")
	assert.True(t, ok, "the oldest were evicted")
}
