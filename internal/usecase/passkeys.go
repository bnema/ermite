// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Owner passkeys (ADR 0026). A ceremony is a registration or a login that the
// browser carries out between two requests; its data lives in memory only.
const (
	// PasskeyCeremonyTTL is how long a ceremony may take.
	PasskeyCeremonyTTL = 2 * time.Minute
	// MaxPendingPasskeyRegistrations bounds the registrations that wait to be
	// finished. Only the owner starts one.
	MaxPendingPasskeyRegistrations = 16
	// MaxPendingPasskeyLogins bounds the logins that wait to be finished.
	// Anyone may start one, so on overflow the oldest is dropped instead of
	// refusing: anonymous callers cannot lock the owner out.
	MaxPendingPasskeyLogins = 256

	// ceremonyIDBytes is the size of a ceremony identifier: 256 bits.
	ceremonyIDBytes = 32
)

type ceremonyKind uint8

const (
	ceremonyRegister ceremonyKind = iota + 1
	ceremonyLogin
)

// ceremony is a begun registration or login.
type ceremony struct {
	kind    ceremonyKind
	data    []byte // opaque, from the verifier
	expires time.Time
	seq     uint64 // creation order
}

// Passkeys are the use cases of the owner's passkeys: registering, signing in,
// listing and removing them.
type Passkeys struct {
	store    ports.PasskeyStore
	verifier ports.PasskeyVerifier
	sessions *Sessions
	clock    ports.Clock

	// writeMu serializes the writes to the store (Create, Update, Delete) and
	// the session they make or void, so that a removed passkey cannot sign in.
	writeMu sync.Mutex

	mu         sync.Mutex
	ceremonies map[[sha256.Size]byte]ceremony // by hash of the identifier
	seq        uint64
}

var _ ports.PasskeyService = (*Passkeys)(nil)

// NewPasskeys builds the passkey use cases. Sessions are issued and voided
// through sessions.
func NewPasskeys(store ports.PasskeyStore, verifier ports.PasskeyVerifier, sessions *Sessions, clock ports.Clock) *Passkeys {
	return &Passkeys{store: store, verifier: verifier, sessions: sessions, clock: clock, ceremonies: map[[sha256.Size]byte]ceremony{}}
}

