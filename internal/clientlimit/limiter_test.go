// SPDX-License-Identifier: AGPL-3.0-or-later

package clientlimit_test

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/clientlimit"
)

var bg = context.Background()

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestLimiter(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := clientlimit.NewLimiter(clk.now)

	for range clientlimit.MaxFailures - 1 {
		l.Fail(bg, "1.2.3.4")
	}
	blocked, _ := l.Blocked(bg, "1.2.3.4")
	assert.False(t, blocked, "9 failures do not block")

	l.Fail(bg, "1.2.3.4")
	blocked, wait := l.Blocked(bg, "1.2.3.4")
	assert.True(t, blocked, "10 failures block")
	assert.Equal(t, clientlimit.Window, wait)

	other, _ := l.Blocked(bg, "5.6.7.8")
	assert.False(t, other, "another address is not affected")

	clk.t = clk.t.Add(clientlimit.Window - time.Second)
	blocked, wait = l.Blocked(bg, "1.2.3.4")
	assert.True(t, blocked)
	assert.Equal(t, time.Second, wait)

	clk.t = clk.t.Add(time.Second)
	blocked, _ = l.Blocked(bg, "1.2.3.4")
	assert.False(t, blocked, "free once the window passed")
	assert.Zero(t, l.Tracked(), "expired entries are pruned")
}

func TestLimiterSlidingWindow(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := clientlimit.NewLimiter(clk.now)
	for range 5 {
		l.Fail(bg, "a")
	}
	clk.t = clk.t.Add(clientlimit.Window - time.Minute)
	for range 5 {
		l.Fail(bg, "a")
	}
	blocked, wait := l.Blocked(bg, "a")
	assert.True(t, blocked)
	assert.Equal(t, time.Minute, wait, "free when the oldest of the last ten leaves")

	clk.t = clk.t.Add(time.Minute)
	blocked, _ = l.Blocked(bg, "a")
	assert.False(t, blocked, "five old failures expired, five remain")
}

func TestLimiterOneAddressStaysSmall(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := clientlimit.NewLimiter(clk.now)
	for range 5000 {
		l.Fail(bg, "x")
	}
	blocked, _ := l.Blocked(bg, "x")
	assert.True(t, blocked)
	assert.Equal(t, 1, l.Tracked())
}

