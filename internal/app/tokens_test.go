// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

type tokenFixture struct {
	store    *mocks.TokenStore
	clock    *mocks.Clock
	sessions *mocks.SessionStore
	tokens   *app.Tokens
	auth     *app.Auth
}

var rootSecret = "root-secret-0123456789abcdef0123456789"

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	f := &tokenFixture{store: mocks.NewTokenStore(t), clock: mocks.NewClock(t), sessions: mocks.NewSessionStore(t)}
	f.clock.EXPECT().Now().Return(now).Maybe()
	f.tokens = app.NewTokens(f.store, f.clock, f.sessions)
	f.auth = app.NewAuth(domain.HashTokenSecret(rootSecret), f.store, f.clock)
	return f
}

func scopes(s ...domain.Scope) []domain.Scope { return s }

func names(t *testing.T, ss ...string) []domain.RepoName {
	t.Helper()
	var out []domain.RepoName
	for _, s := range ss {
		out = append(out, name(t, s))
	}
	return out
}

func TestTokensCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("root creates a token with a well-formed secret", func(t *testing.T) {
		f := newTokenFixture(t)
		var stored domain.Token
		f.store.EXPECT().Create(ctx, mock.Anything).Run(func(_ context.Context, tok domain.Token) { stored = tok }).Return(nil)

		exp := now.Add(48 * time.Hour)
		got, err := f.tokens.Create(ctx, domain.Root(), app.CreateTokenParams{
			Label:     "ci",
			Scopes:    scopes(domain.ScopeRepoWrite, domain.ScopeRepoRead, domain.ScopeRepoRead),
			Repos:     names(t, "b", "a", "b"),
			ExpiresAt: exp,
		})
		require.NoError(t, err)

		assert.True(t, strings.HasPrefix(got.Secret, "ermite_"))
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(got.Secret, "ermite_"))
		require.NoError(t, err)
		assert.Len(t, raw, 32)

		assert.Equal(t, stored, got.Token)
		assert.Equal(t, domain.HashTokenSecret(got.Secret), got.Token.Hash)
		assert.Len(t, got.Token.ID, 16)
		assert.Equal(t, scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), got.Token.Scopes, "canonical order, deduplicated")
		assert.Equal(t, names(t, "a", "b"), got.Token.Repos, "sorted, deduplicated")
		assert.Equal(t, now, got.Token.CreatedAt)
		assert.True(t, exp.Equal(got.Token.ExpiresAt))
		assert.NotContains(t, got.Token.Label, got.Secret)
	})

	t.Run("secrets are unique", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().Create(ctx, mock.Anything).Return(nil)
		a, err := f.tokens.Create(ctx, domain.Root(), app.CreateTokenParams{Label: "a", Scopes: scopes(domain.ScopeRepoRead)})
		require.NoError(t, err)
		b, err := f.tokens.Create(ctx, domain.Root(), app.CreateTokenParams{Label: "a", Scopes: scopes(domain.ScopeRepoRead)})
		require.NoError(t, err)
		assert.NotEqual(t, a.Secret, b.Secret)
		assert.NotEqual(t, a.Token.ID, b.Token.ID)
	})

	t.Run("retries on a collision", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().Create(ctx, mock.Anything).Return(domain.ErrAlreadyExists).Once()
		f.store.EXPECT().Create(ctx, mock.Anything).Return(nil).Once()
		_, err := f.tokens.Create(ctx, domain.Root(), app.CreateTokenParams{Label: "a", Scopes: scopes(domain.ScopeRepoRead)})
		require.NoError(t, err)
	})

	t.Run("store failure", func(t *testing.T) {
		f := newTokenFixture(t)
		boom := errors.New("boom")
		f.store.EXPECT().Create(ctx, mock.Anything).Return(boom)
		_, err := f.tokens.Create(ctx, domain.Root(), app.CreateTokenParams{Label: "a", Scopes: scopes(domain.ScopeRepoRead)})
		assert.ErrorIs(t, err, boom)
	})

	invalid := []struct {
		name   string
		params app.CreateTokenParams
		want   error
	}{
		{"empty label", app.CreateTokenParams{Scopes: scopes(domain.ScopeRepoRead)}, domain.ErrInvalidToken},
		{"long label", app.CreateTokenParams{Label: strings.Repeat("x", 101), Scopes: scopes(domain.ScopeRepoRead)}, domain.ErrInvalidToken},
		{"bad utf8 label", app.CreateTokenParams{Label: "\xff", Scopes: scopes(domain.ScopeRepoRead)}, domain.ErrInvalidToken},
		{"no scopes", app.CreateTokenParams{Label: "x"}, domain.ErrInvalidToken},
		{"unknown scope", app.CreateTokenParams{Label: "x", Scopes: scopes("nope")}, domain.ErrInvalidScope},
		{"past expiry", app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead), ExpiresAt: now}, domain.ErrInvalidToken},
		{"zero repo name", app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead), Repos: []domain.RepoName{{}}}, domain.ErrInvalidToken},
	}
	for _, tt := range invalid {
		t.Run("invalid/"+tt.name, func(t *testing.T) {
			f := newTokenFixture(t)
			_, err := f.tokens.Create(ctx, domain.Root(), tt.params)
			assert.ErrorIs(t, err, tt.want)
		})
	}
}

