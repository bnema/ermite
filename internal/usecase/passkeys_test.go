// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

var testHandle = []byte("0123456789abcdef0123456789abcdef")

type passkeyFixture struct {
	*sessionFixture
	store    *mocks.PasskeyStore
	verifier *mocks.PasskeyVerifier
	passkeys *usecase.Passkeys

	mu      sync.Mutex
	stored  map[string]domain.Passkey // by credential id
	ceremon atomic.Int64              // makes the ceremony data of each begin unique
}

func newPasskeyFixture(t *testing.T) *passkeyFixture {
	t.Helper()
	f := &passkeyFixture{
		sessionFixture: newSessionFixture(t),
		store:          mocks.NewPasskeyStore(t),
		verifier:       mocks.NewPasskeyVerifier(t),
		stored:         map[string]domain.Passkey{},
	}
	f.store.EXPECT().UserHandle(mock.Anything).Return(testHandle, nil).Maybe()
	f.store.EXPECT().List(mock.Anything).RunAndReturn(func(context.Context) ([]domain.Passkey, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []domain.Passkey
		for _, p := range f.stored {
			out = append(out, p)
		}
		slices.SortFunc(out, func(a, b domain.Passkey) int { return bytes.Compare(a.ID, b.ID) })
		return out, nil
	}).Maybe()
	f.store.EXPECT().Get(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, id []byte) (domain.Passkey, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if p, ok := f.stored[string(id)]; ok {
			return p, nil
		}
		return domain.Passkey{}, domain.ErrNotFound
	}).Maybe()
	f.store.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p domain.Passkey) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.stored[string(p.ID)]; ok {
			return domain.ErrAlreadyExists
		}
		f.stored[string(p.ID)] = p
		return nil
	}).Maybe()
	f.store.EXPECT().Update(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p domain.Passkey) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.stored[string(p.ID)]; !ok {
			return domain.ErrNotFound
		}
		f.stored[string(p.ID)] = p
		return nil
	}).Maybe()
	f.store.EXPECT().Delete(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, id []byte) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.stored[string(id)]; !ok {
			return domain.ErrNotFound
		}
		delete(f.stored, string(id))
		return nil
	}).Maybe()

	f.verifier.EXPECT().BeginRegistration(mock.Anything, mock.Anything).RunAndReturn(func([]byte, []domain.Passkey) ([]byte, []byte, error) {
		return []byte(`{"publicKey":{}}`), []byte(fmt.Sprintf("register-%d", f.ceremon.Add(1))), nil
	}).Maybe()
	f.verifier.EXPECT().BeginLogin().RunAndReturn(func() ([]byte, []byte, error) {
		return []byte(`{"publicKey":{}}`), []byte(fmt.Sprintf("login-%d", f.ceremon.Add(1))), nil
	}).Maybe()

	f.passkeys = usecase.NewPasskeys(f.store, f.verifier, f.sessions, f.clock)
	return f
}

// add puts a passkey in the store directly.
func (f *passkeyFixture) add(id string) domain.Passkey {
	p := domain.Passkey{ID: []byte(id), Name: id, PublicKey: []byte("k"), CreatedAt: *f.at.Load()}
	f.mu.Lock()
	f.stored[id] = p
	f.mu.Unlock()
	return p
}

func (f *passkeyFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stored)
}

// acceptRegistration makes the verifier accept any registration as the passkey.
func (f *passkeyFixture) acceptRegistration(id string) {
	f.verifier.EXPECT().FinishRegistration(testHandle, mock.Anything, mock.Anything).RunAndReturn(func(_, ceremony, _ []byte) (domain.Passkey, error) {
		if !bytes.HasPrefix(ceremony, []byte("register-")) {
			return domain.Passkey{}, fmt.Errorf("%w: wrong ceremony %q", domain.ErrUnauthenticated, ceremony)
		}
		return domain.Passkey{ID: []byte(id), PublicKey: []byte("pub"), AttestationType: "none", Transports: []string{"internal"}, BackupEligible: true}, nil
	}).Maybe()
}