func TestLimiterBoundsMemory(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := clientlimit.NewLimiter(clk.now)
	for i := range clientlimit.MaxTracked + 5000 {
		clk.t = clk.t.Add(time.Microsecond) // distinct, increasing last failures
		l.Fail(bg, fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	assert.LessOrEqual(t, l.Tracked(), clientlimit.MaxTracked)

	// The oldest were evicted, the newest kept.
	last := clientlimit.MaxTracked + 5000 - 1
	for range clientlimit.MaxFailures - 1 {
		l.Fail(bg, fmt.Sprintf("10.%d.%d.%d", last>>16&255, last>>8&255, last&255))
	}
	blocked, _ := l.Blocked(bg, fmt.Sprintf("10.%d.%d.%d", last>>16&255, last>>8&255, last&255))
	assert.True(t, blocked, "a recent address keeps its failures")
	l.Fail(bg, "10.0.0.0")
	blocked, _ = l.Blocked(bg, "10.0.0.0")
	assert.False(t, blocked)
}

func TestLimiterSweepsExpiredEntriesOncePerInterval(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := clientlimit.NewLimiter(clk.now)
	for i := range 100 {
		l.Fail(bg, fmt.Sprintf("192.0.2.%d", i))
	}
	require.Equal(t, 100, l.Tracked())

	clk.t = clk.t.Add(clientlimit.Window + time.Second)
	l.Fail(bg, "198.51.100.1") // first call past the interval: sweeps
	assert.Equal(t, 1, l.Tracked(), "expired entries are gone")
}

// The per-failure cost stays flat with a full table: the guard against an
// O(n) pass per Fail.
func BenchmarkLimiterFailAtMaxTracked(b *testing.B) {
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := clientlimit.NewLimiter(clk.now)
	keys := make([]string, clientlimit.MaxTracked)
	for i := range keys {
		keys[i] = fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)
		clk.t = clk.t.Add(time.Microsecond)
		l.Fail(bg, keys[i])
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		clk.t = clk.t.Add(time.Microsecond)
		l.Fail(bg, fmt.Sprintf("172.%d.%d.%d", i>>16&255, i>>8&255, i&255))
		l.Blocked(bg, keys[i%len(keys)])
	}
}

func TestLimiterGroupsIPv6By64(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := clientlimit.NewLimiter(clk.now)

	// Ten different addresses of one /64 count together.
	for i := range clientlimit.MaxFailures {
		l.Fail(bg, fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	assert.Equal(t, 1, l.Tracked())
	for _, addr := range []string{"2001:db8:1:2::ffff", "2001:db8:1:2:aaaa:bbbb:cccc:dddd"} {
		blocked, _ := l.Blocked(bg, addr)
		assert.True(t, blocked, addr)
	}
	// Another /64, and another family, are not affected.
	for _, addr := range []string{"2001:db8:1:3::1", "198.51.100.1"} {
		blocked, _ := l.Blocked(bg, addr)
		assert.False(t, blocked, addr)
	}
}

func TestLimiterIPv4StaysPerAddress(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := clientlimit.NewLimiter(clk.now)
	for range clientlimit.MaxFailures {
		l.Fail(bg, "203.0.113.5")
	}
	blocked, _ := l.Blocked(bg, "203.0.113.5")
	assert.True(t, blocked)
	blocked, _ = l.Blocked(bg, "203.0.113.6")
	assert.False(t, blocked, "a neighbouring IPv4 address is a different client")
	// IPv4-mapped IPv6 is the IPv4 address.
	blocked, _ = l.Blocked(bg, "::ffff:203.0.113.5")
	assert.True(t, blocked)
}

func TestClientAddr(t *testing.T) {
	t.Parallel()
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	tests := []struct {
		name    string
		remote  string
		xff     []string
		trusted []netip.Prefix
		want    string
	}{
		{"no proxies configured", "203.0.113.5:4000", []string{"1.1.1.1"}, nil, "203.0.113.5"},
		{"untrusted peer: header ignored", "203.0.113.5:4000", []string{"1.1.1.1"}, trusted, "203.0.113.5"},
		{"trusted peer, one entry", "10.1.2.3:4000", []string{"198.51.100.7"}, trusted, "198.51.100.7"},
		{"trusted peer, no header", "10.1.2.3:4000", nil, trusted, "10.1.2.3"},
		{"right-most untrusted entry wins over a spoofed left one", "10.1.2.3:4000", []string{"6.6.6.6, 198.51.100.7"}, trusted, "198.51.100.7"},
		{"trusted hops are skipped", "10.1.2.3:4000", []string{"198.51.100.7, 10.9.9.9, 10.8.8.8"}, trusted, "198.51.100.7"},
		{"several header lines", "10.1.2.3:4000", []string{"6.6.6.6", "198.51.100.7"}, trusted, "198.51.100.7"},
		{"all entries trusted", "10.1.2.3:4000", []string{"10.9.9.9"}, trusted, "10.1.2.3"},
		{"garbage entry falls back to the peer", "10.1.2.3:4000", []string{"not-an-ip"}, trusted, "10.1.2.3"},
		{"garbage left of a good entry is never reached", "10.1.2.3:4000", []string{"junk, 198.51.100.7"}, trusted, "198.51.100.7"},
		{"ipv6 peer", "[fd00::1]:4000", []string{"2001:db8::7"}, trusted, "2001:db8::7"},
		{"v4-mapped peer is unmapped", "[::ffff:10.1.2.3]:4000", []string{"198.51.100.7"}, trusted, "198.51.100.7"},
		{"remote without port", "203.0.113.5", nil, trusted, "203.0.113.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &http.Request{RemoteAddr: tt.remote, Header: http.Header{}}
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			assert.Equal(t, tt.want, clientlimit.ClientAddr(r, tt.trusted))
		})
	}
}