func TestTokensCreateNoEscalation(t *testing.T) {
	ctx := context.Background()
	exp := now.Add(time.Hour)
	admin := func(repos []domain.RepoName, expires time.Time, extra ...domain.Scope) domain.Principal {
		return domain.TokenPrincipal(domain.Token{
			ID:        "creator",
			Scopes:    append(scopes(domain.ScopeTokenAdmin, domain.ScopeRepoRead, domain.ScopeRepoWrite), extra...),
			Repos:     repos,
			ExpiresAt: expires,
		})
	}

	tests := []struct {
		name      string
		principal domain.Principal
		params    app.CreateTokenParams
		want      error // nil = allowed
	}{
		{"anonymous", domain.Anonymous(), app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead)}, domain.ErrUnauthenticated},
		{"token without token:admin",
			domain.TokenPrincipal(domain.Token{Scopes: scopes(domain.ScopeRepoWrite)}),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead)}, domain.ErrForbidden},
		{"subset of scopes", admin(nil, time.Time{}),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead)}, nil},
		{"same scopes incl. token:admin", admin(nil, time.Time{}),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeTokenAdmin, domain.ScopeRepoWrite)}, nil},
		{"scope it lacks", admin(nil, time.Time{}),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead, domain.ScopeRepoAdmin)}, domain.ErrForbidden},
		{"limited creator, same repos", admin(names(t, "a", "b"), time.Time{}),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead), Repos: names(t, "a")}, nil},
		{"limited creator, repo outside its list", admin(names(t, "a"), time.Time{}),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead), Repos: names(t, "a", "z")}, domain.ErrForbidden},
		{"limited creator, unlimited child", admin(names(t, "a"), time.Time{}),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead)}, domain.ErrForbidden},
		{"unlimited creator may limit", admin(nil, time.Time{}),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead), Repos: names(t, "z")}, nil},
		{"expiring creator, child never expires", admin(nil, exp),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead)}, domain.ErrForbidden},
		{"expiring creator, child outlasts", admin(nil, exp),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead), ExpiresAt: exp.Add(time.Second)}, domain.ErrForbidden},
		{"expiring creator, child within", admin(nil, exp),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead), ExpiresAt: exp.Add(-time.Second)}, nil},
		{"expiring creator, child same expiry", admin(nil, exp),
			app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoRead), ExpiresAt: exp}, nil},
		{"root: everything", domain.Root(),
			app.CreateTokenParams{Label: "x", Scopes: domain.AllScopes()}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTokenFixture(t)
			if tt.want == nil {
				f.store.EXPECT().Create(ctx, mock.Anything).Return(nil)
			}
			got, err := f.tokens.Create(ctx, tt.principal, tt.params)
			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
				assert.Empty(t, got.Secret)
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, got.Secret)
		})
	}
}

func TestTokensListRevoke(t *testing.T) {
	ctx := context.Background()
	tokenAdmin := domain.TokenPrincipal(domain.Token{ID: "adm", Scopes: scopes(domain.ScopeTokenAdmin)})
	writer := domain.TokenPrincipal(domain.Token{Scopes: scopes(domain.ScopeRepoWrite)})

	t.Run("list for the admin token and root", func(t *testing.T) {
		f := newTokenFixture(t)
		want := []domain.Token{{ID: "a", Scopes: scopes(domain.ScopeTokenAdmin)}}
		f.store.EXPECT().List(ctx).Return(want, nil).Twice()
		for _, p := range []domain.Principal{domain.Root(), tokenAdmin} {
			got, err := f.tokens.List(ctx, p)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		}
		_, err := f.tokens.List(ctx, writer)
		assert.ErrorIs(t, err, domain.ErrForbidden)
		_, err = f.tokens.List(ctx, domain.Anonymous())
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	})

	t.Run("revoke", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().Get(ctx, "a").Return(domain.Token{ID: "a", Scopes: scopes(domain.ScopeTokenAdmin)}, nil).Twice()
		f.store.EXPECT().Get(ctx, "zz").Return(domain.Token{}, domain.ErrNotFound).Once()
		f.store.EXPECT().Revoke(ctx, "a").Return(nil).Twice()
		require.NoError(t, f.tokens.Revoke(ctx, tokenAdmin, "a"))
		require.NoError(t, f.tokens.Revoke(ctx, domain.Root(), "a"))
		assert.ErrorIs(t, f.tokens.Revoke(ctx, domain.Root(), "zz"), domain.ErrNotFound)
		assert.ErrorIs(t, f.tokens.Revoke(ctx, writer, "a"), domain.ErrForbidden)
		assert.ErrorIs(t, f.tokens.Revoke(ctx, domain.Anonymous(), "a"), domain.ErrUnauthenticated)
	})
}

