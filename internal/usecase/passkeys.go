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
	// MaxPendingPasskeyRegistrations bounds the registrations that wait to be
	// finished. Only the owner starts one.
	MaxPendingPasskeyRegistrations = 16
	// MaxPendingPasskeyLogins bounds the logins that wait to be finished.
	// Anyone may start one, so a login is never refused: on overflow one is
	// dropped instead, and anonymous callers cannot lock the owner out.
	MaxPendingPasskeyLogins = 256
	// MaxPendingPasskeyLoginsPerClient bounds the logins one client (one client
	// key) may have waiting. A client over it loses its own oldest.
	MaxPendingPasskeyLoginsPerClient = 8

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
	seq     uint64             // creation order
	client  string             // who began a login (its client key); "" for a registration
	name    domain.PasskeyName // what a registration will call the passkey
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

// pendingLogins is what begin knows of the logins that wait, per client.
type pendingLogins struct {
	count     int
	oldest    [sha256.Size]byte
	oldestSeq uint64 // 0 until a login is added: sequence numbers start at 1
}

func (p *pendingLogins) add(h [sha256.Size]byte, seq uint64) {
	p.count++
	if p.oldestSeq == 0 || seq < p.oldestSeq {
		p.oldest, p.oldestSeq = h, seq
	}
}

// begin keeps a ceremony and returns its identifier. It sweeps the expired
// ones first. A registration over the limit gives domain.ErrResourceExhausted.
// A login is never refused, but it can cost another login its place: over the
// per-client cap it drops that client's own oldest; over the global cap it drops
// the oldest of the client with the most logins waiting. A client that floods
// therefore only ever loses its own ceremonies, never another client's.
func (s *Passkeys) begin(c ceremony) (string, error) {
	var raw [ceremonyIDBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate passkey ceremony: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(raw[:])
	now := s.clock.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	registrations := 0
	logins := 0
	byClient := map[string]*pendingLogins{}
	for h, c := range s.ceremonies {
		if !now.Before(c.expires) {
			delete(s.ceremonies, h)
			continue
		}
		switch c.kind {
		case ceremonyRegister:
			registrations++
		case ceremonyLogin:
			logins++
			pl := byClient[c.client]
			if pl == nil {
				pl = &pendingLogins{}
				byClient[c.client] = pl
			}
			pl.add(h, c.seq)
		}
	}
	switch c.kind {
	case ceremonyRegister:
		if registrations >= MaxPendingPasskeyRegistrations {
			return "", fmt.Errorf("%w: %d passkey registrations are already pending, finish one or wait two minutes", domain.ErrResourceExhausted, MaxPendingPasskeyRegistrations)
		}
	case ceremonyLogin:
		if pl := byClient[c.client]; pl != nil && pl.count >= MaxPendingPasskeyLoginsPerClient {
			delete(s.ceremonies, pl.oldest)
			logins--
			// The client is back under its cap; the global step below needs
			// its new count and its new oldest.
			pl.count--
			s.refreshOldest(pl, c.client)
		}
		if logins >= MaxPendingPasskeyLogins {
			// The client with the most waiting loses its oldest; among equals,
			// the one whose oldest waited longest.
			var victim *pendingLogins
			for _, pl := range byClient {
				if pl.count == 0 {
					continue
				}
				if victim == nil || pl.count > victim.count || (pl.count == victim.count && pl.oldestSeq < victim.oldestSeq) {
					victim = pl
				}
			}
			if victim != nil {
				delete(s.ceremonies, victim.oldest)
			}
		}
	}
	s.seq++
	c.expires, c.seq = now.Add(domain.PasskeyCeremonyTTL), s.seq
	s.ceremonies[sha256.Sum256([]byte(id))] = c
	return id, nil
}

// refreshOldest recomputes the oldest login of a client after one was dropped.
// The caller holds s.mu.
func (s *Passkeys) refreshOldest(pl *pendingLogins, client string) {
	pl.oldestSeq = 0
	for h, c := range s.ceremonies {
		if c.kind != ceremonyLogin || c.client != client {
			continue
		}
		if pl.oldestSeq == 0 || c.seq < pl.oldestSeq {
			pl.oldest, pl.oldestSeq = h, c.seq
		}
	}
}

// take removes the ceremony and returns it. It is single use, also when the
// ceremony expired or is of another kind. ok is false for an unknown, expired or
// mismatched ceremony.
func (s *Passkeys) take(id string, kind ceremonyKind) (c ceremony, ok bool) {
	h := sha256.Sum256([]byte(id))
	s.mu.Lock()
	c, found := s.ceremonies[h]
	delete(s.ceremonies, h)
	s.mu.Unlock()
	if !found || c.kind != kind || !s.clock.Now().Before(c.expires) {
		return ceremony{}, false
	}
	return c, true
}

// BeginRegistration starts adding a passkey. Only the owner may. name is
// checked now (an empty name means the default), so that a name the owner would
// be prompted for and then see refused costs no ceremony; the ceremony keeps the
// checked name and FinishRegistration stores it. It gives domain.ErrResourceExhausted when MaxPasskeys passkeys exist or too
// many registrations wait. It returns the ceremony identifier and the options
// for the browser.
func (s *Passkeys) BeginRegistration(ctx context.Context, p domain.Principal, name string) (string, []byte, error) {
	if err := requireOwner(p); err != nil {
		return "", nil, err
	}
	parsed, err := parsePasskeyName(name)
	if err != nil {
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
	id, err := s.begin(ceremony{kind: ceremonyRegister, data: data, name: parsed})
	if err != nil {
		return "", nil, err
	}
	return id, options, nil
}

// FinishRegistration checks the browser's response and stores the passkey under
// the name BeginRegistration checked. A ceremony that is unknown, expired, used
// or of the wrong kind, and a response the verifier refuses, give
// domain.ErrUnauthenticated; a credential already registered gives
// domain.ErrAlreadyExists; too many passkeys, domain.ErrResourceExhausted.
func (s *Passkeys) FinishRegistration(ctx context.Context, p domain.Principal, ceremonyID string, response []byte) error {
	if err := requireOwner(p); err != nil {
		return err
	}
	log := zerowrap.FromCtx(ctx)
	c, ok := s.take(ceremonyID, ceremonyRegister)
	if !ok {
		err := fmt.Errorf("%w: passkey registration expired or unknown, start again", domain.ErrUnauthenticated)
		log.Info().Err(err).Msg("passkey registration refused")
		return err
	}
	handle, err := s.store.UserHandle(ctx)
	if err != nil {
		return fmt.Errorf("load passkey user handle: %w", err)
	}
	pk, err := s.verifier.FinishRegistration(handle, c.data, response)
	if err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			// The reason is for the log (it holds no key); the caller gets a 400.
			log.Info().Err(err).Msg("passkey registration refused")
		}
		return err
	}
	if n := len(pk.ID); n == 0 || n > domain.MaxPasskeyIDLen {
		return fmt.Errorf("%w: credential id has %d bytes", domain.ErrUnauthenticated, n)
	}
	pk.Name = c.name.String()
	pk.CreatedAt = s.clock.Now().UTC()
	pk.LastUsedAt = time.Time{}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	existing, err := s.store.List(ctx)
	if err != nil {
		return fmt.Errorf("list passkeys: %w", err)
	}
	if len(existing) >= domain.MaxPasskeys {
		return fmt.Errorf("%w: %d passkeys are registered, remove one first", domain.ErrResourceExhausted, domain.MaxPasskeys)
	}
	if err := s.store.Create(ctx, pk); err != nil {
		return err
	}
	log.Info().Str("passkey", passkeyRef(pk.ID)).Msg("passkey added")
	return nil
}

