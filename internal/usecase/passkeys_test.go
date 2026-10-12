// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
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

// acceptLogin makes the verifier accept a login with the stored passkey `id`.
// first is the counter of the first sign-in; like a real authenticator, each
// later one reports a higher counter. 0 means a synced passkey: always 0.
func (f *passkeyFixture) acceptLogin(id string, first uint32) {
	var next atomic.Uint32
	next.Store(first)
	f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_, ceremony, _ []byte, lookup func([]byte) (domain.Passkey, error)) (domain.Passkey, error) {
			if !bytes.HasPrefix(ceremony, []byte("login-")) {
				return domain.Passkey{}, fmt.Errorf("%w: wrong ceremony %q", domain.ErrUnauthenticated, ceremony)
			}
			p, err := lookup([]byte(id))
			if err != nil {
				return domain.Passkey{}, fmt.Errorf("%w: lookup: %v", domain.ErrUnauthenticated, err)
			}
			if first != 0 {
				p.SignCount = next.Add(1) - 1
			}
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
			_, _, err := f.passkeys.BeginRegistration(ctx, tc.p, "")
			check("begin registration", err)
			_, err = f.passkeys.List(ctx, tc.p)
			check("list", err)

			// A ceremony begun by the owner cannot be finished by anyone else.
			id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
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
	id, options, err := f.passkeys.BeginLogin(context.Background(), "192.0.2.1")
	require.NoError(t, err)
	assert.Len(t, id, 43, "256 bits")
	assert.JSONEq(t, `{"publicKey":{}}`, string(options))
}

func TestRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.acceptRegistration("cred-1")

	id, options, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
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

	id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
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

	id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
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

	id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
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
	id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
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
	_, _, err := f.passkeys.BeginRegistration(context.Background(), domain.Owner(), "")
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
	id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	require.NoError(t, err)
	f.add("last")
	assert.Equal(t, domain.MaxPasskeys, f.count())

	// Full: no new ceremony, and the one in flight is refused at the end.
	_, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	assert.ErrorIs(t, err, domain.ErrResourceExhausted)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "x", []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrResourceExhausted)
	assert.Equal(t, domain.MaxPasskeys, f.count())

	// Removing one makes room again.
	require.NoError(t, f.passkeys.Remove(ctx, domain.Owner(), []byte("last")))
	_, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
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
	reg, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	require.NoError(t, err)
	login, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
	require.NoError(t, err)
	f.advance(usecase.PasskeyCeremonyTTL - time.Second)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), reg, "x", []byte(`{}`))
	assert.NoError(t, err)
	_, _, err = f.passkeys.FinishLogin(ctx, login, []byte(`{}`))
	assert.NoError(t, err)

	// At the TTL it is gone, and it stays gone.
	reg, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	require.NoError(t, err)
	login, _, err = f.passkeys.BeginLogin(ctx, "192.0.2.1")
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
		_, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
		require.NoError(t, err)
	}
	_, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	f.advance(usecase.PasskeyCeremonyTTL)
	_, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	assert.NoError(t, err)
}

func TestPendingRegistrationsAreBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.acceptRegistration("cred-1")
	var ids []string
	for range usecase.MaxPendingPasskeyRegistrations {
		id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
		require.NoError(t, err)
		ids = append(ids, id)
	}
	assert.Len(t, ids, 16)
	_, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	require.ErrorIs(t, err, domain.ErrResourceExhausted, "the 17th is refused")

	// Pending logins do not use the registrations' room, and a finished
	// registration frees one.
	_, _, err = f.passkeys.BeginLogin(ctx, "192.0.2.1")
	require.NoError(t, err)
	_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), ids[0], "x", []byte(`{}`))
	require.NoError(t, err)
	_, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	assert.NoError(t, err)
}

// loginIDs begins n logins for the client, a millisecond apart so that the order
// of creation is the order of time too.
func (f *passkeyFixture) loginIDs(t *testing.T, client string, n int) []string {
	t.Helper()
	var ids []string
	for range n {
		id, _, err := f.passkeys.BeginLogin(context.Background(), client)
		require.NoError(t, err, "a login is never refused")
		ids = append(ids, id)
		f.advance(time.Millisecond)
	}
	return ids
}