// GO-003b: revoking a token with owner rights signs every browser out (the
// session epoch is bumped); revoking any other token does not.
func TestTokensRevokeOwnerRightsEndsWebSessions(t *testing.T) {
	ctx := context.Background()
	ownerTok := domain.Token{ID: "own", Scopes: domain.AllScopes()}

	for name, tc := range map[string]struct {
		tok    domain.Token
		by     domain.Principal
		bumped bool
	}{
		"every scope, by root":         {ownerTok, domain.Root(), true},
		"every scope, by an owner key": {ownerTok, domain.TokenPrincipal(domain.Token{ID: "adm", Scopes: domain.AllScopes()}), true},
		"one scope short":              {domain.Token{ID: "p", Scopes: domain.AllScopes()[1:]}, domain.Root(), false},
		"repo list":                    {domain.Token{ID: "l", Scopes: domain.AllScopes(), Repos: names(t, "a")}, domain.Root(), false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTokenFixture(t)
			f.store.EXPECT().Get(ctx, tc.tok.ID).Return(tc.tok, nil)
			f.store.EXPECT().Revoke(ctx, tc.tok.ID).Return(nil)
			if tc.bumped {
				f.sessions.EXPECT().BumpEpoch(ctx).Return(1, nil).Once()
			}
			require.NoError(t, f.tokens.Revoke(ctx, tc.by, tc.tok.ID))
		})
	}

	t.Run("a revoke that fails bumps nothing", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().Get(ctx, "own").Return(ownerTok, nil)
		f.store.EXPECT().Revoke(ctx, "own").Return(errors.New("kv down"))
		assert.Error(t, f.tokens.Revoke(ctx, domain.Root(), "own"))
	})

	t.Run("a token out of reach is not revoked and bumps nothing", func(t *testing.T) {
		f := newTokenFixture(t)
		limited := domain.TokenPrincipal(domain.Token{ID: "adm", Scopes: scopes(domain.ScopeTokenAdmin)})
		f.store.EXPECT().Get(ctx, "own").Return(ownerTok, nil)
		assert.ErrorIs(t, f.tokens.Revoke(ctx, limited, "own"), domain.ErrNotFound)
	})

	t.Run("a failure to sign out is reported once the token is revoked", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().Get(ctx, "own").Return(ownerTok, nil)
		f.store.EXPECT().Revoke(ctx, "own").Return(nil)
		f.sessions.EXPECT().BumpEpoch(ctx).Return(0, errors.New("kv down"))
		assert.ErrorContains(t, f.tokens.Revoke(ctx, domain.Root(), "own"), "signing out")
	})
}

