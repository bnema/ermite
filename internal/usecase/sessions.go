// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bnema/zerowrap"
	"github.com/golang-jwt/jwt/v5"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Web login (ADR 0018). The owner asks for a one-time link with the API, opens
// it in a browser and confirms; the confirmation trades the link for a signed
// session token that the web keeps in an HttpOnly cookie.
const (
	// LoginLinkTTL is how long a login link works.
	LoginLinkTTL = 60 * time.Second
	// MaxPendingLoginLinks bounds the links that wait to be used, so that a
	// loop of CreateLoginLink cannot grow the memory.
	MaxPendingLoginLinks = 16
	// SessionTTL is the lifetime of a session.
	SessionTTL = 12 * time.Hour

	// loginNonceBytes is the size of the secret in a link: 256 bits.
	loginNonceBytes = 32
	// sessionSubject is the "sub" claim of every session: there is one owner.
	sessionSubject = "owner"
	// LoginPath is the path prefix of login links on the public URL.
	LoginPath = "/-/login/"
)

// Sessions are the use cases of the owner's browser sessions: login links,
// session tokens and signing out everywhere. Login links live in memory only,
// so a restart voids them; the signing key and the epoch are in the store.
type Sessions struct {
	store     ports.SessionStore
	clock     ports.Clock
	publicURL string
	key       []byte

	mu    sync.Mutex
	links map[[sha256.Size]byte]pendingLink // by hash of the nonce
}

var _ ports.SessionService = (*Sessions)(nil)

// pendingLink is a login link that waits to be used.
type pendingLink struct {
	expires time.Time
	// sessionCap is the latest moment the session it opens may last until: the
	// expiry of the token that created the link. Zero means no cap.
	sessionCap time.Time
}

// NewSessions builds the session use cases and loads the signing key, which
// the store generates on first start. publicURL is the base URL the login
// links are built on.
func NewSessions(ctx context.Context, store ports.SessionStore, clock ports.Clock, publicURL string) (*Sessions, error) {
	key, err := store.Key(ctx)
	if err != nil {
		return nil, fmt.Errorf("load session key: %w", err)
	}
	return &Sessions{store: store, clock: clock, publicURL: publicURL, key: key, links: map[[sha256.Size]byte]pendingLink{}}, nil
}

// requireOwner is the access rule of the session API: root, or a stored token
// with every scope and no repository list.
func requireOwner(p domain.Principal) error {
	switch {
	case p.IsAnonymous():
		return domain.ErrUnauthenticated
	case !p.HasOwnerRights():
		return fmt.Errorf("%w: managing web sessions needs the admin token or a token with every scope and no repository list", domain.ErrForbidden)
	}
	return nil
}