// finishes tells whether the ceremony still opens a session.
func (f *passkeyFixture) finishes(id string) bool {
	_, _, err := f.passkeys.FinishLogin(context.Background(), id, []byte(`{}`))
	return err == nil
}

// A client keeps at most 8 logins waiting: the 9th costs it its own oldest.
func TestPendingLoginsPerClientCap(t *testing.T) {
	t.Parallel()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptLogin("cred-1", 1)

	other := f.loginIDs(t, "198.51.100.7", 1)[0]
	ids := f.loginIDs(t, "192.0.2.1", usecase.MaxPendingPasskeyLoginsPerClient)
	assert.Equal(t, 8, usecase.MaxPendingPasskeyLoginsPerClient)
	newest := f.loginIDs(t, "192.0.2.1", 1)[0]

	assert.False(t, f.finishes(ids[0]), "the 9th evicted this client's oldest")
	assert.True(t, f.finishes(ids[1]), "the next oldest is still there")
	assert.True(t, f.finishes(newest), "the newest works")
	assert.True(t, f.finishes(other), "another client's ceremony is untouched")
}

// One address flooding can never evict another address's ceremony.
func TestLoginFloodDoesNotEvictAnotherClient(t *testing.T) {
	t.Parallel()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptLogin("cred-1", 1)

	owner := f.loginIDs(t, "198.51.100.7", 1)[0]
	f.loginIDs(t, "192.0.2.1", 2*usecase.MaxPendingPasskeyLogins)
	assert.True(t, f.finishes(owner), "the owner's ceremony from another address still works")
}

// Over the global cap, the client with the most waiting loses its oldest; a
// client with one ceremony is never the victim.
func TestPendingLoginsGlobalOverflow(t *testing.T) {
	t.Parallel()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptLogin("cred-1", 1)

	owner := f.loginIDs(t, "198.51.100.7", 1)[0]
	perClient := usecase.MaxPendingPasskeyLoginsPerClient
	var first, second []string
	n := 0
	for total := 1; total+perClient <= usecase.MaxPendingPasskeyLogins; total += perClient {
		ids := f.loginIDs(t, fmt.Sprintf("10.0.%d.1", n), perClient)
		if n == 0 {
			first = ids
		}
		if n == 1 {
			second = ids
		}
		n++
	}
	// 1 + 31*8 = 249 waiting; top up to the cap with a client below the cap.
	small := f.loginIDs(t, "10.1.0.1", usecase.MaxPendingPasskeyLogins-1-n*perClient)
	require.Len(t, small, 7)

	// The 257th comes from a client that has none: nobody is refused, the
	// oldest of a client with 8 waiting goes (the one that waited longest).
	fresh := f.loginIDs(t, "203.0.113.9", 1)[0]
	assert.False(t, f.finishes(first[0]), "the oldest of the fullest, oldest client was evicted")
	assert.True(t, f.finishes(first[1]))
	assert.True(t, f.finishes(second[0]), "other full clients keep theirs")
	assert.True(t, f.finishes(owner), "a client with one ceremony is never the victim")
	assert.True(t, f.finishes(small[0]), "nor one below the others")
	assert.True(t, f.finishes(fresh), "the new login works")
}

// IPv6 clients share a cap per /64; an IPv4-mapped address is the IPv4 one.
func TestPendingLoginsClientKey(t *testing.T) {
	t.Parallel()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptLogin("cred-1", 1)
	perClient := usecase.MaxPendingPasskeyLoginsPerClient

	// Different addresses of one /64 count together.
	ids := f.loginIDs(t, "2001:db8:1:2::1", perClient)
	next := f.loginIDs(t, "2001:db8:1:2:ffff:ffff:ffff:ffff", 1)[0]
	assert.False(t, f.finishes(ids[0]), "the same /64: the 9th evicted the oldest")
	assert.True(t, f.finishes(next))

	// Another /64 has its own cap.
	other := f.loginIDs(t, "2001:db8:1:3::1", perClient)
	for _, id := range other {
		assert.True(t, f.finishes(id), "a different /64 is another client")
	}

	// ::ffff:192.0.2.1 and 192.0.2.1 are one client.
	v4 := f.loginIDs(t, "192.0.2.1", perClient)
	f.loginIDs(t, "::ffff:192.0.2.1", 1)
	assert.False(t, f.finishes(v4[0]))
	// Two IPv4 addresses of one /24 are two clients.
	a := f.loginIDs(t, "192.0.2.50", perClient)
	f.loginIDs(t, "192.0.2.51", perClient)
	assert.True(t, f.finishes(a[0]))
}

