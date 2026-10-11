// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

type sessionFixture struct {
	store    *mocks.SessionStore
	clock    *mocks.Clock
	at       atomic.Pointer[time.Time] // what the clock reads; the test moves it
	sessions *app.Sessions
	key      []byte
}

// advance moves the clock forward by d.
func (f *sessionFixture) advance(d time.Duration) {
	next := f.at.Load().Add(d)
	f.at.Store(&next)
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	f := &sessionFixture{
		store: mocks.NewSessionStore(t),
		clock: mocks.NewClock(t),
		key:   []byte("0123456789abcdef0123456789abcdef"),
	}
	start := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	f.at.Store(&start)
	f.clock.EXPECT().Now().RunAndReturn(func() time.Time { return *f.at.Load() }).Maybe()

	var epoch atomic.Uint64
	f.store.EXPECT().Key(mock.Anything).Return(f.key, nil).Maybe()
	f.store.EXPECT().Epoch(mock.Anything).RunAndReturn(func(context.Context) (uint64, error) { return epoch.Load(), nil }).Maybe()
	f.store.EXPECT().BumpEpoch(mock.Anything).RunAndReturn(func(context.Context) (uint64, error) { return epoch.Add(1), nil }).Maybe()

	s, err := app.NewSessions(context.Background(), f.store, f.clock, "https://forge.example")
	require.NoError(t, err)
	f.sessions = s
	return f
}

func (f *sessionFixture) nonce(t *testing.T) string {
	t.Helper()
	link, err := f.sessions.CreateLoginLink(context.Background(), domain.Root())
	require.NoError(t, err)
	n, ok := strings.CutPrefix(link.URL, "https://forge.example/-/login/")
	require.True(t, ok, link.URL)
	return n
}

func allScopeToken(repos ...domain.RepoName) domain.Principal {
	return domain.TokenPrincipal(domain.Token{ID: "t", Scopes: domain.AllScopes(), Repos: repos})
}

// Only the owner creates a link: root, or a token with every scope and no
// repository list.
func TestLoginLinkIsOwnerOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSessionFixture(t)
	partial := domain.TokenPrincipal(domain.Token{ID: "p", Scopes: domain.AllScopes()[:8]})
	limited := allScopeToken(mustRepo(t, "a"))

	for name, tc := range map[string]struct {
		p   domain.Principal
		err error
	}{
		"root":            {domain.Root(), nil},
		"owner session":   {domain.Owner(), nil},
		"every scope":     {allScopeToken(), nil},
		"one scope short": {partial, domain.ErrForbidden},
		"repo list":       {limited, domain.ErrForbidden},
		"anonymous":       {domain.Anonymous(), domain.ErrUnauthenticated},
	} {
		_, err := f.sessions.CreateLoginLink(ctx, tc.p)
		if tc.err == nil {
			assert.NoError(t, err, name)
		} else {
			assert.ErrorIs(t, err, tc.err, name)
		}
		err = f.sessions.SignOutEverywhere(ctx, tc.p)
		if tc.err == nil {
			assert.NoError(t, err, name)
		} else {
			assert.ErrorIs(t, err, tc.err, name)
		}
	}
}

func mustRepo(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}

// AC1: checking a link does not use it; logging in uses it once; an expired or
// replayed link fails.
func TestLoginLinkLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSessionFixture(t)

	link, err := f.sessions.CreateLoginLink(ctx, domain.Root())
	require.NoError(t, err)
	assert.Equal(t, f.clock.Now().Add(app.LoginLinkTTL), link.ExpiresAt)
	nonce := strings.TrimPrefix(link.URL, "https://forge.example/-/login/")
	assert.Len(t, nonce, 43, "256 bits")

	for range 3 {
		assert.True(t, f.sessions.LoginLinkValid(nonce), "looking never consumes")
	}
	token, exp, err := f.sessions.Login(ctx, nonce)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.Equal(t, f.clock.Now().Add(app.SessionTTL), exp)
	assert.False(t, f.sessions.LoginLinkValid(nonce))
	_, _, err = f.sessions.Login(ctx, nonce)
	assert.ErrorIs(t, err, domain.ErrUnauthenticated, "replay")

	// Expired.
	nonce = f.nonce(t)
	f.advance(app.LoginLinkTTL)
	assert.False(t, f.sessions.LoginLinkValid(nonce))
	_, _, err = f.sessions.Login(ctx, nonce)
	assert.ErrorIs(t, err, domain.ErrUnauthenticated, "expired")

	// Unknown.
	_, _, err = f.sessions.Login(ctx, strings.Repeat("a", 43))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)

	// Two links are independent.
	a, b := f.nonce(t), f.nonce(t)
	assert.NotEqual(t, a, b)
	_, _, err = f.sessions.Login(ctx, a)
	require.NoError(t, err)
	assert.True(t, f.sessions.LoginLinkValid(b))
}