// acceptLogin makes the verifier accept a login with the stored passkey `id`,
// reporting the sign count.
func (f *passkeyFixture) acceptLogin(id string, signCount uint32) {
	f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_, ceremony, _ []byte, lookup func([]byte) (domain.Passkey, error)) (domain.Passkey, error) {
			if !bytes.HasPrefix(ceremony, []byte("login-")) {
				return domain.Passkey{}, fmt.Errorf("%w: wrong ceremony %q", domain.ErrUnauthenticated, ceremony)
			}
			p, err := lookup([]byte(id))
			if err != nil {
				return domain.Passkey{}, fmt.Errorf("%w: lookup: %v", domain.ErrUnauthenticated, err)
			}
			p.SignCount = signCount
			p.BackupState = true
			return p, nil
		}).Maybe()
}

func ownerAndOthers() map[string]struct {
	p   domain.Principal
	err error
} {
	partial := domain.TokenPrincipal(domain.Token{ID: "p", Scopes: domain.AllScopes()[:8]})
	return map[string]struct {
		p   domain.Principal
		err error
	}{
		"root":          {domain.Root(), nil},
		"owner session": {domain.Owner(), nil},
		"every scope":   {allScopeToken(), nil},
		"scoped token":  {partial, domain.ErrForbidden},
		"anonymous":     {domain.Anonymous(), domain.ErrUnauthenticated},
	}
}

func TestPasskeysAreOwnerOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range ownerAndOthers() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPasskeyFixture(t)
			f.add("a")
			f.acceptRegistration("new")
			check := func(what string, err error) {
				t.Helper()
				if tc.err == nil {
					assert.NoError(t, err, what)
				} else {
					assert.ErrorIs(t, err, tc.err, what)
				}
			}
			_, _, err := f.passkeys.BeginRegistration(ctx, tc.p)
			check("begin registration", err)
			_, err = f.passkeys.List(ctx, tc.p)
			check("list", err)

			// A ceremony begun by the owner cannot be finished by anyone else.
			id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
			require.NoError(t, err)
			_, err = f.passkeys.FinishRegistration(ctx, tc.p, id, "n", []byte("{}"))
			check("finish registration", err)
			if tc.err != nil {
				_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "n", []byte("{}"))
				assert.NoError(t, err, "a refused caller does not burn the ceremony")
			}

			err = f.passkeys.Remove(ctx, tc.p, []byte("a"))
			check("remove", err)
			if tc.err != nil {
				assert.Equal(t, 2, f.count(), "nothing removed")
			}
		})
	}
}

func TestBeginLoginIsAnonymous(t *testing.T) {
	t.Parallel()
	f := newPasskeyFixture(t)
	id, options, err := f.passkeys.BeginLogin(context.Background())
	require.NoError(t, err)
	assert.Len(t, id, 43, "256 bits")
	assert.JSONEq(t, `{"publicKey":{}}`, string(options))
}

func TestRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.acceptRegistration("cred-1")

	id, options, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.NoError(t, err)
	assert.JSONEq(t, `{"publicKey":{}}`, string(options))

	got, err := f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "Pixel 9", []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "Pixel 9", got.Name)
	assert.Equal(t, *f.at.Load(), got.CreatedAt)
	assert.True(t, got.LastUsedAt.IsZero())
	assert.Equal(t, []byte("cred-1"), got.ID)
	list, err := f.passkeys.List(ctx, domain.Owner())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, got, list[0])

	// The ceremony is single use.
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "again", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
}

func TestRegistrationDefaultNameAndBadName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.acceptRegistration("cred-1")

	id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.NoError(t, err)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "evil \u202e name", []byte(`{}`))
	require.ErrorIs(t, err, domain.ErrInvalidText)
	assert.Zero(t, f.count())

	// The verifier was not asked, and the ceremony is still there for a retry.
	got, err := f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "", []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, domain.DefaultPasskeyName, got.Name)
}

func TestRegistrationDuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptRegistration("cred-1")

	id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.NoError(t, err)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "dup", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrAlreadyExists)
	assert.Equal(t, 1, f.count())
}

