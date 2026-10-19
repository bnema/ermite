// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"context"
	"time"

	"bnema.dev/ermite/internal/domain"
)

// OIDCKeyStore keeps the signing keys of the ID tokens of CI jobs.
// *kvstore.Store implements it.
type OIDCKeyStore interface {
	// ListOIDCKeys returns every stored key, oldest first.
	ListOIDCKeys(ctx context.Context) ([]domain.OIDCKey, error)
	// SaveOIDCKeys writes the keys of put (created or replaced) and deletes the
	// keys whose ids are in drop, in one batch.
	SaveOIDCKeys(ctx context.Context, put []domain.OIDCKey, drop []string) error
}

// OIDCService is the OIDC provider of CI jobs, served under /-/oidc: the
// discovery document, the JWKS and the token endpoint. *usecase.CI implements
// it.
type OIDCService interface {
	// OIDCEnabled reports whether the server issues ID tokens (it has a secrets
	// key to seal its signing key with). When it does not, the other methods
	// give domain.ErrNotFound.
	OIDCEnabled() bool
	// OIDCDiscovery returns the JSON of the OpenID Provider configuration.
	OIDCDiscovery() ([]byte, error)
	// OIDCKeys returns the JSON of the JWKS: the key that signs and the keys
	// replaced less than domain.OIDCKeyRetention ago.
	OIDCKeys() ([]byte, error)
	// IDToken returns a signed ID token of the job whose request token is
	// secret, for audience ("" for the default). A secret that is not the
	// request token of a job that is assigned or running gives
	// domain.ErrUnauthenticated; an audience that is too long gives a
	// domain.InvalidFieldsError.
	IDToken(ctx context.Context, secret, audience string) (string, error)
}

// OIDCRotation is the outcome of a key rotation.
type OIDCRotation struct {
	Issuer string
	// Kid is the id of the new key, RetiredKid the one it replaced ("" for
	// none), published until RetiredUntil.
	Kid          string
	RetiredKid   string
	RetiredUntil time.Time
}

// OIDCKeyService manages the signing key of the ID tokens. *usecase.OIDC
// implements it.
type OIDCKeyService interface {
	// Enabled reports whether the server issues ID tokens.
	Enabled() bool
	// RotateKey replaces the signing key by a new one. It needs owner rights.
	RotateKey(ctx context.Context, p domain.Principal) (OIDCRotation, error)
}