// CreateLoginLink makes a link valid for LoginLinkTTL and usable once. Only
// the owner may. The session the link opens never outlives the token that
// created it: it is capped at that token's expiry (no cap for root, or a token
// that never expires). It gives domain.ErrResourceExhausted when MaxPendingLoginLinks
// links wait. The nonce is never logged.
func (s *Sessions) CreateLoginLink(ctx context.Context, p domain.Principal) (ports.LoginLink, error) {
	if err := requireOwner(p); err != nil {
		return ports.LoginLink{}, err
	}
	var raw [loginNonceBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ports.LoginLink{}, fmt.Errorf("generate login link: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(raw[:])
	now := s.clock.Now()
	exp := now.Add(LoginLinkTTL)

	s.mu.Lock()
	for h, l := range s.links {
		if !now.Before(l.expires) {
			delete(s.links, h)
		}
	}
	if len(s.links) >= MaxPendingLoginLinks {
		s.mu.Unlock()
		return ports.LoginLink{}, fmt.Errorf("%w: %d login links are already pending, use one or wait a minute", domain.ErrResourceExhausted, MaxPendingLoginLinks)
	}
	s.links[sha256.Sum256([]byte(nonce))] = pendingLink{expires: exp, sessionCap: p.ExpiresAt()}
	s.mu.Unlock()

	log := zerowrap.FromCtx(ctx)
	log.Info().Str("created_by", p.Label()).Str("created_by_token", p.TokenID()).Time("expires_at", exp).Msg("login link created")
	return ports.LoginLink{URL: s.publicURL + LoginPath + nonce, ExpiresAt: exp.UTC()}, nil
}

// LoginLinkValid reports whether the nonce is a pending link, without using
// it: opening a link shows a confirmation, only confirming consumes it.
func (s *Sessions) LoginLinkValid(nonce string) bool {
	h := sha256.Sum256([]byte(nonce))
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[h]
	return ok && s.clock.Now().Before(l.expires)
}

// Login consumes the link and opens a session: it returns the signed session
// token, for the cookie, and when it expires. A link that is unknown, expired
// or already used gives domain.ErrUnauthenticated, whatever the reason. The
// nonce and the token are never logged. The session lasts SessionTTL, or until
// the expiry of the token that created the link when that comes first; a link
// whose token has expired since gives domain.ErrUnauthenticated.
func (s *Sessions) Login(ctx context.Context, nonce string) (token string, expiresAt time.Time, err error) {
	h := sha256.Sum256([]byte(nonce))
	s.mu.Lock()
	l, ok := s.links[h]
	delete(s.links, h) // single use, also when expired
	s.mu.Unlock()
	if !ok || !s.clock.Now().Before(l.expires) {
		return "", time.Time{}, domain.ErrUnauthenticated
	}
	token, expiresAt, err = s.issue(ctx, l.sessionCap)
	if err != nil {
		return "", time.Time{}, err
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Time("expires_at", expiresAt).Msg("owner signed in on the web")
	return token, expiresAt, nil
}

// sessionClaims are the claims of a session token. The epoch is a pointer so
// that a token without one is not taken for epoch 0.
type sessionClaims struct {
	Epoch *uint64 `json:"epoch"`
	jwt.RegisteredClaims
}

// issue signs a session for SessionTTL, cut at capAt unless that is zero.
func (s *Sessions) issue(ctx context.Context, capAt time.Time) (string, time.Time, error) {
	epoch, err := s.store.Epoch(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read session epoch: %w", err)
	}
	now := s.clock.Now().UTC()
	exp := now.Add(SessionTTL)
	if !capAt.IsZero() && capAt.Before(exp) {
		exp = capAt.UTC()
	}
	if !exp.Truncate(time.Second).After(now) {
		return "", time.Time{}, domain.ErrUnauthenticated
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, sessionClaims{
		Epoch: &epoch,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sessionSubject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}).SignedString(s.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign session: %w", err)
	}
	return tok, exp.Truncate(time.Second), nil
}

// issueOwner opens an uncapped session for the owner (a passkey login): the
// same token a login link gives, for SessionTTL.
func (s *Sessions) issueOwner(ctx context.Context) (string, time.Time, error) {
	return s.issue(ctx, time.Time{})
}

// bumpEpoch ends every session and returns the new epoch.
func (s *Sessions) bumpEpoch(ctx context.Context) (uint64, error) {
	return s.store.BumpEpoch(ctx)
}

// Verify checks a session token and returns the owner. The signature must be
// HS256 with the session key (the algorithm is pinned, so "none" and any other
// are refused), the token must not be expired or issued in the future, and its
// epoch must be the current one. Any such failure gives
// domain.ErrUnauthenticated; a store failure is returned as it is, so that the
// caller fails closed.
func (s *Sessions) Verify(ctx context.Context, token string) (domain.Principal, error) {
	var c sessionClaims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return s.key, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithSubject(sessionSubject),
		jwt.WithTimeFunc(s.clock.Now),
	)
	if err != nil || c.Epoch == nil {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	epoch, err := s.store.Epoch(ctx)
	if err != nil {
		return domain.Principal{}, fmt.Errorf("read session epoch: %w", err)
	}
	if *c.Epoch != epoch {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	return domain.Owner(), nil
}

// SignOutEverywhere ends every session by incrementing the epoch. Only the
// owner may. Pending login links stay valid.
func (s *Sessions) SignOutEverywhere(ctx context.Context, p domain.Principal) error {
	if err := requireOwner(p); err != nil {
		return err
	}
	epoch, err := s.store.BumpEpoch(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return err
		}
		return fmt.Errorf("sign out everywhere: %w", err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("by", p.Label()).Str("by_token", p.TokenID()).Uint64("epoch", epoch).Msg("every web session signed out")
	return nil
}