// Logins never push out a registration, and registrations never push out a login.
func TestCeremonyKindsAreSeparate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptLogin("cred-1", 1)
	f.acceptRegistration("cred-2")

	reg, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	require.NoError(t, err)
	login, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
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

	id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
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

// logCtx is a context whose logger writes JSON lines into the buffer.
func logCtx(t *testing.T) (context.Context, *syncLog) {
	t.Helper()
	buf := &syncLog{}
	log := zerowrap.New(zerowrap.Config{Level: "trace", Format: "json", Output: buf})
	return zerowrap.WithCtx(context.Background(), log), buf
}

type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// GO-001: the reason of a refusal is logged (info), never answered, and holds
// no key material.
func TestRefusalsAreLogged(t *testing.T) {
	t.Parallel()

	t.Run("sign-in", func(t *testing.T) {
		t.Parallel()
		ctx, logs := logCtx(t)
		f := newPasskeyFixture(t)
		f.add("cred-1")
		f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Passkey{}, fmt.Errorf("%w: passkey login: origin mismatch", domain.ErrUnauthenticated))
		id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
		require.NoError(t, err)
		_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{"secret":"response-bytes"}`))
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		out := logs.String()
		assert.Contains(t, out, `"message":"passkey sign-in refused"`)
		assert.Contains(t, out, `"level":"info"`)
		assert.Contains(t, out, "origin mismatch")
		assert.NotContains(t, out, "response-bytes")
	})

	t.Run("sign-in with an unknown ceremony", func(t *testing.T) {
		t.Parallel()
		ctx, logs := logCtx(t)
		f := newPasskeyFixture(t)
		_, _, err := f.passkeys.FinishLogin(ctx, "nope", []byte(`{}`))
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		assert.Contains(t, logs.String(), "passkey sign-in refused")
	})

	t.Run("registration", func(t *testing.T) {
		t.Parallel()
		ctx, logs := logCtx(t)
		f := newPasskeyFixture(t)
		f.verifier.EXPECT().FinishRegistration(testHandle, mock.Anything, mock.Anything).
			Return(domain.Passkey{}, fmt.Errorf("%w: passkey registration: user verification missing", domain.ErrUnauthenticated))
		id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
		require.NoError(t, err)
		_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, "x", []byte(`{"secret":"response-bytes"}`))
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		out := logs.String()
		assert.Contains(t, out, `"message":"passkey registration refused"`)
		assert.Contains(t, out, "user verification missing")
		assert.NotContains(t, out, "response-bytes")

		_, err = f.passkeys.FinishRegistration(ctx, domain.Owner(), "unknown", "x", []byte(`{}`))
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		assert.Equal(t, 2, strings.Count(logs.String(), "passkey registration refused"))
	})
}

// GO-002: the clone warning says which passkey, by its short reference.
func TestCloneWarningNamesThePasskey(t *testing.T) {
	t.Parallel()
	ctx, logs := logCtx(t)
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_, _, _ []byte, lookup func([]byte) (domain.Passkey, error)) (domain.Passkey, error) {
			_, err := lookup([]byte("cred-1"))
			require.NoError(t, err)
			return domain.Passkey{}, domain.ErrPasskeyCloned
		})
	id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
	require.NoError(t, err)
	_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
	require.ErrorIs(t, err, domain.ErrUnauthenticated)

	sum := sha256.Sum256([]byte("cred-1"))
	ref := hex.EncodeToString(sum[:])[:8]
	out := logs.String()
	assert.Contains(t, out, `"level":"warn"`)
	assert.Contains(t, out, `"passkey":"`+ref+`"`)
	assert.Contains(t, out, "may be cloned")
	assert.NotContains(t, out, "cred-1", "the credential id itself is never logged")
}

// GO-003: two logins verified against the same stored counter cannot both
// succeed. The verifier saw counter 5; by the time the second one writes, the
// first has stored 6.
func TestLoginRefusesACounterThatDidNotAdvance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("another login moved the counter meanwhile", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		stored := f.add("cred-1")
		stored.SignCount = 5
		f.mu.Lock()
		f.stored["cred-1"] = stored
		f.mu.Unlock()
		f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
			func(_, _, _ []byte, lookup func([]byte) (domain.Passkey, error)) (domain.Passkey, error) {
				p, err := lookup([]byte("cred-1"))
				require.NoError(t, err)
				p.SignCount = 6 // what the response reports, against the 5 it was shown
				// A concurrent login stores 6 before this one gets the lock.
				f.mu.Lock()
				f.stored["cred-1"] = p
				f.mu.Unlock()
				return p, nil
			})
		id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
		require.NoError(t, err)
		token, _, err := f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		assert.Empty(t, token, "no session")
		f.mu.Lock()
		assert.True(t, f.stored["cred-1"].LastUsedAt.IsZero(), "nothing was recorded")
		f.mu.Unlock()
	})

	t.Run("a counter that advances is accepted", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		p := f.add("cred-1")
		p.SignCount = 5
		f.mu.Lock()
		f.stored["cred-1"] = p
		f.mu.Unlock()
		f.acceptLogin("cred-1", 6)
		id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
		require.NoError(t, err)
		_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
		assert.NoError(t, err)
	})

	t.Run("a synced passkey reports 0 and is exempt", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		f.add("cred-1") // stored counter 0
		f.acceptLogin("cred-1", 0)
		for range 2 {
			id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
			require.NoError(t, err)
			_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
			assert.NoError(t, err)
		}
	})

	t.Run("equal counters are refused when the stored one is not 0", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		p := f.add("cred-1")
		p.SignCount = 5
		f.mu.Lock()
		f.stored["cred-1"] = p
		f.mu.Unlock()
		f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Passkey{ID: []byte("cred-1"), SignCount: 5}, nil)
		id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
		require.NoError(t, err)
		_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	})
}

func TestLoginRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("unknown passkey", func(t *testing.T) {
		t.Parallel()
		f := newPasskeyFixture(t)
		f.acceptLogin("ghost", 1)
		id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
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
		id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
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
		id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
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
		id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
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
		store.EXPECT().Get(mock.Anything, []byte("cred-1")).Return(domain.Passkey{ID: []byte("cred-1")}, nil)
		store.EXPECT().Update(mock.Anything, mock.Anything).Return(boom)
		verifier := mocks.NewPasskeyVerifier(t)
		verifier.EXPECT().BeginLogin().Return([]byte(`{}`), []byte("login-1"), nil)
		verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Passkey{ID: []byte("cred-1")}, nil)
		pk := usecase.NewPasskeys(store, verifier, f.sessions, f.clock)
		id, _, err := pk.BeginLogin(ctx, "192.0.2.1")
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
		id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
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

	id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
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
	id, _, err = f.passkeys.BeginLogin(ctx, "192.0.2.1")
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
	f.acceptLogin("cred-1", 0) // synced: counters racing in order would be refused, rightly
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
			if assert.NoError(t, err) {
				_, _, err = f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
				assert.NoError(t, err)
			}
			_, _, _ = f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
		}()
	}
	wg.Wait()
}

// TS-001: a name that breaks the rules is refused before any ceremony starts,
// and does not use up one of the pending registrations.
func TestBeginRegistrationChecksTheName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	for _, name := range []string{"evil \u202e name", "a\x00b", strings.Repeat("a", domain.MaxPasskeyNameLen+1), "two\nlines", "\xff"} {
		_, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), name)
		assert.ErrorIs(t, err, domain.ErrInvalidText, "%q", name)
	}
	// None of those counted as a pending registration.
	for range usecase.MaxPendingPasskeyRegistrations {
		_, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "ok")
		require.NoError(t, err)
	}
	_, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	assert.ErrorIs(t, err, domain.ErrResourceExhausted)
}
