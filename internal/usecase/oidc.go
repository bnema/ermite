// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The OIDC provider of CI jobs (CI-4, D5). The server signs GitHub-shaped ID
// tokens (RS256) with a key of its own, published with the discovery document
// and the JWKS under <public URL>/-/oidc, the issuer. The key is generated at
// the first start, stored sealed with ERMITE_SECRETS_KEY (without a secrets key
// there is no OIDC at all) and replaced by RotateKey; a replaced key stays in
// the JWKS for domain.OIDCKeyRetention so that the tokens it signed verify
// until they expire.

// OIDCPath is where the provider is served under the public URL; the issuer
// is the public URL followed by it.
const OIDCPath = "/-/oidc"

// OIDCKeyBits is the size of the RSA signing keys.
const OIDCKeyBits = 3072

// OIDCConfig holds what the OIDC provider needs. Store, Tokens, Clock and
// PublicURL are required; Sealer nil disables OIDC.
type OIDCConfig struct {
	Store ports.OIDCKeyStore
	// Sealer seals the private keys (ERMITE_SECRETS_KEY). Leave it unset
	// rather than assigning a typed nil pointer.
	Sealer ports.SecretSealer
	// Tokens finds the request tokens of the jobs.
	Tokens    ports.CIRepoTokenStore
	Clock     ports.Clock
	PublicURL string
	// NewKey generates a signing key; nil generates an RSA key of OIDCKeyBits.
	// Tests give prepared keys.
	NewKey func() (*rsa.PrivateKey, error)
}

// OIDC is the key management of the provider and the signature of the tokens.
// It is safe for concurrent use.
type OIDC struct {
	cfg    OIDCConfig
	issuer string

	mu sync.RWMutex
	// keys are the published keys, oldest first; the last one that is not
	// retired signs.
	keys []oidcKey
}

type oidcKey struct {
	stored domain.OIDCKey
	priv   *rsa.PrivateKey
}

var _ ports.OIDCKeyService = (*OIDC)(nil)

// NewOIDC loads the signing keys, generates the first one when there is none,
// and forgets the retired keys whose publication is over. With cfg.Sealer nil
// it returns a disabled service: no ID tokens are issued. A
// stored key that does not open with the secrets key is an error: the server
// must not start (the secrets key check already refuses another key, so this
// is a damaged record).
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	log := zerowrap.FromCtx(ctx)
	if cfg.Sealer == nil {
		log.Info().Msg("ERMITE_SECRETS_KEY is not set: CI jobs get no OIDC ID tokens")
		return &OIDC{}, nil
	}
	if cfg.NewKey == nil {
		cfg.NewKey = func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, OIDCKeyBits) }
	}
	o := &OIDC{cfg: cfg, issuer: cfg.PublicURL + OIDCPath}
	stored, err := cfg.Store.ListOIDCKeys(ctx)
	if err != nil {
		return nil, err
	}
	now := cfg.Clock.Now().UTC()
	var drop []string
	for _, k := range stored {
		if !k.Published(now) {
			drop = append(drop, k.ID)
			continue
		}
		priv, err := o.open(k)
		if err != nil {
			return nil, err
		}
		o.keys = append(o.keys, oidcKey{stored: k, priv: priv})
	}
	var put []domain.OIDCKey
	if _, ok := o.signing(); !ok {
		k, err := o.newKey(now)
		if err != nil {
			return nil, err
		}
		o.keys = append(o.keys, k)
		put = append(put, k.stored)
	}
	if len(put) > 0 || len(drop) > 0 {
		if err := cfg.Store.SaveOIDCKeys(ctx, put, drop); err != nil {
			return nil, err
		}
	}
	k, _ := o.signing()
	log.Info().Str("issuer", o.issuer).Str("kid", k.stored.ID).Msg("oidc provider ready")
	return o, nil
}

// Issuer is the iss of the tokens.
func (o *OIDC) Issuer() string { return o.issuer }