func TestRegistrationRefusedByVerifier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.verifier.EXPECT().FinishRegistration(testHandle, mock.Anything, mock.Anything).
		Return(domain.Passkey{}, fmt.Errorf("%w: bad origin", domain.ErrUnauthenticated))

	id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.NoError(t, err)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "x", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	assert.Zero(t, f.count())
}

func TestRegistrationRefusesAnIdTooLong(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.verifier.EXPECT().FinishRegistration(testHandle, mock.Anything, mock.Anything).
		Return(domain.Passkey{ID: make([]byte, domain.MaxPasskeyIDLen+1), PublicKey: []byte("k")}, nil)
	id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.NoError(t, err)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "x", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	assert.Zero(t, f.count())
}

// The verifier is told about the passkeys to exclude.
func TestBeginRegistrationExcludesExisting(t *testing.T) {
	t.Parallel()
	f := newPasskeyFixture(t)
	f.add("a")
	f.add("b")
	f.verifier.ExpectedCalls = nil // replace the permissive default
	f.verifier.EXPECT().BeginRegistration(testHandle, mock.MatchedBy(func(ps []domain.Passkey) bool {
		return len(ps) == 2 && string(ps[0].ID) == "a" && string(ps[1].ID) == "b"
	})).Return([]byte(`{}`), []byte("register-x"), nil).Once()
	_, _, err := f.passkeys.BeginRegistration(context.Background(), domain.Owner())
	require.NoError(t, err)
}

func TestMaxPasskeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.acceptRegistration("one-more")
	for i := range domain.MaxPasskeys - 1 {
		f.add(fmt.Sprintf("p%02d", i))
	}
	// A ceremony begun with room for one more.
	id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.NoError(t, err)
	f.add("last")
	assert.Equal(t, domain.MaxPasskeys, f.count())

	// Full: no new ceremony, and the one in flight is refused at the end.
	_, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner())
	assert.ErrorIs(t, err, domain.ErrResourceExhausted)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "x", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrResourceExhausted)
	assert.Equal(t, domain.MaxPasskeys, f.count())

	// Removing one makes room again.
	require.NoError(t, f.passkeys.Remove(ctx, domain.Owner(), []byte("last")))
	_, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner())
	assert.NoError(t, err)
}

func TestCeremonyExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.acceptRegistration("cred-1")
	f.add("login-cred")
	f.acceptLogin("login-cred", 1)

	// One second short of the TTL still works.
	reg, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.NoError(t, err)
	login, _, err := f.passkeys.BeginLogin(ctx)
	require.NoError(t, err)
	f.advance(usecase.PasskeyCeremonyTTL - time.Second)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), reg, "x", []byte(`{}`))
	assert.NoError(t, err)
	_, _, err = f.passkeys.FinishLogin(ctx, login, []byte(`{}`))
	assert.NoError(t, err)

	// At the TTL it is gone, and it stays gone.
	reg, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.NoError(t, err)
	login, _, err = f.passkeys.BeginLogin(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, usecase.PasskeyCeremonyTTL)
	f.advance(usecase.PasskeyCeremonyTTL)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), reg, "x", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	_, _, err = f.passkeys.FinishLogin(ctx, login, []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
}

// Expired ceremonies are swept on begin, so they stop counting.
func TestExpiredRegistrationsDoNotCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	for range usecase.MaxPendingPasskeyRegistrations {
		_, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
		require.NoError(t, err)
	}
	_, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	f.advance(usecase.PasskeyCeremonyTTL)
	_, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner())
	assert.NoError(t, err)
}

func TestPendingRegistrationsAreBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.acceptRegistration("cred-1")
	var ids []string
	for range usecase.MaxPendingPasskeyRegistrations {
		id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
		require.NoError(t, err)
		ids = append(ids, id)
	}
	assert.Len(t, ids, 16)
	_, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.ErrorIs(t, err, domain.ErrResourceExhausted, "the 17th is refused")

	// Pending logins do not use the registrations' room, and a finished
	// registration frees one.
	_, _, err = f.passkeys.BeginLogin(ctx)
	require.NoError(t, err)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), ids[0], "x", []byte(`{}`))
	require.NoError(t, err)
	_, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner())
	assert.NoError(t, err)
}