// GO-003a: the session of a link made with a token never outlives that token;
// root and a token that never expires leave it at SessionTTL.
func TestSessionIsCappedByTheTokenThatMadeTheLink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSessionFixture(t)
	tokenExpiring := func(in time.Duration) domain.Principal {
		return domain.TokenPrincipal(domain.Token{ID: "t", Scopes: domain.AllScopes(), ExpiresAt: f.clock.Now().Add(in)})
	}
	login := func(p domain.Principal) (string, time.Time, error) {
		link, err := f.sessions.CreateLoginLink(ctx, p)
		require.NoError(t, err)
		return f.sessions.Login(ctx, strings.TrimPrefix(link.URL, "https://forge.example/-/login/"))
	}
	start := f.clock.Now()

	for name, tc := range map[string]struct {
		p    domain.Principal
		want time.Time
	}{
		"root":                {domain.Root(), start.Add(app.SessionTTL)},
		"owner session":       {domain.Owner(), start.Add(app.SessionTTL)},
		"never expires":       {allScopeToken(), start.Add(app.SessionTTL)},
		"expires sooner":      {tokenExpiring(2 * time.Hour), start.Add(2 * time.Hour)},
		"expires later":       {tokenExpiring(48 * time.Hour), start.Add(app.SessionTTL)},
		"expires exactly TTL": {tokenExpiring(app.SessionTTL), start.Add(app.SessionTTL)},
	} {
		tok, exp, err := login(tc.p)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, exp, name)
		_, err = f.sessions.Verify(ctx, tok)
		assert.NoError(t, err, name)
		// The session token itself carries the cap as its exp claim.
		var c claims
		_, _, err = jwt.NewParser().ParseUnverified(tok, &c)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, c.ExpiresAt.UTC(), name)
	}

	// It stops working when the token would have.
	tok, _, err := login(tokenExpiring(2 * time.Hour))
	require.NoError(t, err)
	f.advance(2*time.Hour - time.Second)
	_, err = f.sessions.Verify(ctx, tok)
	assert.NoError(t, err)
	f.advance(time.Second)
	_, err = f.sessions.Verify(ctx, tok)
	assert.ErrorIs(t, err, domain.ErrUnauthenticated, "the token expired, so did the session")

	// A token that expires before the link is used opens nothing.
	link, err := f.sessions.CreateLoginLink(ctx, tokenExpiring(30*time.Second))
	require.NoError(t, err)
	f.advance(31 * time.Second)
	_, _, err = f.sessions.Login(ctx, strings.TrimPrefix(link.URL, "https://forge.example/-/login/"))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
}

// A single use also holds under concurrent confirmations.
func TestLoginLinkConsumedOnceConcurrently(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	nonce := f.nonce(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := f.sessions.Login(context.Background(), nonce); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, won)
}

func TestLoginLinksAreBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSessionFixture(t)
	for range app.MaxPendingLoginLinks {
		f.nonce(t)
	}
	_, err := f.sessions.CreateLoginLink(ctx, domain.Root())
	assert.ErrorIs(t, err, domain.ErrResourceExhausted)

	f.advance(app.LoginLinkTTL)
	_, err = f.sessions.CreateLoginLink(ctx, domain.Root())
	assert.NoError(t, err, "expired links make room")
}

func sign(t *testing.T, m jwt.SigningMethod, key any, claims jwt.Claims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(m, claims).SignedString(key)
	require.NoError(t, err)
	return s
}

type claims struct {
	Epoch *uint64 `json:"epoch"`
	jwt.RegisteredClaims
}

