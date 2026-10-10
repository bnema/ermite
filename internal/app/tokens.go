// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// MaxTokenLabelLen is the longest token label, in bytes.
const MaxTokenLabelLen = 100

// touchInterval is how often last_used_at of a token is written at most.
const touchInterval = time.Minute

// Tokens are the use cases that manage stored tokens.
type Tokens struct {
	store ports.TokenStore
	clock ports.Clock
}

// NewTokens builds the token use cases over their ports.
func NewTokens(store ports.TokenStore, clock ports.Clock) *Tokens {
	return &Tokens{store: store, clock: clock}
}

// CreateTokenParams are the settings of a new token.
type CreateTokenParams struct {
	Label string
	// Scopes must not be empty.
	Scopes []domain.Scope
	// Repos limits the token to these repositories; empty means every one.
	Repos []domain.RepoName
	// ExpiresAt is the zero time for a token that never expires.
	ExpiresAt time.Time
}

// CreatedToken is a new token with its secret, which is shown once and never
// stored.
type CreatedToken struct {
	Token  domain.Token
	Secret string
}

// Create makes a stored token and returns it with its secret.
//
// The principal needs token:admin: anonymous callers get
// domain.ErrUnauthenticated, others without it domain.ErrForbidden. A token
// cannot create a token more powerful than itself: every scope must be one it
// holds, the repositories must lie within its list (a limited token cannot
// create an unlimited one) and the expiry must not outlast its own. Violations
// give domain.ErrForbidden. Root has no such limits. Invalid settings give
// domain.ErrInvalidToken, an unknown scope domain.ErrInvalidScope.
func (t *Tokens) Create(ctx context.Context, p domain.Principal, params CreateTokenParams) (CreatedToken, error) {
	if p.IsAnonymous() {
		return CreatedToken{}, domain.ErrUnauthenticated
	}
	if !canManageTokens(p) {
		return CreatedToken{}, fmt.Errorf("%w: creating tokens needs %s", domain.ErrForbidden, domain.ScopeTokenAdmin)
	}
	now := t.clock.Now().UTC()
	tok, err := buildToken(params, now)
	if err != nil {
		return CreatedToken{}, err
	}
	if err := checkNoEscalation(p, tok); err != nil {
		return CreatedToken{}, err
	}

	for range 3 {
		secret, err := newSecret()
		if err != nil {
			return CreatedToken{}, err
		}
		id, err := newTokenID()
		if err != nil {
			return CreatedToken{}, err
		}
		tok.ID, tok.Hash = id, domain.HashTokenSecret(secret)
		err = t.store.Create(ctx, tok)
		if errors.Is(err, domain.ErrAlreadyExists) {
			continue // a random id or secret collided: practically never
		}
		if err != nil {
			return CreatedToken{}, fmt.Errorf("store token: %w", err)
		}
		log := zerowrap.FromCtx(ctx)
		log.Info().
			Str("token_id", tok.ID).Str("label", tok.Label).
			Int("scopes", len(tok.Scopes)).Int("repos", len(tok.Repos)).
			Str("created_by", p.Label()).Str("created_by_token", p.TokenID()).
			Msg("token created")
		return CreatedToken{Token: tok, Secret: secret}, nil
	}
	return CreatedToken{}, errors.New("store token: could not find a free id")
}

// buildToken validates the settings and returns the token without id and hash.
func buildToken(params CreateTokenParams, now time.Time) (domain.Token, error) {
	if params.Label == "" || len(params.Label) > MaxTokenLabelLen || !utf8.ValidString(params.Label) {
		return domain.Token{}, fmt.Errorf("%w: label must be 1 to %d bytes of UTF-8", domain.ErrInvalidToken, MaxTokenLabelLen)
	}
	if len(params.Scopes) == 0 {
		return domain.Token{}, fmt.Errorf("%w: at least one scope is required", domain.ErrInvalidToken)
	}
	for _, s := range params.Scopes {
		if _, err := domain.ParseScope(s.String()); err != nil {
			return domain.Token{}, err
		}
	}
	for _, r := range params.Repos {
		if r.IsZero() {
			return domain.Token{}, fmt.Errorf("%w: empty repository name", domain.ErrInvalidToken)
		}
	}
	if !params.ExpiresAt.IsZero() && !params.ExpiresAt.After(now) {
		return domain.Token{}, fmt.Errorf("%w: expiry is not in the future", domain.ErrInvalidToken)
	}
	tok := domain.Token{
		Label:     params.Label,
		CreatedAt: now,
		ExpiresAt: params.ExpiresAt.UTC(),
	}
	// Canonical order, no duplicates.
	for _, s := range domain.AllScopes() {
		if slices.Contains(params.Scopes, s) {
			tok.Scopes = append(tok.Scopes, s)
		}
	}
	for _, r := range params.Repos {
		if !slices.Contains(tok.Repos, r) {
			tok.Repos = append(tok.Repos, r)
		}
	}
	slices.SortFunc(tok.Repos, func(a, b domain.RepoName) int { return compareNames(a, b) })
	return tok, nil
}

