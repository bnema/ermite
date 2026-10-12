// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
			err = f.passkeys.FinishRegistration(ctx, tc.p, id, []byte("{}"))
			check("finish registration", err)
			if tc.err != nil {
				err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, []byte("{}"))
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

	id, options, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "Pixel 9")
	require.NoError(t, err)
	assert.JSONEq(t, `{"publicKey":{}}`, string(options))

	require.NoError(t, f.passkeys.FinishRegistration(ctx, domain.Owner(), id, []byte(`{}`)))
	list, err := f.passkeys.List(ctx, domain.Owner())
	require.NoError(t, err)
	require.Len(t, list, 1)
	got := list[0]
	assert.Equal(t, "Pixel 9", got.Name, "the name given at begin")
	assert.Equal(t, *f.at.Load(), got.CreatedAt)
	assert.True(t, got.LastUsedAt.IsZero())
	assert.Equal(t, []byte("cred-1"), got.ID)

	// The ceremony is single use.
	err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
}

// An empty name is the default one, and each ceremony keeps the name it began
// with, whatever else is pending.
func TestRegistrationNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	// The credential id is the ceremony data, so that two ceremonies finish as
	// two different passkeys.
	f.verifier.EXPECT().FinishRegistration(testHandle, mock.Anything, mock.Anything).RunAndReturn(
		func(_, ceremony, _ []byte) (domain.Passkey, error) {
			return domain.Passkey{ID: bytes.Clone(ceremony), PublicKey: []byte("pub")}, nil
		})
	unnamed, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	require.NoError(t, err)
	named, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "Second")
	require.NoError(t, err)
	// Finished in the other order.
	require.NoError(t, f.passkeys.FinishRegistration(ctx, domain.Owner(), named, []byte(`{}`)))
	require.NoError(t, f.passkeys.FinishRegistration(ctx, domain.Owner(), unnamed, []byte(`{}`)))

	names := map[string]string{}
	list, err := f.passkeys.List(ctx, domain.Owner())
	require.NoError(t, err)
	for _, p := range list {
		names[string(p.ID)] = p.Name
	}
	assert.ElementsMatch(t, []string{"Second", domain.DefaultPasskeyName}, []string{names["register-2"], names["register-1"]})
	assert.Equal(t, "Second", names["register-2"])
	assert.Equal(t, domain.DefaultPasskeyName, names["register-1"])
}

// What FinishRegistration refuses, and what is left in the store each time.
func TestRegistrationRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		existing []string // passkeys already stored
		verifier func(f *passkeyFixture)
		wantErr  error
		wantKept int // passkeys in the store afterwards
	}{
		{
			name:     "duplicate credential",
			existing: []string{"cred-1"},
			verifier: func(f *passkeyFixture) { f.acceptRegistration("cred-1") },
			wantErr:  domain.ErrAlreadyExists, wantKept: 1,
		},
		{
			name: "refused by the verifier",
			verifier: func(f *passkeyFixture) {
				f.verifier.EXPECT().FinishRegistration(testHandle, mock.Anything, mock.Anything).
					Return(domain.Passkey{}, fmt.Errorf("%w: bad origin", domain.ErrUnauthenticated))
			},
			wantErr: domain.ErrUnauthenticated, wantKept: 0,
		},
		{
			name: "credential id too long",
			verifier: func(f *passkeyFixture) {
				f.verifier.EXPECT().FinishRegistration(testHandle, mock.Anything, mock.Anything).
					Return(domain.Passkey{ID: make([]byte, domain.MaxPasskeyIDLen+1), PublicKey: []byte("k")}, nil)
			},
			wantErr: domain.ErrUnauthenticated, wantKept: 0,
		},
		{
			name: "empty credential id",
			verifier: func(f *passkeyFixture) {
				f.verifier.EXPECT().FinishRegistration(testHandle, mock.Anything, mock.Anything).
					Return(domain.Passkey{PublicKey: []byte("k")}, nil)
			},
			wantErr: domain.ErrUnauthenticated, wantKept: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newPasskeyFixture(t)
			for _, id := range tc.existing {
				f.add(id)
			}
			tc.verifier(f)
			id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "x")
			require.NoError(t, err)
			err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, []byte(`{}`))
			assert.ErrorIs(t, err, tc.wantErr)
			assert.Equal(t, tc.wantKept, f.count())
		})
	}
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
	err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, []byte(`{}`))
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
	f.advance(domain.PasskeyCeremonyTTL - time.Second)
	err = f.passkeys.FinishRegistration(ctx, domain.Owner(), reg, []byte(`{}`))
	assert.NoError(t, err)
	_, _, err = f.passkeys.FinishLogin(ctx, login, []byte(`{}`))
	assert.NoError(t, err)

	// At the TTL it is gone, and it stays gone.
	reg, _, err = f.passkeys.BeginRegistration(ctx, domain.Owner(), "")
	require.NoError(t, err)
	login, _, err = f.passkeys.BeginLogin(ctx, "192.0.2.1")
	require.NoError(t, err)
	f.advance(domain.PasskeyCeremonyTTL)
	err = f.passkeys.FinishRegistration(ctx, domain.Owner(), reg, []byte(`{}`))
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
	f.advance(domain.PasskeyCeremonyTTL)
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
	err = f.passkeys.FinishRegistration(ctx, domain.Owner(), ids[0], []byte(`{}`))
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
	err = f.passkeys.FinishRegistration(ctx, domain.Owner(), login, []byte(`{}`))
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
	err = f.passkeys.FinishRegistration(ctx, domain.Owner(), "nope", []byte(`{}`))
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