// BeginLogin starts a passkey sign-in. Anonymous callers may. client is the
// client key of the caller (clientlimit.Key of the address the failure limiter
// uses); it bounds how many sign-ins one client keeps waiting. It returns the
// ceremony identifier and the options for the browser.
func (s *Passkeys) BeginLogin(_ context.Context, client string) (string, []byte, error) {
	options, data, err := s.verifier.BeginLogin()
	if err != nil {
		return "", nil, err
	}
	id, err := s.begin(ceremony{kind: ceremonyLogin, data: data, client: client})
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
	log := zerowrap.FromCtx(ctx)
	c, ok := s.take(ceremonyID, ceremonyLogin)
	if !ok {
		log.Info().Msg("passkey sign-in refused: the ceremony expired or is unknown")
		return "", time.Time{}, domain.ErrUnauthenticated
	}
	handle, err := s.store.UserHandle(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("load passkey user handle: %w", err)
	}
	pk, err := s.verifier.FinishLogin(handle, c.data, response, func(id []byte) (domain.Passkey, error) { return s.store.Get(ctx, id) })
	if err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			// The reason is for the log (it holds no key); the caller learns nothing.
			log.Info().Err(err).Msg("passkey sign-in refused")
			return "", time.Time{}, domain.ErrUnauthenticated
		}
		return "", time.Time{}, fmt.Errorf("verify passkey login: %w", err)
	}

	// Recording the use and opening the session are one step with respect to
	// Remove and to a concurrent login, so that a passkey removed meanwhile
	// cannot sign in, nor two logins with the same counter.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	cur, err := s.store.Get(ctx, pk.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			log.Info().Str("passkey", passkeyRef(pk.ID)).Msg("passkey sign-in refused: the passkey was removed meanwhile")
			return "", time.Time{}, domain.ErrUnauthenticated
		}
		return "", time.Time{}, fmt.Errorf("read passkey: %w", err)
	}
	// The one sign-count check. The verifier returns the counter the response
	// reports (the stored one when it does not advance), and another login may
	// have moved the stored one since it looked. A counter that does not advance
	// is the clone signal; synced passkeys report 0 and are exempt.
	if cur.SignCount != 0 && pk.SignCount <= cur.SignCount {
		log.Warn().Str("passkey", passkeyRef(pk.ID)).Msg("passkey sign-in refused: the counter did not advance, the authenticator may be cloned")
		return "", time.Time{}, domain.ErrUnauthenticated
	}
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

// parsePasskeyName validates a name; an empty one is domain.DefaultPasskeyName.
func parsePasskeyName(name string) (domain.PasskeyName, error) {
	if name == "" {
		name = domain.DefaultPasskeyName
	}
	parsed, err := domain.ParsePasskeyName(name)
	if err != nil {
		return domain.PasskeyName{}, fmt.Errorf("passkey name: %w", err)
	}
	return parsed, nil
}

// passkeyRef is how logs name a passkey: the first 8 hex characters of the
// SHA-256 of its credential id.
func passkeyRef(id []byte) string {
	sum := sha256.Sum256(id)
	return hex.EncodeToString(sum[:])[:8]
}