// begin keeps a ceremony and returns its identifier. It sweeps the expired
// ones first. A registration over the limit gives domain.ErrResourceExhausted;
// a login over it drops the oldest login.
func (s *Passkeys) begin(kind ceremonyKind, data []byte) (string, error) {
	var raw [ceremonyIDBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate passkey ceremony: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(raw[:])
	now := s.clock.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	pending := 0
	var oldest [sha256.Size]byte
	var oldestSeq uint64
	for h, c := range s.ceremonies {
		if !now.Before(c.expires) {
			delete(s.ceremonies, h)
			continue
		}
		if c.kind != kind {
			continue
		}
		if pending == 0 || c.seq < oldestSeq {
			oldest, oldestSeq = h, c.seq
		}
		pending++
	}
	switch kind {
	case ceremonyRegister:
		if pending >= MaxPendingPasskeyRegistrations {
			return "", fmt.Errorf("%w: %d passkey registrations are already pending, finish one or wait two minutes", domain.ErrResourceExhausted, MaxPendingPasskeyRegistrations)
		}
	case ceremonyLogin:
		if pending >= MaxPendingPasskeyLogins {
			delete(s.ceremonies, oldest)
		}
	}
	s.seq++
	s.ceremonies[sha256.Sum256([]byte(id))] = ceremony{kind: kind, data: data, expires: now.Add(PasskeyCeremonyTTL), seq: s.seq}
	return id, nil
}

// take removes the ceremony and returns its data. It is single use, also when
// the ceremony expired or is of another kind. ok is false for an unknown,
// expired or mismatched ceremony.
func (s *Passkeys) take(id string, kind ceremonyKind) (data []byte, ok bool) {
	h := sha256.Sum256([]byte(id))
	s.mu.Lock()
	c, found := s.ceremonies[h]
	delete(s.ceremonies, h)
	s.mu.Unlock()
	if !found || c.kind != kind || !s.clock.Now().Before(c.expires) {
		return nil, false
	}
	return c.data, true
}

// BeginRegistration starts adding a passkey. Only the owner may. It gives
// domain.ErrResourceExhausted when MaxPasskeys passkeys exist or too many
// registrations wait. It returns the ceremony identifier and the options for
// the browser.
func (s *Passkeys) BeginRegistration(ctx context.Context, p domain.Principal) (string, []byte, error) {
	if err := requireOwner(p); err != nil {
		return "", nil, err
	}
	existing, err := s.store.List(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("list passkeys: %w", err)
	}
	if len(existing) >= domain.MaxPasskeys {
		return "", nil, fmt.Errorf("%w: %d passkeys are registered, remove one first", domain.ErrResourceExhausted, domain.MaxPasskeys)
	}
	handle, err := s.store.UserHandle(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("load passkey user handle: %w", err)
	}
	options, data, err := s.verifier.BeginRegistration(handle, existing)
	if err != nil {
		return "", nil, err
	}
	id, err := s.begin(ceremonyRegister, data)
	if err != nil {
		return "", nil, err
	}
	return id, options, nil
}

// FinishRegistration checks the browser's response and stores the passkey
// under name; an empty name means domain.DefaultPasskeyName. A ceremony that is
// unknown, expired, used or of the wrong kind, and a response the verifier
// refuses, give domain.ErrUnauthenticated; a bad name matches
// domain.ErrInvalidText; a credential already registered gives
// domain.ErrAlreadyExists; too many passkeys, domain.ErrResourceExhausted.
func (s *Passkeys) FinishRegistration(ctx context.Context, p domain.Principal, ceremonyID, name string, response []byte) (domain.Passkey, error) {
	if err := requireOwner(p); err != nil {
		return domain.Passkey{}, err
	}
	if name == "" {
		name = domain.DefaultPasskeyName
	}
	parsed, err := domain.ParsePasskeyName(name) // before take: a bad name does not burn the ceremony
	if err != nil {
		return domain.Passkey{}, fmt.Errorf("passkey name: %w", err)
	}
	data, ok := s.take(ceremonyID, ceremonyRegister)
	if !ok {
		return domain.Passkey{}, fmt.Errorf("%w: passkey registration expired or unknown, start again", domain.ErrUnauthenticated)
	}
	handle, err := s.store.UserHandle(ctx)
	if err != nil {
		return domain.Passkey{}, fmt.Errorf("load passkey user handle: %w", err)
	}
	pk, err := s.verifier.FinishRegistration(handle, data, response)
	if err != nil {
		return domain.Passkey{}, err
	}
	if n := len(pk.ID); n == 0 || n > domain.MaxPasskeyIDLen {
		return domain.Passkey{}, fmt.Errorf("%w: credential id has %d bytes", domain.ErrUnauthenticated, n)
	}
	pk.Name = parsed.String()
	pk.CreatedAt = s.clock.Now().UTC()
	pk.LastUsedAt = time.Time{}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	existing, err := s.store.List(ctx)
	if err != nil {
		return domain.Passkey{}, fmt.Errorf("list passkeys: %w", err)
	}
	if len(existing) >= domain.MaxPasskeys {
		return domain.Passkey{}, fmt.Errorf("%w: %d passkeys are registered, remove one first", domain.ErrResourceExhausted, domain.MaxPasskeys)
	}
	if err := s.store.Create(ctx, pk); err != nil {
		return domain.Passkey{}, err
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("passkey", passkeyRef(pk.ID)).Msg("passkey added")
	return pk, nil
}

// BeginLogin starts a passkey sign-in. Anonymous callers may. It returns the
// ceremony identifier and the options for the browser.
func (s *Passkeys) BeginLogin(context.Context) (string, []byte, error) {
	options, data, err := s.verifier.BeginLogin()
	if err != nil {
		return "", nil, err
	}
	id, err := s.begin(ceremonyLogin, data)
	if err != nil {
		return "", nil, err
	}
	return id, options, nil
}

// FinishLogin checks the browser's response and opens the owner's session,
// like a login link does. Whatever the reason of a refusal (unknown or
// expired ceremony, unknown passkey, bad signature, missing user verification,
// cloned authenticator), it gives domain.ErrUnauthenticated; a failure of the
// store is returned as it is, so that the caller fails closed.
func (s *Passkeys) FinishLogin(ctx context.Context, ceremonyID string, response []byte) (token string, expiresAt time.Time, err error) {
	data, ok := s.take(ceremonyID, ceremonyLogin)
	if !ok {
		return "", time.Time{}, domain.ErrUnauthenticated
	}
	handle, err := s.store.UserHandle(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("load passkey user handle: %w", err)
	}
	log := zerowrap.FromCtx(ctx)
	pk, err := s.verifier.FinishLogin(handle, data, response, func(id []byte) (domain.Passkey, error) { return s.store.Get(ctx, id) })
	if err != nil {
		if errors.Is(err, domain.ErrPasskeyCloned) {
			log.Warn().Msg("passkey sign-in refused: the counter did not advance, the authenticator may be cloned")
		}
		if errors.Is(err, domain.ErrUnauthenticated) {
			return "", time.Time{}, domain.ErrUnauthenticated
		}
		return "", time.Time{}, fmt.Errorf("verify passkey login: %w", err)
	}

	// Recording the use and opening the session are one step with respect to
	// Remove, so that a passkey removed meanwhile cannot sign in.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	pk.LastUsedAt = s.clock.Now().UTC()
	if err := s.store.Update(ctx, pk); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", time.Time{}, domain.ErrUnauthenticated
		}
		return "", time.Time{}, fmt.Errorf("record passkey use: %w", err)
	}
	token, expiresAt, err = s.sessions.issueOwner(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	log.Info().Str("passkey", passkeyRef(pk.ID)).Time("expires_at", expiresAt).Msg("owner signed in with a passkey")
	return token, expiresAt, nil
}

// List returns the passkeys, oldest first. Only the owner may.
func (s *Passkeys) List(ctx context.Context, p domain.Principal) ([]domain.Passkey, error) {
	if err := requireOwner(p); err != nil {
		return nil, err
	}
	return s.store.List(ctx)
}

// Remove deletes a passkey and signs every browser out, like revoking a token
// with owner rights does. Only the owner may. An unknown id gives
// domain.ErrNotFound.
func (s *Passkeys) Remove(ctx context.Context, p domain.Principal, id []byte) error {
	if err := requireOwner(p); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.store.Delete(ctx, id); err != nil {
		return err
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("passkey", passkeyRef(id)).Str("removed_by", p.Label()).Str("removed_by_token", p.TokenID()).Msg("passkey removed")
	epoch, err := s.sessions.bumpEpoch(ctx)
	if err != nil {
		return fmt.Errorf("passkey %s removed, but signing out the web sessions failed: %w", passkeyRef(id), err)
	}
	log.Info().Str("passkey", passkeyRef(id)).Uint64("epoch", epoch).Msg("every web session signed out: a passkey was removed")
	return nil
}

// passkeyRef is how logs name a passkey: the first 8 hex characters of the
// SHA-256 of its credential id.
func passkeyRef(id []byte) string {
	sum := sha256.Sum256(id)
	return hex.EncodeToString(sum[:])[:8]
}