// Anonymous callers cannot fill the room: the 257th pending login evicts the
// oldest, which then fails; the newest works.
func TestPendingLoginsEvictTheOldest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptLogin("cred-1", 1)

	var ids []string
	for range usecase.MaxPendingPasskeyLogins {
		id, _, err := f.passkeys.BeginLogin(ctx)
		require.NoError(t, err)
		ids = append(ids, id)
		f.advance(time.Millisecond) // the order of creation is the order of time too
	}
	newest, _, err := f.passkeys.BeginLogin(ctx)
	require.NoError(t, err, "the 257th is not refused")

	_, _, err = f.passkeys.FinishLogin(ctx, ids[0], []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated, "the oldest was evicted")
	_, _, err = f.passkeys.FinishLogin(ctx, ids[1], []byte(`{}`))
	assert.NoError(t, err, "the second oldest is still there")
	_, _, err = f.passkeys.FinishLogin(ctx, newest, []byte(`{}`))
	assert.NoError(t, err, "the newest works")
}

// Logins never push out a registration, and registrations never push out a login.
func TestCeremonyKindsAreSeparate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptLogin("cred-1", 1)
	f.acceptRegistration("cred-2")

	reg, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner())
	require.NoError(t, err)
	login, _, err := f.passkeys.BeginLogin(ctx)
	require.NoError(t, err)

	// A registration ceremony does not finish a login, nor the reverse; the
	// attempt burns it.
	_, _, err = f.passkeys.FinishLogin(ctx, reg, []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), login, "x", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	_, _, err = f.passkeys.FinishLogin(ctx, login, []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated, "burned by the mismatch")
	assert.Equal(t, 1, f.count(), "no passkey was added")
}

func TestUnknownCeremony(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	_, _, err := f.passkeys.FinishLogin(ctx, "", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	_, _, err = f.passkeys.FinishLogin(ctx, "nope", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), "nope", "x", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
}

// A passkey login gives a session that Verify accepts, and records the use.
func TestLoginIssuesASession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptLogin("cred-1", 9)

	id, _, err := f.passkeys.BeginLogin(ctx)
	require.NoError(t, err)
	f.advance(10 * time.Second)
	token, exp, err := f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, f.at.Load().Add(usecase.SessionTTL).Truncate(time.Second), exp)

	p, err := f.sessions.Verify(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, "owner", p.Label())
	assert.True(t, p.HasOwnerRights())

	list, err := f.passkeys.List(ctx, domain.Owner())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, uint32(9), list[0].SignCount)
	assert.True(t, list[0].BackupState)
	assert.Equal(t, *f.at.Load(), list[0].LastUsedAt)

	// The ceremony is single use.
	_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
}

func TestLoginRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("unknown passkey", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		f.acceptLogin("ghost", 1)
		id, _, err := f.passkeys.BeginLogin(ctx)
		require.NoError(t, err)
		_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	})

	t.Run("verifier refuses", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		f.add("cred-1")
		f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Passkey{}, fmt.Errorf("%w: bad signature", domain.ErrUnauthenticated))
		id, _, err := f.passkeys.BeginLogin(ctx)
		require.NoError(t, err)
		_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		assert.NotContains(t, err.Error(), "bad signature", "the reason is for logs, not for the caller")
	})

	t.Run("cloned authenticator", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		p := f.add("cred-1")
		f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Passkey{}, domain.ErrPasskeyCloned)
		id, _, err := f.passkeys.BeginLogin(ctx)
		require.NoError(t, err)
		_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		got, err := f.passkeys.List(ctx, domain.Owner())
		require.NoError(t, err)
		assert.Equal(t, []domain.Passkey{p}, got, "nothing recorded")
	})

	t.Run("store failure fails closed and is not an authentication error", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		boom := errors.New("disk on fire")
		f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Passkey{}, boom)
		id, _, err := f.passkeys.BeginLogin(ctx)
		require.NoError(t, err)
		token, _, err := f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
		require.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, domain.ErrUnauthenticated)
		assert.Empty(t, token)
	})

	t.Run("update failure gives no session", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		store := mocks.NewPasskeyStore(t)
		boom := errors.New("disk on fire")
		store.EXPECT().UserHandle(mock.Anything).Return(testHandle, nil)
		store.EXPECT().Update(mock.Anything, mock.Anything).Return(boom)
		verifier := mocks.NewPasskeyVerifier(t)
		verifier.EXPECT().BeginLogin().Return([]byte(`{}`), []byte("login-1"), nil)
		verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Passkey{ID: []byte("cred-1")}, nil)
		pk := usecase.NewPasskeys(store, verifier, f.sessions, f.clock)
		id, _, err := pk.BeginLogin(ctx)
		require.NoError(t, err)
		token, _, err := pk.FinishLogin(ctx, id, []byte(`{}`))
		require.ErrorIs(t, err, boom)
		assert.Empty(t, token)
	})

	t.Run("removed while verifying", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		f.add("cred-1")
		f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
			func(_, _, _ []byte, lookup func([]byte) (domain.Passkey, error)) (domain.Passkey, error) {
				p, err := lookup([]byte("cred-1"))
				require.NoError(t, err)
				f.mu.Lock()
				delete(f.stored, "cred-1") // an owner removes it meanwhile
				f.mu.Unlock()
				return p, nil
			})
		id, _, err := f.passkeys.BeginLogin(ctx)
		require.NoError(t, err)
		token, _, err := f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		assert.Empty(t, token)
	})
}

