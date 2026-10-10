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
	store  *mocks.TokenStore
	clock  *mocks.Clock
	tokens *app.Tokens
	auth   *app.Auth
}

var rootSecret = "root-secret-0123456789abcdef0123456789"

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	f := &tokenFixture{store: mocks.NewTokenStore(t), clock: mocks.NewClock(t)}
	f.clock.EXPECT().Now().Return(now).Maybe()
	f.tokens = app.NewTokens(f.store, f.clock)
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
	tokenAdmin := domain.TokenPrincipal(domain.Token{Scopes: scopes(domain.ScopeTokenAdmin)})
	writer := domain.TokenPrincipal(domain.Token{Scopes: scopes(domain.ScopeRepoWrite)})

	t.Run("list", func(t *testing.T) {
		f := newTokenFixture(t)
		want := []domain.Token{{ID: "a"}}
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
		f.store.EXPECT().Revoke(ctx, "a").Return(nil).Once()
		f.store.EXPECT().Revoke(ctx, "zz").Return(domain.ErrNotFound).Once()
		require.NoError(t, f.tokens.Revoke(ctx, tokenAdmin, "a"))
		assert.ErrorIs(t, f.tokens.Revoke(ctx, domain.Root(), "zz"), domain.ErrNotFound)
		assert.ErrorIs(t, f.tokens.Revoke(ctx, writer, "a"), domain.ErrForbidden)
		assert.ErrorIs(t, f.tokens.Revoke(ctx, domain.Anonymous(), "a"), domain.ErrUnauthenticated)
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
		assert.True(t, p.IsToken())
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
