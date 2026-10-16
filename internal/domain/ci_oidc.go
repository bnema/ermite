// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "time"

// OIDC ID tokens of CI jobs (CI-4, D5): a job asks the server for an RS256 JWT
// with GitHub's claims, which a relying party (Vault, a cloud) verifies against
// the server's JWKS instead of holding a long-lived secret.

// OIDCRequestTokenPrefix starts the secret a job presents to the token
// endpoint (ACTIONS_ID_TOKEN_REQUEST_TOKEN). It is a job repository token
// (CIRepoToken.OIDC) that authenticates nothing else.
const OIDCRequestTokenPrefix = "ermiteoidc_"

// IsOIDCRequestToken reports whether s has the shape of an ID token request
// token.
func IsOIDCRequestToken(s string) bool { return hasTokenShape(s, OIDCRequestTokenPrefix) }

// PermIDToken is the permission that lets a job request ID tokens.
const PermIDToken = "id-token"

// IDTokenAllowed reports whether a job with the permissions stored for it may
// request ID tokens: the run is trusted and the job has id-token: write. The
// permissions are lowered by the trust rule first (TokenPermissions), so an
// untrusted run never qualifies, whatever it asks.
func IDTokenAllowed(stored map[string]string, trusted bool) bool {
	return trusted && TokenPermissions(stored, trusted)[PermIDToken] == PermissionWrite
}

// OIDCKeyRetention is how long a replaced signing key stays in the JWKS: far
// longer than the life of the tokens it signed (OIDCTokenLifetime).
const OIDCKeyRetention = 24 * time.Hour

// OIDCTokenLifetime is how long an ID token is valid after it is issued, and
// OIDCNotBeforeSkew how far before its issue it is already valid (GitHub's
// values: exp = iat + 5 min, nbf = iat - 10 min).
const (
	OIDCTokenLifetime = 5 * time.Minute
	OIDCNotBeforeSkew = 10 * time.Minute
)

// OIDCRunnerEnvironment is the runner_environment claim: ermite's runners are
// the owner's machines, which GitHub calls self-hosted.
const OIDCRunnerEnvironment = "self-hosted"

// MaxOIDCAudienceLen bounds the audience a job may ask for.
const MaxOIDCAudienceLen = 1024

// OIDCKey is a signing key of the ID tokens as stored: the private key sealed
// with the secrets key, bound to its path (OIDCKeyPath). A key without
// RetiredAt signs; a retired one is only published until RetiredAt plus
// OIDCKeyRetention.
type OIDCKey struct {
	// ID is the kid: the RFC 7638 thumbprint of the public key.
	ID string
	// Sealed is the PKCS #8 DER of the private key, sealed.
	Sealed    []byte
	CreatedAt time.Time
	RetiredAt time.Time
}

// OIDCKeyPath is where the key with that id is stored, and the additional data
// it is sealed with, so that a sealed key moved to another id does not open.
func OIDCKeyPath(kid string) string { return "oidc/key/" + kid }

// Published reports whether the key is still in the JWKS at now.
func (k OIDCKey) Published(now time.Time) bool {
	return k.RetiredAt.IsZero() || now.Before(k.RetiredAt.Add(OIDCKeyRetention))
}

// OIDCSubject is the sub claim of a job's ID token in GitHub's default format:
// the environment of the job when it has one, else "pull_request" for the
// events of merge requests, else the ref. repository is "owner/name".
//
// GitHub gives pull_request_target runs the pull_request context too, though
// they run the workflow of the default branch: a relying party that trusts
// "repo:o/r:pull_request" trusts both.
func OIDCSubject(repository, eventName, environment, ref string) string {
	prefix := "repo:" + repository + ":"
	switch {
	case environment != "":
		return prefix + "environment:" + environment
	case eventName == "pull_request" || eventName == "pull_request_target":
		return prefix + "pull_request"
	}
	return prefix + "ref:" + ref
}