// Removing a passkey signs every browser out, the passkey's own sessions
// included.
func TestRemoveBumpsTheEpoch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.add("cred-2")
	f.acceptLogin("cred-1", 1)

	id, _, err := f.passkeys.BeginLogin(ctx)
	require.NoError(t, err)
	token, _, err := f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
	require.NoError(t, err)
	_, err = f.sessions.Verify(ctx, token)
	require.NoError(t, err)
	linkToken := func() string {
		tok, _, err := f.sessions.Login(ctx, f.nonce(t))
		require.NoError(t, err)
		return tok
	}()

	require.NoError(t, f.passkeys.Remove(ctx, domain.Owner(), []byte("cred-2")))
	assert.Equal(t, 1, f.count())
	for name, tok := range map[string]string{"passkey session": token, "link session": linkToken} {
		_, err = f.sessions.Verify(ctx, tok)
		assert.ErrorIs(t, err, domain.ErrUnauthenticated, name)
	}

	// A new login works with the epoch it finds now.
	id, _, err = f.passkeys.BeginLogin(ctx)
	require.NoError(t, err)
	token, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
	require.NoError(t, err)
	_, err = f.sessions.Verify(ctx, token)
	assert.NoError(t, err)
}

func TestRemoveUnknownKeepsTheSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	tok, _, err := f.sessions.Login(ctx, f.nonce(t))
	require.NoError(t, err)

	err = f.passkeys.Remove(ctx, domain.Owner(), []byte("ghost"))
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.sessions.Verify(ctx, tok)
	assert.NoError(t, err, "nothing was removed, nobody is signed out")
}

func TestRemoveReportsASignOutFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	boom := errors.New("disk on fire")
	sessionStore := mocks.NewSessionStore(t)
	sessionStore.EXPECT().Key(mock.Anything).Return(testHandle, nil)
	sessionStore.EXPECT().BumpEpoch(mock.Anything).Return(0, boom)
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)).Maybe()
	sessions, err := usecase.NewSessions(ctx, sessionStore, clock, "https://forge.example")
	require.NoError(t, err)

	store := mocks.NewPasskeyStore(t)
	store.EXPECT().Delete(mock.Anything, []byte("cred-1")).Return(nil)
	pk := usecase.NewPasskeys(store, mocks.NewPasskeyVerifier(t), sessions, clock)
	err = pk.Remove(ctx, domain.Owner(), []byte("cred-1"))
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "signing out the web sessions failed")
}

// The use cases are safe for concurrent callers (run with -race).
func TestPasskeysConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptLogin("cred-1", 1)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, err := f.passkeys.BeginLogin(ctx)
			if assert.NoError(t, err) {
				_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
				assert.NoError(t, err)
			}
			_, _, _ = f.passkeys.BeginRegistration(ctx, domain.Owner())
		}()
	}
	wg.Wait()
}
