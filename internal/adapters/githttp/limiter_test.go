// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp_test

import (
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/adapters/githttp"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestLimiter(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := githttp.NewLimiter(clk.now)

	for range githttp.MaxFailures - 1 {
		l.Fail("1.2.3.4")
	}
	blocked, _ := l.Blocked("1.2.3.4")
	assert.False(t, blocked, "9 failures do not block")

	l.Fail("1.2.3.4")
	blocked, wait := l.Blocked("1.2.3.4")
	assert.True(t, blocked, "10 failures block")
	assert.Equal(t, githttp.Window, wait)

	other, _ := l.Blocked("5.6.7.8")
	assert.False(t, other, "another address is not affected")

	clk.t = clk.t.Add(githttp.Window - time.Second)
	blocked, wait = l.Blocked("1.2.3.4")
	assert.True(t, blocked)
	assert.Equal(t, time.Second, wait)

	clk.t = clk.t.Add(time.Second)
	blocked, _ = l.Blocked("1.2.3.4")
	assert.False(t, blocked, "free once the window passed")
	assert.Zero(t, l.Tracked(), "expired entries are pruned")
}

func TestLimiterSlidingWindow(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := githttp.NewLimiter(clk.now)
	for range 5 {
		l.Fail("a")
	}
	clk.t = clk.t.Add(githttp.Window - time.Minute)
	for range 5 {
		l.Fail("a")
	}
	blocked, wait := l.Blocked("a")
	assert.True(t, blocked)
	assert.Equal(t, time.Minute, wait, "free when the oldest of the last ten leaves")

	clk.t = clk.t.Add(time.Minute)
	blocked, _ = l.Blocked("a")
	assert.False(t, blocked, "five old failures expired, five remain")
}

func TestLimiterBoundsMemory(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	l := githttp.NewLimiter(clk.now)
	for range 5000 {
		l.Fail("x")
	}
	blocked, _ := l.Blocked("x")
	assert.True(t, blocked)
}

func TestClientAddr(t *testing.T) {
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
			r := &http.Request{RemoteAddr: tt.remote, Header: http.Header{}}
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			assert.Equal(t, tt.want, githttp.ClientAddr(r, tt.trusted))
		})
	}
}