func compareNames(a, b domain.RepoName) int {
	switch {
	case a.String() < b.String():
		return -1
	case a.String() > b.String():
		return 1
	}
	return 0
}

// checkNoEscalation enforces that tok is within the rights of its creator.
func checkNoEscalation(p domain.Principal, tok domain.Token) error {
	if p.IsRoot() {
		return nil
	}
	for _, s := range tok.Scopes {
		if !p.HasScope(s) {
			return fmt.Errorf("%w: scope %s exceeds the creating token", domain.ErrForbidden, s)
		}
	}
	if own := p.Repos(); len(own) > 0 {
		if len(tok.Repos) == 0 {
			return fmt.Errorf("%w: the creating token is limited to a repository list, so the new token must be too", domain.ErrForbidden)
		}
		for _, r := range tok.Repos {
			if !slices.Contains(own, r) {
				return fmt.Errorf("%w: repository %s is outside the creating token", domain.ErrForbidden, r)
			}
		}
	}
	if exp := p.ExpiresAt(); !exp.IsZero() {
		if tok.ExpiresAt.IsZero() || tok.ExpiresAt.After(exp) {
			return fmt.Errorf("%w: expiry outlasts the creating token", domain.ErrForbidden)
		}
	}
	return nil
}

// List returns the stored tokens, oldest first. It needs token:admin.
func (t *Tokens) List(ctx context.Context, p domain.Principal) ([]domain.Token, error) {
	if err := requireTokenAdmin(p); err != nil {
		return nil, err
	}
	return t.store.List(ctx)
}

// Revoke deletes a stored token; it stops working at once. It needs
// token:admin and returns domain.ErrNotFound for an unknown id.
func (t *Tokens) Revoke(ctx context.Context, p domain.Principal, id string) error {
	if err := requireTokenAdmin(p); err != nil {
		return err
	}
	if err := t.store.Revoke(ctx, id); err != nil {
		return err
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("token_id", id).Str("revoked_by", p.Label()).Str("revoked_by_token", p.TokenID()).Msg("token revoked")
	return nil
}

func requireTokenAdmin(p domain.Principal) error {
	if p.IsAnonymous() {
		return domain.ErrUnauthenticated
	}
	if !canManageTokens(p) {
		return fmt.Errorf("%w: needs %s", domain.ErrForbidden, domain.ScopeTokenAdmin)
	}
	return nil
}

// newSecret returns "ermite_" and 32 random bytes in base64url.
func newSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("random token secret: %w", err)
	}
	return domain.TokenSecretPrefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// newTokenID returns 16 hex characters.
func newTokenID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("random token id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// Auth authenticates secrets presented by callers.
type Auth struct {
	root   domain.TokenHash
	tokens ports.TokenStore
	clock  ports.Clock
}

// NewAuth builds the authentication use case. rootHash is the SHA-256 of the
// admin token.
func NewAuth(rootHash domain.TokenHash, tokens ports.TokenStore, clock ports.Clock) *Auth {
	return &Auth{root: rootHash, tokens: tokens, clock: clock}
}

// Authenticate resolves a token secret to its principal: root when it is the
// admin token (constant-time comparison of the hashes), else the stored token
// with that hash if it has not expired. Any other secret, including the empty
// one, gives domain.ErrUnauthenticated. last_used_at of a stored token is
// written at most once a minute; a failure to write it is logged, not
// returned.
func (a *Auth) Authenticate(ctx context.Context, secret string) (domain.Principal, error) {
	if secret == "" {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	h := domain.HashTokenSecret(secret)
	if subtle.ConstantTimeCompare(h[:], a.root[:]) == 1 {
		return domain.Root(), nil
	}
	tok, err := a.tokens.FindByHash(ctx, h)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	if err != nil {
		return domain.Principal{}, fmt.Errorf("find token: %w", err)
	}
	now := a.clock.Now().UTC()
	if tok.Expired(now) {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	if tok.LastUsedAt.IsZero() || now.Sub(tok.LastUsedAt) >= touchInterval {
		if err := a.tokens.Touch(ctx, tok.ID, now); err != nil && !errors.Is(err, domain.ErrNotFound) {
			log := zerowrap.FromCtx(ctx)
			log.Warn().Err(err).Str("token_id", tok.ID).Msg("record token use")
		}
	}
	return domain.TokenPrincipal(tok), nil
}
