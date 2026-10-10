// SPDX-License-Identifier: AGPL-3.0-or-later

// Package clientlimit counts failed authentications per client address and
// finds the address of a request behind trusted proxies. The HTTP adapters
// that authenticate callers (smart HTTP, the API) share it.
package clientlimit

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"
)

// Limiter defaults: 10 failed authentications in 10 minutes block the client
// address until the oldest failure leaves the window.
const (
	// MaxFailures is the number of failures in Window that blocks an address.
	MaxFailures = 10
	// Window is how long a failure counts.
	Window = 10 * time.Minute

	// sweepInterval is the least time between two sweeps of expired entries.
	sweepInterval = Window / 10
	// maxTracked bounds memory: when the table reaches it, expired entries
	// are dropped and then the oldest tenth of the rest, in one pass.
	maxTracked = 100_000
	// evictFraction is the share of maxTracked evicted at once, so that the
	// cost of an eviction pass is spread over many failures.
	evictFraction = 10

	// ipv6Prefix is the width at which IPv6 clients are grouped: one network
	// (a /64) is what a single subscriber gets, so it counts as one client.
	ipv6Prefix = 64
)

// Limiter counts failed authentications per client, in memory. Clients are
// IPv4 addresses and IPv6 /64 networks. It is safe for concurrent use.
//
// Blocked and Fail are O(1) amortized: they touch one entry, a sweep of the
// expired entries runs at most once per sweepInterval, and a full table is
// trimmed by a tenth in one pass.
type Limiter struct {
	now func() time.Time

	mu        sync.Mutex
	failures  map[string][]time.Time // oldest first, all within Window
	lastSweep time.Time
}

// NewLimiter builds a limiter. now defaults to time.Now.
func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, failures: make(map[string][]time.Time)}
}

// limiterKey is the identity the limiter counts: an IPv4 address (also when
// mapped into IPv6), or the /64 network of an IPv6 address. Anything that is
// not an IP address is used as is.
func limiterKey(addr string) string {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	return netip.PrefixFrom(a, ipv6Prefix).Masked().String()
}

// Blocked reports whether addr has used up its failures, and how long until
// it may try again.
func (l *Limiter) Blocked(_ context.Context, addr string) (bool, time.Duration) {
	key := limiterKey(addr)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	fs := l.prune(key, now)
	if len(fs) < MaxFailures {
		return false, 0
	}
	// Free again when the failure that makes it MaxFailures leaves the window.
	wait := fs[len(fs)-MaxFailures].Add(Window).Sub(now)
	return true, max(wait, time.Second)
}

// Fail records a failed authentication from addr.
func (l *Limiter) Fail(ctx context.Context, addr string) {
	key := limiterKey(addr)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maintain(now)
	fs := append(l.prune(key, now), now)
	if len(fs) > MaxFailures*2 {
		fs = fs[len(fs)-MaxFailures*2:] // enough to compute the wait
	}
	l.failures[key] = fs
	if len(fs) == MaxFailures {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Str("client", key).Msg("client blocked after repeated failed authentications")
	}
}

// maintain keeps the table small without a pass per call: expired entries
// go at most once per sweepInterval, and a table at its bound is trimmed.
// The caller holds mu.
func (l *Limiter) maintain(now time.Time) {
	if now.Sub(l.lastSweep) >= sweepInterval {
		l.sweep(now)
	}
	if len(l.failures) >= maxTracked {
		l.sweep(now) // a full table may hold expired entries the interval kept
		l.evictOldest()
	}
}

// prune drops the expired failures of key and returns the rest. The caller
// holds mu.
func (l *Limiter) prune(key string, now time.Time) []time.Time {
	fs := l.failures[key]
	cut := now.Add(-Window)
	i := 0
	for i < len(fs) && !fs[i].After(cut) {
		i++
	}
	if i == len(fs) {
		delete(l.failures, key)
		return nil
	}
	if i > 0 {
		fs = fs[i:]
		l.failures[key] = fs
	}
	return fs
}

// sweep drops every expired entry. The caller holds mu.
func (l *Limiter) sweep(now time.Time) {
	l.lastSweep = now
	for key := range l.failures {
		l.prune(key, now)
	}
}

// evictOldest drops the entries whose last failure is the oldest, enough to
// leave the table a tenth under maxTracked. It is a single pass plus a sort.
// The caller holds mu.
func (l *Limiter) evictOldest() {
	drop := len(l.failures) - (maxTracked - maxTracked/evictFraction)
	if drop <= 0 {
		return
	}
	type entry struct {
		key  string
		last time.Time
	}
	all := make([]entry, 0, len(l.failures))
	for key, fs := range l.failures {
		all = append(all, entry{key, fs[len(fs)-1]})
	}
	slices.SortFunc(all, func(a, b entry) int { return a.last.Compare(b.last) })
	for _, e := range all[:drop] {
		delete(l.failures, e.key)
	}
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
