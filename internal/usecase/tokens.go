// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// MaxTokenLabelLen is the longest token label, in bytes.
const MaxTokenLabelLen = 100

// reservedTokenLabels cannot label a token: they are what the audit trail
// (author of a merge, "created_by" in the log) shows for the admin token and
// for the owner signed in on the web, so a token with one of them would pass
// for the owner.
var reservedTokenLabels = []string{"owner", "root"}

// touchInterval is how often last_used_at of a token is written at most.
const touchInterval = time.Minute

// Tokens are the use cases that manage stored tokens.
type Tokens struct {
	store    ports.TokenStore
	clock    ports.Clock
	sessions ports.SessionStore
}

var _ ports.TokenService = (*Tokens)(nil)

// NewTokens builds the token use cases over their ports. sessions is where
// revoking a token with owner rights ends every web session (see Revoke).
func NewTokens(store ports.TokenStore, clock ports.Clock, sessions ports.SessionStore) *Tokens {
	return &Tokens{store: store, clock: clock, sessions: sessions}
}

// Create makes a stored token and returns it with its secret.
//
// The principal needs token:admin: anonymous callers get
// domain.ErrUnauthenticated, others without it domain.ErrForbidden. A token
// cannot create a token more powerful than itself: every scope must be one it
// holds, the repositories must lie within its list (a limited token cannot
// create an unlimited one), the same for branch prefixes (each must start
// with one of its own) and the expiry must not outlast its own. Violations
// give domain.ErrForbidden. Root has no such limits. Invalid settings give
// domain.ErrInvalidToken (this includes the labels "owner" and "root", which
// are reserved, in any case and around spaces), an unknown scope
// domain.ErrInvalidScope.
func (t *Tokens) Create(ctx context.Context, p domain.Principal, params ports.CreateTokenParams) (ports.CreatedToken, error) {
	if p.IsAnonymous() {
		return ports.CreatedToken{}, domain.ErrUnauthenticated
	}
	if !canManageTokens(p) {
		return ports.CreatedToken{}, fmt.Errorf("%w: creating tokens needs %s", domain.ErrForbidden, domain.ScopeTokenAdmin)
	}
	now := t.clock.Now().UTC()
	tok, err := buildToken(params, now)
	if err != nil {
		return ports.CreatedToken{}, err
	}
	if err := checkNoEscalation(p, tok); err != nil {
		return ports.CreatedToken{}, err
	}

	for range 3 {
		secret := newSecret()
		tok.ID, tok.Hash = newTokenID(), domain.HashTokenSecret(secret)
		err := t.store.Create(ctx, tok)
		if errors.Is(err, domain.ErrAlreadyExists) {
			continue // a random id or secret collided: practically never
		}
		if err != nil {
			return ports.CreatedToken{}, fmt.Errorf("store token: %w", err)
		}
		log := zerowrap.FromCtx(ctx)
		log.Info().
			Str("token_id", tok.ID).Str("label", tok.Label).
			Int("scopes", len(tok.Scopes)).Int("repos", len(tok.Repos)).Int("branch_prefixes", len(tok.BranchPrefixes)).
			Str("created_by", p.Label()).Str("created_by_token", p.TokenID()).
			Msg("token created")
		return ports.CreatedToken{Token: tok, Secret: secret}, nil
	}
	return ports.CreatedToken{}, errors.New("store token: could not find a free id")
}

