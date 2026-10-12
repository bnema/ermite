// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

// cookieLifetime: the session lifetime, cut short by an expiry that comes
// first, and no session at all when under a second is left.
func TestCookieLifetime(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		name    string
		expires time.Time
		want    time.Duration // approximate, within a second
		ok      bool
	}{
		{"a full session", now.Add(usecase.SessionTTL), usecase.SessionTTL, true},
		{"the expiry is truncated to the second: the slack counts as full", now.Add(usecase.SessionTTL - 500*time.Millisecond), usecase.SessionTTL, true},
		{"cut short by the token that made the link", now.Add(30 * time.Minute), 30 * time.Minute, true},
		{"two seconds left", now.Add(2 * time.Second), 2 * time.Second, true},
		{"under a second left", now.Add(400 * time.Millisecond), 0, false},
		{"already over", now.Add(-time.Hour), 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := cookieLifetime(tc.expires)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.InDelta(t, tc.want.Seconds(), got.Seconds(), 1.5)
			}
		})
	}
}

// webPasskeys.FinishLogin turns the use case's answer into the web's: a refusal
// is ports.ErrLinkRejected, any other failure is passed on, and a success has
// the cookie lifetime of its session.
func TestWebPasskeysFinishLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	boom := errors.New("disk on fire")

	// newPasskeys builds the use cases over mocks; the verifier answers verify.
	newPasskeys := func(t *testing.T, verify func() (domain.Passkey, error), stored domain.Passkey) (webPasskeys, string) {
		t.Helper()
		clock := mocks.NewClock(t)
		clock.EXPECT().Now().RunAndReturn(time.Now).Maybe()
		sessionStore := mocks.NewSessionStore(t)
		sessionStore.EXPECT().Key(mock.Anything).Return([]byte("0123456789abcdef0123456789abcdef"), nil)
		sessionStore.EXPECT().Epoch(mock.Anything).Return(uint64(0), nil).Maybe()
		sessions, err := usecase.NewSessions(ctx, sessionStore, clock, "https://forge.example")
		require.NoError(t, err)

		store := mocks.NewPasskeyStore(t)
		store.EXPECT().UserHandle(mock.Anything).Return([]byte("handle"), nil).Maybe()
		store.EXPECT().Get(mock.Anything, mock.Anything).Return(stored, nil).Maybe()
		store.EXPECT().Update(mock.Anything, mock.Anything).Return(nil).Maybe()
		verifier := mocks.NewPasskeyVerifier(t)
		verifier.EXPECT().BeginLogin().Return([]byte(`{}`), []byte("c"), nil)
		verifier.EXPECT().FinishLogin(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func([]byte, []byte, []byte, func([]byte) (domain.Passkey, error)) (domain.Passkey, error) {
				return verify()
			}).Maybe() // not reached for an unknown ceremony
		pk := webPasskeys{usecase.NewPasskeys(store, verifier, sessions, clock)}
		id, _, err := pk.BeginLogin(ctx, "192.0.2.1")
		require.NoError(t, err)
		return pk, id
	}
	stored := domain.Passkey{ID: []byte("cred-1")}

	t.Run("a refusal is ErrLinkRejected", func(t *testing.T) {
		t.Parallel()
		pk, id := newPasskeys(t, func() (domain.Passkey, error) {
			return domain.Passkey{}, domain.ErrUnauthenticated
		}, stored)
		token, lifetime, err := pk.FinishLogin(ctx, id, []byte(`{}`))
		assert.ErrorIs(t, err, ports.ErrLinkRejected)
		assert.Empty(t, token)
		assert.Zero(t, lifetime)
	})

	t.Run("an unknown ceremony is a refusal too", func(t *testing.T) {
		t.Parallel()
		pk, _ := newPasskeys(t, func() (domain.Passkey, error) { return stored, nil }, stored)
		_, _, err := pk.FinishLogin(ctx, "unknown", []byte(`{}`))
		assert.ErrorIs(t, err, ports.ErrLinkRejected)
	})

	t.Run("another error is passed on as it is", func(t *testing.T) {
		t.Parallel()
		pk, id := newPasskeys(t, func() (domain.Passkey, error) { return domain.Passkey{}, boom }, stored)
		token, _, err := pk.FinishLogin(ctx, id, []byte(`{}`))
		require.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, ports.ErrLinkRejected)
		assert.Empty(t, token)
	})

	t.Run("a success gives the token and the lifetime of the session", func(t *testing.T) {
		t.Parallel()
		pk, id := newPasskeys(t, func() (domain.Passkey, error) { return stored, nil }, stored)
		token, lifetime, err := pk.FinishLogin(ctx, id, []byte(`{}`))
		require.NoError(t, err)
		assert.NotEmpty(t, token)
		assert.InDelta(t, usecase.SessionTTL.Seconds(), lifetime.Seconds(), 1.5)
	})
}