// TokenURL is the token endpoint as a job gets it (Job.id_token_url): it has a
// query already, so clients append "&audience=".
func (o *OIDC) TokenURL() string { return o.issuer + "/token?api-version=2.0" }

// open unseals and parses a stored key, and checks that it is the key its id
// names.
func (o *OIDC) open(k domain.OIDCKey) (*rsa.PrivateKey, error) {
	der, err := o.cfg.Sealer.Open(k.Sealed, []byte(domain.OIDCKeyPath(k.ID)))
	if err != nil {
		return nil, fmt.Errorf("open the oidc key %s: %w", k.ID, err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("open the oidc key %s: %w", k.ID, err)
	}
	priv, ok := parsed.(*rsa.PrivateKey)
	if !ok || thumbprint(&priv.PublicKey) != k.ID {
		return nil, fmt.Errorf("open the oidc key %s: not the key of that id", k.ID)
	}
	return priv, nil
}

// newKey generates a key and seals it under its path.
func (o *OIDC) newKey(now time.Time) (oidcKey, error) {
	priv, err := o.cfg.NewKey()
	if err != nil {
		return oidcKey{}, fmt.Errorf("generate the oidc key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return oidcKey{}, fmt.Errorf("generate the oidc key: %w", err)
	}
	kid := thumbprint(&priv.PublicKey)
	sealed := o.cfg.Sealer.Seal(der, []byte(domain.OIDCKeyPath(kid)))
	clear(der)
	return oidcKey{stored: domain.OIDCKey{ID: kid, Sealed: sealed, CreatedAt: now}, priv: priv}, nil
}

// signing returns the key that signs: the newest that is not retired.
func (o *OIDC) signing() (oidcKey, bool) {
	for i := len(o.keys) - 1; i >= 0; i-- {
		if o.keys[i].stored.RetiredAt.IsZero() {
			return o.keys[i], true
		}
	}
	return oidcKey{}, false
}

// RotateKey replaces the signing key by a new one. The old one signs no more
// and stays in the JWKS for domain.OIDCKeyRetention. Retired keys whose
// publication is over are deleted with it. Only the owner may rotate.
func (o *OIDC) RotateKey(ctx context.Context, p domain.Principal) (ports.OIDCRotation, error) {
	if !o.Enabled() {
		if !p.HasOwnerRights() {
			return ports.OIDCRotation{}, domain.ErrForbidden
		}
		return ports.OIDCRotation{}, oidcDisabledError{}
	}

	switch {
	case p.IsAnonymous():
		return ports.OIDCRotation{}, domain.ErrUnauthenticated
	case !p.HasOwnerRights():
		return ports.OIDCRotation{}, fmt.Errorf("%w: rotating the oidc key needs the admin token or a token with every scope, no repository list and no branch prefixes", domain.ErrForbidden)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.cfg.Clock.Now().UTC()
	fresh, err := o.newKey(now)
	if err != nil {
		return ports.OIDCRotation{}, err
	}
	out := ports.OIDCRotation{Issuer: o.issuer, Kid: fresh.stored.ID}
	put := []domain.OIDCKey{fresh.stored}
	var drop []string
	keys := make([]oidcKey, 0, len(o.keys)+1)
	for _, k := range o.keys {
		if k.stored.RetiredAt.IsZero() {
			k.stored.RetiredAt = now
			put = append(put, k.stored)
			out.RetiredKid, out.RetiredUntil = k.stored.ID, now.Add(domain.OIDCKeyRetention)
		}
		if !k.stored.Published(now) {
			drop = append(drop, k.stored.ID)
			continue
		}
		keys = append(keys, k)
	}
	if err := o.cfg.Store.SaveOIDCKeys(ctx, put, drop); err != nil {
		return ports.OIDCRotation{}, err
	}
	o.keys = append(keys, fresh)
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("kid", fresh.stored.ID).Str("retired_kid", out.RetiredKid).Str("by", p.Label()).Msg("oidc key rotated")
	return out, nil
}

// jwk is a public key of the JWKS (RFC 7517).
type jwk struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func publicJWK(kid string, pub *rsa.PublicKey) jwk {
	return jwk{Kty: "RSA", Alg: "RS256", Use: "sig", Kid: kid, N: b64(pub.N.Bytes()), E: b64(big.NewInt(int64(pub.E)).Bytes())}
}

// JWKS returns the JSON of the published keys.
func (o *OIDC) JWKS() ([]byte, error) {
	if !o.Enabled() {
		return nil, domain.ErrNotFound
	}
	now := o.cfg.Clock.Now().UTC()
	o.mu.RLock()
	defer o.mu.RUnlock()
	keys := make([]jwk, 0, len(o.keys))
	for _, k := range o.keys {
		if k.stored.Published(now) {
			keys = append(keys, publicJWK(k.stored.ID, &k.priv.PublicKey))
		}
	}
	return json.Marshal(map[string][]jwk{"keys": keys})
}

// oidcClaims are the claims a job's token may hold, as listed by the discovery
// document (GitHub's, without those of features ermite lacks).
var oidcClaims = []string{
	"sub", "aud", "exp", "iat", "iss", "jti", "nbf", "ref", "sha", "repository", "repository_id", "repository_owner",
	"repository_owner_id", "run_id", "run_number", "run_attempt", "actor", "actor_id", "workflow", "workflow_ref",
	"workflow_sha", "head_ref", "base_ref", "event_name", "ref_type", "ref_protected", "environment",
	"job_workflow_ref", "job_workflow_sha", "repository_visibility", "runner_environment", "check_run_id",
}

// Discovery returns the JSON of the OpenID Provider configuration, in the
// shape of GitHub's.
func (o *OIDC) Discovery() ([]byte, error) {
	if !o.Enabled() {
		return nil, domain.ErrNotFound
	}
	return json.Marshal(ordered{
		{"issuer", o.issuer},
		{"jwks_uri", o.issuer + "/.well-known/jwks"},
		{"subject_types_supported", []string{"public", "pairwise"}},
		{"response_types_supported", []string{"id_token"}},
		{"claims_supported", oidcClaims},
		{"id_token_signing_alg_values_supported", []string{"RS256"}},
		{"scopes_supported", []string{"openid"}},
	})
}

// sign returns the compact JWS of claims, signed RS256 by the signing key.
func (o *OIDC) sign(claims ordered) (string, error) {
	o.mu.RLock()
	k, ok := o.signing()
	o.mu.RUnlock()
	if !ok {
		return "", errors.New("no oidc signing key")
	}
	header, err := json.Marshal(ordered{{"typ", "JWT"}, {"alg", "RS256"}, {"kid", k.stored.ID}})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign the id token: %w", err)
	}
	return input + "." + b64(sig), nil
}

// thumbprint is the RFC 7638 thumbprint of an RSA public key: base64url of the
// SHA-256 of {"e":…,"kty":"RSA","n":…} (members in lexical order, no spaces).
func thumbprint(pub *rsa.PublicKey) string {
	e := b64(big.NewInt(int64(pub.E)).Bytes())
	sum := sha256.Sum256([]byte(`{"e":"` + e + `","kty":"RSA","n":"` + b64(pub.N.Bytes()) + `"}`))
	return b64(sum[:])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Enabled reports whether the provider issues ID tokens. A nil provider is disabled.
func (o *OIDC) Enabled() bool { return o != nil && o.cfg.Sealer != nil }

// oidcDisabledError preserves the public disabled-provider message and precondition code.
type oidcDisabledError struct{}

func (oidcDisabledError) Error() string {
	return "the server issues no OIDC ID tokens: it has no ERMITE_SECRETS_KEY"
}
func (oidcDisabledError) Is(target error) bool { return target == domain.ErrPreconditionFailed }