// A token admin sees and revokes only the tokens it could have created: the
// no-escalation rule of Create, applied to scopes, repositories and expiry.
func TestTokensAdminReach(t *testing.T) {
	ctx := context.Background()
	exp := now.Add(48 * time.Hour)
	admin := func(repos []domain.RepoName, expires time.Time, extra ...domain.Scope) domain.Principal {
		return domain.TokenPrincipal(domain.Token{
			ID:        "caller",
			Scopes:    append(scopes(domain.ScopeTokenAdmin, domain.ScopeRepoRead, domain.ScopeRepoWrite), extra...),
			Repos:     repos,
			ExpiresAt: expires,
		})
	}
	tok := func(id string, sc []domain.Scope, repos []domain.RepoName, expires time.Time) domain.Token {
		return domain.Token{ID: id, Scopes: sc, Repos: repos, ExpiresAt: expires}
	}
	rw := scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite)
	ab, a := names(t, "a", "b"), names(t, "a")

	tests := []struct {
		name   string
		caller domain.Principal
		target domain.Token
		within bool
	}{
		{"fewer scopes", admin(nil, time.Time{}), tok("t", scopes(domain.ScopeRepoRead), nil, time.Time{}), true},
		{"the same rights", admin(ab, exp), tok("t", rw, ab, exp), true},
		{"a scope the caller lacks", admin(nil, time.Time{}), tok("t", scopes(domain.ScopeRepoRead, domain.ScopeRepoAdmin), nil, time.Time{}), false},
		{"root-only reach: a token:admin caller cannot reach a mr:merge token", admin(nil, time.Time{}), tok("t", scopes(domain.ScopeMRMerge), nil, time.Time{}), false},
		{"unlimited token, limited caller", admin(a, time.Time{}), tok("t", rw, nil, time.Time{}), false},
		{"repo outside the caller's list", admin(a, time.Time{}), tok("t", rw, ab, time.Time{}), false},
		{"repo inside the caller's list", admin(ab, time.Time{}), tok("t", rw, a, time.Time{}), true},
		{"limited token, unlimited caller", admin(nil, time.Time{}), tok("t", rw, ab, time.Time{}), true},
		{"never expires, caller expires", admin(nil, exp), tok("t", rw, nil, time.Time{}), false},
		{"expires later than the caller", admin(nil, exp), tok("t", rw, nil, exp.Add(time.Hour)), false},
		{"expires sooner than the caller", admin(nil, exp), tok("t", rw, nil, exp.Add(-time.Hour)), true},
		{"expiring token, caller never expires", admin(nil, time.Time{}), tok("t", rw, nil, exp), true},
	}
	for _, tt := range tests {
		t.Run("list: "+tt.name, func(t *testing.T) {
			f := newTokenFixture(t)
			other := tok("other", scopes(domain.ScopeRepoRead), nil, time.Time{})
			f.store.EXPECT().List(ctx).Return([]domain.Token{tt.target, other}, nil)
			got, err := f.tokens.List(ctx, tt.caller)
			require.NoError(t, err)
			if !tt.within {
				// Only the token that is within reach of every caller shows.
				assert.NotContains(t, got, tt.target)
				return
			}
			assert.Contains(t, got, tt.target)
		})
		t.Run("revoke: "+tt.name, func(t *testing.T) {
			f := newTokenFixture(t)
			f.store.EXPECT().Get(ctx, "t").Return(tt.target, nil)
			if tt.within {
				f.store.EXPECT().Revoke(ctx, "t").Return(nil)
				require.NoError(t, f.tokens.Revoke(ctx, tt.caller, "t"))
				return
			}
			// Not leaked: the same answer as for a token that does not exist, and nothing is deleted.
			assert.ErrorIs(t, f.tokens.Revoke(ctx, tt.caller, "t"), domain.ErrNotFound)
		})
	}

	t.Run("root lists and revokes everything", func(t *testing.T) {
		f := newTokenFixture(t)
		all := []domain.Token{tok("x", domain.AllScopes(), nil, time.Time{}), tok("y", scopes(domain.ScopeMRMerge), a, exp)}
		f.store.EXPECT().List(ctx).Return(all, nil)
		got, err := f.tokens.List(ctx, domain.Root())
		require.NoError(t, err)
		assert.Equal(t, all, got)
		f.store.EXPECT().Get(ctx, "x").Return(all[0], nil)
		f.store.EXPECT().Revoke(ctx, "x").Return(nil) // root has no limits
		f.sessions.EXPECT().BumpEpoch(ctx).Return(1, nil) // "x" has owner rights
		require.NoError(t, f.tokens.Revoke(ctx, domain.Root(), "x"))
	})

	t.Run("an unknown id is not found for everyone", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().Get(ctx, "nope").Return(domain.Token{}, domain.ErrNotFound)
		assert.ErrorIs(t, f.tokens.Revoke(ctx, admin(nil, time.Time{}), "nope"), domain.ErrNotFound)
	})

	t.Run("a store failure on the lookup is returned", func(t *testing.T) {
		f := newTokenFixture(t)
		boom := errors.New("boom")
		f.store.EXPECT().Get(ctx, "t").Return(domain.Token{}, boom)
		assert.ErrorIs(t, f.tokens.Revoke(ctx, admin(nil, time.Time{}), "t"), boom)
	})
}