// logEntries parses the JSON lines of the buffer.
func logEntries(t *testing.T, logs *syncLog) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		out = append(out, m)
	}
	return out
}

// The reason of a refusal is logged at info, never answered, and holds no
// secret: neither the response bytes nor a key.
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
		assert.NotContains(t, err.Error(), "origin mismatch", "the caller learns nothing")
		entries := logEntries(t, logs)
		require.Len(t, entries, 1)
		assert.Equal(t, "info", entries[0]["level"])
		assert.Contains(t, entries[0]["error"], "origin mismatch")
		assert.NotContains(t, logs.String(), "response-bytes")
	})

	t.Run("sign-in with an unknown ceremony", func(t *testing.T) {
		t.Parallel()
		ctx, logs := logCtx(t)
		f := newPasskeyFixture(t)
		_, _, err := f.passkeys.FinishLogin(ctx, "nope", []byte(`{"secret":"response-bytes"}`))
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		entries := logEntries(t, logs)
		require.Len(t, entries, 1)
		assert.Equal(t, "info", entries[0]["level"])
		assert.NotContains(t, logs.String(), "response-bytes")
	})

	t.Run("registration", func(t *testing.T) {
		t.Parallel()
		ctx, logs := logCtx(t)
		f := newPasskeyFixture(t)
		f.verifier.EXPECT().FinishRegistration(testHandle, mock.Anything, mock.Anything).
			Return(domain.Passkey{}, fmt.Errorf("%w: passkey registration: user verification missing", domain.ErrUnauthenticated))
		id, _, err := f.passkeys.BeginRegistration(ctx, domain.Owner(), "x")
		require.NoError(t, err)
		err = f.passkeys.FinishRegistration(ctx, domain.Owner(), id, []byte(`{"secret":"response-bytes"}`))
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		entries := logEntries(t, logs)
		require.Len(t, entries, 1)
		assert.Equal(t, "info", entries[0]["level"])
		assert.Contains(t, entries[0]["error"], "user verification missing")
		assert.NotContains(t, logs.String(), "response-bytes")

		// An unknown ceremony is logged too.
		err = f.passkeys.FinishRegistration(ctx, domain.Owner(), "unknown", []byte(`{}`))
		require.ErrorIs(t, err, domain.ErrUnauthenticated)
		entries = logEntries(t, logs)
		require.Len(t, entries, 2)
		assert.Equal(t, "info", entries[1]["level"])
	})
}

