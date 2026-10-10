// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Limiter defaults: 10 failed authentications in 10 minutes block the client
// address until the oldest failure leaves the window.
const (
	// MaxFailures is the number of failures in Window that blocks an address.
	MaxFailures = 10
	// Window is how long a failure counts.
	Window = 10 * time.Minute

	// sweepAbove is the number of tracked addresses above which expired
	// entries are swept on the next failure.
	sweepAbove = 1024
	// maxTracked bounds memory: past it the entries with the oldest last
	// failure are dropped.
	maxTracked = 100_000
)

// Limiter counts failed authentications per client address, in memory.
// It is safe for concurrent use.
type Limiter struct {
	now func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time // oldest first, all within Window
}

// NewLimiter builds a limiter. now defaults to time.Now.
func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, failures: make(map[string][]time.Time)}
}

// Blocked reports whether addr has used up its failures, and how long until
// it may try again.
func (l *Limiter) Blocked(addr string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	fs := l.prune(addr, now)
	if len(fs) < MaxFailures {
		return false, 0
	}
	// Free again when the failure that makes it MaxFailures leaves the window.
	wait := fs[len(fs)-MaxFailures].Add(Window).Sub(now)
	return true, max(wait, time.Second)
}

// Fail records a failed authentication from addr.
func (l *Limiter) Fail(addr string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) > sweepAbove {
		l.sweep(now)
	}
	fs := append(l.prune(addr, now), now)
	if len(fs) > MaxFailures*2 {
		fs = fs[len(fs)-MaxFailures*2:] // enough to compute the wait
	}
	l.failures[addr] = fs
}

// prune drops the expired failures of addr and returns the rest. The caller
// holds mu.
func (l *Limiter) prune(addr string, now time.Time) []time.Time {
	fs := l.failures[addr]
	cut := now.Add(-Window)
	i := 0
	for i < len(fs) && !fs[i].After(cut) {
		i++
	}
	if i == len(fs) {
		delete(l.failures, addr)
		return nil
	}
	if i > 0 {
		fs = fs[i:]
		l.failures[addr] = fs
	}
	return fs
}

// sweep removes every expired address, and the oldest ones when the table is
// still too large. The caller holds mu.
func (l *Limiter) sweep(now time.Time) {
	for addr := range l.failures {
		l.prune(addr, now)
	}
	for len(l.failures) >= maxTracked {
		var oldest string
		var at time.Time
		for addr, fs := range l.failures {
			if last := fs[len(fs)-1]; oldest == "" || last.Before(at) {
				oldest, at = addr, last
			}
		}
		delete(l.failures, oldest)
	}
}

// Tracked returns how many addresses have failures on record, for tests.
func (l *Limiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.failures)
}

// ClientAddr returns the address a request is attributed to: the host of
// RemoteAddr, or, when that peer is in one of the trusted proxy networks, the
// right-most X-Forwarded-For entry that is not itself a trusted proxy. An
// untrusted peer cannot choose its address: its X-Forwarded-For is ignored.
// A header that does not parse falls back to the peer.
func ClientAddr(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if len(trusted) == 0 || !isTrusted(peer, trusted) {
		return peer.String()
	}
	var entries []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for e := range strings.SplitSeq(h, ",") {
			entries = append(entries, strings.TrimSpace(e))
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(entries[i])
		if err != nil {
			return peer.String() // garbage from a trusted proxy: do not guess
		}
		a = a.Unmap()
		if !isTrusted(a, trusted) {
			return a.String()
		}
	}
	return peer.String()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