func TestAuthRevalidate(t *testing.T) {
	ctx := context.Background()
	p := domain.TokenPrincipal(domain.Token{ID: "t1", Scopes: scopes(domain.ScopeRepoWrite)})

	t.Run("root is always valid", func(t *testing.T) {
		f := newTokenFixture(t)
		require.NoError(t, f.auth.Revalidate(ctx, domain.Root()))
	})
	t.Run("anonymous is not", func(t *testing.T) {
		f := newTokenFixture(t)
		assert.ErrorIs(t, f.auth.Revalidate(ctx, domain.Anonymous()), domain.ErrUnauthenticated)
	})
	t.Run("a stored token that exists", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().Get(ctx, "t1").Return(domain.Token{ID: "t1", ExpiresAt: now.Add(time.Hour)}, nil)
		require.NoError(t, f.auth.Revalidate(ctx, p))
	})
	t.Run("revoked", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().Get(ctx, "t1").Return(domain.Token{}, domain.ErrNotFound)
		assert.ErrorIs(t, f.auth.Revalidate(ctx, p), domain.ErrUnauthenticated)
	})
	t.Run("expired", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().Get(ctx, "t1").Return(domain.Token{ID: "t1", ExpiresAt: now}, nil)
		assert.ErrorIs(t, f.auth.Revalidate(ctx, p), domain.ErrUnauthenticated)
	})
	t.Run("a store failure is an error, not a pass", func(t *testing.T) {
		f := newTokenFixture(t)
		boom := errors.New("boom")
		f.store.EXPECT().Get(ctx, "t1").Return(domain.Token{}, boom)
		err := f.auth.Revalidate(ctx, p)
		require.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, domain.ErrUnauthenticated)
	})
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	secret := "ermite_stored-secret"
	hash := domain.HashTokenSecret(secret)
	tok := domain.Token{
		ID: "t1", Hash: hash, Scopes: scopes(domain.ScopeRepoWrite), Repos: names(t, "a"),
		ExpiresAt: now.Add(time.Hour),
	}

	t.Run("root", func(t *testing.T) {
		f := newTokenFixture(t)
		p, err := f.auth.Authenticate(ctx, rootSecret)
		require.NoError(t, err)
		assert.True(t, p.IsRoot())
	})

	t.Run("empty and unknown secrets", func(t *testing.T) {
		f := newTokenFixture(t)
		_, err := f.auth.Authenticate(ctx, "")
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		f.store.EXPECT().FindByHash(ctx, domain.HashTokenSecret("nope")).Return(domain.Token{}, domain.ErrNotFound)
		_, err = f.auth.Authenticate(ctx, "nope")
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	})

	t.Run("stored token first use touches", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().FindByHash(ctx, hash).Return(tok, nil)
		f.store.EXPECT().Touch(ctx, "t1", now).Return(nil)
		p, err := f.auth.Authenticate(ctx, secret)
		require.NoError(t, err)
		assert.Equal(t, "t1", p.TokenID())
		assert.True(t, p.Allows(domain.ScopeRepoWrite, name(t, "a")))
		assert.False(t, p.Allows(domain.ScopeRepoWrite, name(t, "b")))
	})

	t.Run("touch at most once a minute", func(t *testing.T) {
		f := newTokenFixture(t)
		recent := tok
		recent.LastUsedAt = now.Add(-30 * time.Second)
		f.store.EXPECT().FindByHash(ctx, hash).Return(recent, nil)
		_, err := f.auth.Authenticate(ctx, secret) // no Touch expected
		require.NoError(t, err)

		f2 := newTokenFixture(t)
		old := tok
		old.LastUsedAt = now.Add(-time.Minute)
		f2.store.EXPECT().FindByHash(ctx, hash).Return(old, nil)
		f2.store.EXPECT().Touch(ctx, "t1", now).Return(nil)
		_, err = f2.auth.Authenticate(ctx, secret)
		require.NoError(t, err)
	})

	t.Run("touch failure does not fail authentication", func(t *testing.T) {
		f := newTokenFixture(t)
		f.store.EXPECT().FindByHash(ctx, hash).Return(tok, nil)
		f.store.EXPECT().Touch(ctx, "t1", now).Return(errors.New("disk"))
		_, err := f.auth.Authenticate(ctx, secret)
		require.NoError(t, err)
	})

	t.Run("expired", func(t *testing.T) {
		f := newTokenFixture(t)
		exp := tok
		exp.ExpiresAt = now
		f.store.EXPECT().FindByHash(ctx, hash).Return(exp, nil)
		_, err := f.auth.Authenticate(ctx, secret)
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	})

	t.Run("store error is not an authentication failure", func(t *testing.T) {
		f := newTokenFixture(t)
		boom := errors.New("boom")
		f.store.EXPECT().FindByHash(ctx, hash).Return(domain.Token{}, boom)
		_, err := f.auth.Authenticate(ctx, secret)
		assert.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, domain.ErrUnauthenticated)
	})
}