// A counter that does not advance on a passkey that has one is refused, and the
// warning names the passkey by its short reference. The library hands back the
// stored counter when the received one is not higher, which is what the verifier
// returns here.
func TestCloneIsRefusedAndNamed(t *testing.T) {
	t.Parallel()
	ctx, logs := logCtx(t)
	f := newPasskeyFixture(t)
	p := f.add("cred-1")
	p.SignCount = 5
	f.mu.Lock()
	f.stored["cred-1"] = p
	f.mu.Unlock()
	f.verifier.EXPECT().FinishLogin(testHandle, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_, _, _ []byte, lookup func([]byte) (domain.Passkey, error)) (domain.Passkey, error) {
			return lookup([]byte("cred-1")) // SignCount stays 5
		})
	id, _, err := f.passkeys.BeginLogin(ctx, "192.0.2.1")
	require.NoError(t, err)
	token, _, err := f.passkeys.FinishLogin(ctx, id, []byte(`{}`))
	require.ErrorIs(t, err, domain.ErrUnauthenticated)
	assert.Empty(t, token, "no session")
	assert.Equal(t, 1, f.count())
	f.mu.Lock()
	assert.True(t, f.stored["cred-1"].LastUsedAt.IsZero(), "nothing was recorded")
	f.mu.Unlock()

	sum := sha256.Sum256([]byte("cred-1"))
	ref := hex.EncodeToString(sum[:])[:8]
	entries := logEntries(t, logs)
	require.Len(t, entries, 1, "one log line for the refusal")
	assert.Equal(t, "warn", entries[0]["level"])
	assert.Equal(t, ref, entries[0]["passkey"])
	assert.NotContains(t, logs.String(), "cred-1", "the credential id itself is never logged")
}

// Two logins verified against the same stored counter cannot both
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

// A client at its cap begins a login while the store is full. Its own eviction
// makes the room the new login needs, so exactly one ceremony is dropped, the
// client's own oldest: the global rule must not take a second one, from anyone.
func TestFullClientOnAFullStoreLosesOnlyItsOwnOldest(t *testing.T) {
	t.Parallel()
	f := newPasskeyFixture(t)
	f.add("cred-1")
	f.acceptLogin("cred-1", 1)
	perClient := usecase.MaxPendingPasskeyLoginsPerClient

	// Seven clients with one login each, the flooder with a full cap, then full
	// clients and a last partial one until the store holds exactly its cap.
	small := make([]string, 0, 7)
	for i := range 7 {
		small = append(small, f.loginIDs(t, fmt.Sprintf("10.9.0.%d", i), 1)[0])
	}
	flooder := f.loginIDs(t, "192.0.2.1", perClient)
	total := len(small) + len(flooder)
	var others []string
	for n := 0; total+perClient <= usecase.MaxPendingPasskeyLogins; n++ {
		others = append(others, f.loginIDs(t, fmt.Sprintf("10.0.%d.1", n), perClient)...)
		total += perClient
	}
	if rest := usecase.MaxPendingPasskeyLogins - total; rest > 0 {
		others = append(others, f.loginIDs(t, "10.8.0.1", rest)...)
		total += rest
	}
	require.Equal(t, usecase.MaxPendingPasskeyLogins, total, "the store is full")

	newest := f.loginIDs(t, "192.0.2.1", 1)[0]

	assert.False(t, f.finishes(flooder[0]), "the flooder lost its own oldest")
	for _, id := range append(append(flooder[1:], small...), others...) {
		assert.True(t, f.finishes(id), "nobody else lost a ceremony")
	}
	assert.True(t, f.finishes(newest), "the new login works")
}

// A failure of the store while removing a passkey is returned as it is, and
// nobody is signed out for it.
func TestRemoveReturnsAStoreError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPasskeyFixture(t)
	boom := errors.New("disk on fire")
	store := mocks.NewPasskeyStore(t)
	store.EXPECT().Delete(mock.Anything, []byte("cred-1")).Return(boom)
	// The session store is a strict mock: a BumpEpoch would fail the test.
	pk := usecase.NewPasskeys(store, mocks.NewPasskeyVerifier(t), f.sessions, f.clock)
	tok, _, err := f.sessions.Login(ctx, f.nonce(t))
	require.NoError(t, err)

	err = pk.Remove(ctx, domain.Owner(), []byte("cred-1"))
	require.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, domain.ErrNotFound)
	_, err = f.sessions.Verify(ctx, tok)
	assert.NoError(t, err, "the epoch did not move: the sessions are still valid")
}