// buildToken validates the settings and returns the token without id and hash.
func buildToken(params ports.CreateTokenParams, now time.Time) (domain.Token, error) {
	if params.Label == "" || len(params.Label) > MaxTokenLabelLen || !utf8.ValidString(params.Label) {
		return domain.Token{}, fmt.Errorf("%w: label must be 1 to %d bytes of UTF-8", domain.ErrInvalidToken, MaxTokenLabelLen)
	}
	for _, r := range reservedTokenLabels {
		if strings.EqualFold(strings.TrimSpace(params.Label), r) {
			return domain.Token{}, fmt.Errorf("%w: label %q is reserved", domain.ErrInvalidToken, r)
		}
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
	prefixes, err := domain.ParseBranchPrefixes(params.BranchPrefixes)
	if err != nil {
		return domain.Token{}, err
	}
	tok := domain.Token{
		Label:          params.Label,
		BranchPrefixes: prefixes,
		CreatedAt:      now,
		ExpiresAt:      params.ExpiresAt.UTC(),
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
	slices.SortFunc(tok.Repos, func(a, b domain.RepoName) int { return strings.Compare(a.String(), b.String()) })
	return tok, nil
}

// checkNoEscalation enforces that tok is within the rights of p: every scope
// held by p, the repositories within its list, the branch prefixes within its
// own (a limited p only makes tokens that are limited too, each prefix under
// one of its own) and the expiry not beyond its own. Root has no limits.
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
	if p.IsBranchLimited() {
		if len(tok.BranchPrefixes) == 0 {
			return fmt.Errorf("%w: the creating token is limited to branch prefixes, so the new token must be too", domain.ErrForbidden)
		}
		for _, bp := range tok.BranchPrefixes {
			if !p.CoversBranchPrefix(bp) {
				return fmt.Errorf("%w: branch prefix %s is outside the creating token", domain.ErrForbidden, bp)
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

// List returns the stored tokens the principal could have created, oldest
// first: those whose scopes, repositories, branch prefixes and expiry lie within its own (the
// no-escalation rule of Create). Root sees every token. It needs token:admin.
func (t *Tokens) List(ctx context.Context, p domain.Principal) ([]domain.Token, error) {
	if err := requireTokenAdmin(p); err != nil {
		return nil, err
	}
	all, err := t.store.List(ctx)
	if err != nil {
		return nil, err
	}
	if p.IsRoot() {
		return all, nil
	}
	return slices.DeleteFunc(all, func(tok domain.Token) bool { return checkNoEscalation(p, tok) != nil }), nil
}

// Revoke deletes a stored token; it stops working at once. It needs
// token:admin and returns domain.ErrNotFound for an unknown id, and also for
// a token more powerful than the principal (the no-escalation rule of
// Create), so that it does not show itself. Root may revoke any token.
//
// A token that has owner rights (HasOwnerRights: every scope, no repository
// list and no branch prefixes) could open web sessions, so revoking it also signs every browser out,
// as `ermite web logout --all` does: web sessions are not tied to the token
// that opened them. The revocation holds even if signing out fails, which is
// then reported as an error.
func (t *Tokens) Revoke(ctx context.Context, p domain.Principal, id string) error {
	if err := requireTokenAdmin(p); err != nil {
		return err
	}
	tok, err := t.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if !p.IsRoot() && checkNoEscalation(p, tok) != nil {
		return domain.ErrNotFound
	}
	if err := t.store.Revoke(ctx, id); err != nil {
		return err
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("token_id", id).Str("revoked_by", p.Label()).Str("revoked_by_token", p.TokenID()).Msg("token revoked")
	if !domain.TokenPrincipal(tok).HasOwnerRights() {
		return nil
	}
	epoch, err := t.sessions.BumpEpoch(ctx)
	if err != nil {
		return fmt.Errorf("token %s revoked, but signing out the web sessions failed: %w", id, err)
	}
	log.Info().Str("token_id", id).Uint64("epoch", epoch).Msg("every web session signed out: the revoked token had owner rights")
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

// newSecret returns "ermite_" and 32 random bytes in base64url. crypto/rand
// does not fail: it aborts the program if the system has no entropy.
func newSecret() string {
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	return domain.TokenSecretPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
}

// newTokenID returns 16 hex characters.
func newTokenID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// Auth authenticates secrets presented by callers.
type Auth struct {
	root   domain.TokenHash
	tokens ports.TokenStore
	clock  ports.Clock
	// repoTokens resolves the job repository tokens (domain.RepoTokenPrefix);
	// nil when CI is not wired.
	repoTokens ports.CIRepoTokenStore
}

var (
	_ ports.AuthService = (*Auth)(nil)
	_ ports.Revalidator = (*Auth)(nil)
)

// NewAuth builds the authentication use case. rootHash is the SHA-256 of the
// admin token.
func NewAuth(rootHash domain.TokenHash, tokens ports.TokenStore, clock ports.Clock) *Auth {
	return &Auth{root: rootHash, tokens: tokens, clock: clock}
}

// WithRepoTokens makes Authenticate accept the job repository tokens of CI
// (the "github.token" of a job, ADR 0025). They resolve to a principal shaped
// like a stored token (repo:read on one repository, with an expiry), so no
// rule of the forge knows them; they are stored apart from the stored tokens,
// so ListTokens never shows them and token:admin never manages them. Call it
// once while wiring.
func (a *Auth) WithRepoTokens(s ports.CIRepoTokenStore) *Auth {
	a.repoTokens = s
	return a
}

// Revalidate checks that the holder of a principal obtained earlier is still
// authorized: root always is; a stored token must still exist and not have
// expired. It gives domain.ErrUnauthenticated otherwise, and any error of the
// store as it is, so that callers fail closed. It is for long operations that
// outlive the request that authenticated them, such as a push.
func (a *Auth) Revalidate(ctx context.Context, p domain.Principal) error {
	if p.IsRoot() {
		return nil
	}
	if p.IsAnonymous() {
		return domain.ErrUnauthenticated
	}
	if id := p.TokenID(); domain.IsRepoTokenID(id) {
		return a.revalidateRepoToken(ctx, strings.TrimPrefix(id, "job:"))
	}
	tok, err := a.tokens.Get(ctx, p.TokenID())
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrUnauthenticated
	}
	if err != nil {
		return fmt.Errorf("find token: %w", err)
	}
	if tok.Expired(a.clock.Now().UTC()) {
		return domain.ErrUnauthenticated
	}
	return nil
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
	if domain.IsRepoToken(secret) {
		return a.authenticateRepoToken(ctx, h)
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

// authenticateRepoToken resolves a job repository token: it exists (it is
// deleted when its job ends, is cancelled or lost) and has not expired.
func (a *Auth) authenticateRepoToken(ctx context.Context, h domain.TokenHash) (domain.Principal, error) {
	if a.repoTokens == nil {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	t, err := a.repoTokens.FindRepoTokenByHash(ctx, h)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	if err != nil {
		return domain.Principal{}, fmt.Errorf("find repository token: %w", err)
	}
	if t.Expired(a.clock.Now().UTC()) {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	return domain.RepoTokenPrincipal(t), nil
}

// revalidateRepoToken is Revalidate for the principal of a job repository
// token.
func (a *Auth) revalidateRepoToken(ctx context.Context, id string) error {
	if a.repoTokens == nil {
		return domain.ErrUnauthenticated
	}
	t, err := a.repoTokens.GetRepoToken(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrUnauthenticated
	}
	if err != nil {
		return fmt.Errorf("find repository token: %w", err)
	}
	if t.Expired(a.clock.Now().UTC()) {
		return domain.ErrUnauthenticated
	}
	return nil
}