// AC1: a valid session is the owner; none, alg none, another algorithm, another
// key, an old epoch, expiry, a missing claim and garbage are rejected.
func TestSessionTokenVerification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSessionFixture(t)
	now := f.clock.Now()
	zero, one := uint64(0), uint64(1)
	reg := func(sub string, iat, exp time.Time) jwt.RegisteredClaims {
		return jwt.RegisteredClaims{Subject: sub, IssuedAt: jwt.NewNumericDate(iat), ExpiresAt: jwt.NewNumericDate(exp)}
	}
	good := reg("owner", now, now.Add(time.Hour))

	token, _, err := f.sessions.Login(ctx, f.nonce(t))
	require.NoError(t, err)
	p, err := f.sessions.Verify(ctx, token)
	require.NoError(t, err)
	assert.True(t, p.IsOwner())
	assert.Equal(t, "owner", p.Label())

	cases := map[string]string{
		"alg none":       sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, claims{&zero, good}),
		"hs384":          sign(t, jwt.SigningMethodHS384, f.key, claims{&zero, good}),
		"hs512":          sign(t, jwt.SigningMethodHS512, f.key, claims{&zero, good}),
		"wrong key":      sign(t, jwt.SigningMethodHS256, []byte("another key of 32 bytes.........."), claims{&zero, good}),
		"old epoch":      sign(t, jwt.SigningMethodHS256, f.key, claims{&one, good}),
		"no epoch":       sign(t, jwt.SigningMethodHS256, f.key, claims{nil, good}),
		"expired":        sign(t, jwt.SigningMethodHS256, f.key, claims{&zero, reg("owner", now.Add(-2*time.Hour), now.Add(-time.Hour))}),
		"no expiry":      sign(t, jwt.SigningMethodHS256, f.key, claims{&zero, jwt.RegisteredClaims{Subject: "owner", IssuedAt: jwt.NewNumericDate(now)}}),
		"future iat":     sign(t, jwt.SigningMethodHS256, f.key, claims{&zero, reg("owner", now.Add(time.Hour), now.Add(2*time.Hour))}),
		"other subject":  sign(t, jwt.SigningMethodHS256, f.key, claims{&zero, reg("someone", now, now.Add(time.Hour))}),
		"empty":          "",
		"garbage":        "not.a.jwt",
		"truncated":      token[:len(token)-3],
		"signature swap": token[:strings.LastIndex(token, ".")] + ".AAAA",
		"payload only":   strings.Split(token, ".")[1],
		"three empty":    "..",
	}
	for name, tok := range cases {
		_, err := f.sessions.Verify(ctx, tok)
		assert.ErrorIs(t, err, domain.ErrUnauthenticated, name)
	}
	// The control: the same claims signed right are accepted.
	_, err = f.sessions.Verify(ctx, sign(t, jwt.SigningMethodHS256, f.key, claims{&zero, good}))
	assert.NoError(t, err)

	// Expiry after the session lifetime; and the epoch ends every session.
	f.advance(app.SessionTTL)
	_, err = f.sessions.Verify(ctx, token)
	assert.ErrorIs(t, err, domain.ErrUnauthenticated, "expired after 12 h")
}

func TestSignOutEverywhereEndsSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSessionFixture(t)
	a, _, err := f.sessions.Login(ctx, f.nonce(t))
	require.NoError(t, err)
	b, _, err := f.sessions.Login(ctx, f.nonce(t))
	require.NoError(t, err)
	pending := f.nonce(t)

	require.NoError(t, f.sessions.SignOutEverywhere(ctx, domain.Root()))
	for _, tok := range []string{a, b} {
		_, err := f.sessions.Verify(ctx, tok)
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	}
	assert.True(t, f.sessions.LoginLinkValid(pending), "pending links are not affected")
	c, _, err := f.sessions.Login(ctx, pending)
	require.NoError(t, err)
	_, err = f.sessions.Verify(ctx, c)
	assert.NoError(t, err, "a new session works")
}

// The owner signed in on the web signs what it writes as "owner", where root
// signs as "root" although both hold every right.
func TestAuthorOfOwnerSession(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "owner", app.AuthorOf(domain.Owner()))
	assert.Equal(t, "root", app.AuthorOf(domain.Root()))
	assert.Equal(t, "ci", app.AuthorOf(domain.TokenPrincipal(domain.Token{Label: "ci", Scopes: domain.AllScopes()})))
}
