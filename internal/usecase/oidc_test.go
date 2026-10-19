// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/adapters/secretbox"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

// testRSAKeys are generated once for the package: a key of the real size takes
// a while, more so under the race detector.
var testRSAKeys struct {
	sync.Mutex
	keys []*rsa.PrivateKey
}

// testRSAKey returns the i-th test key.
func testRSAKey(t *testing.T, i int) *rsa.PrivateKey {
	t.Helper()
	testRSAKeys.Lock()
	defer testRSAKeys.Unlock()
	for len(testRSAKeys.keys) <= i {
		k, err := rsa.GenerateKey(rand.Reader, usecase.OIDCKeyBits)
		require.NoError(t, err)
		testRSAKeys.keys = append(testRSAKeys.keys, k)
	}
	return testRSAKeys.keys[i]
}

// keySeq hands out the test keys in order, one per call.
func keySeq(t *testing.T) func() (*rsa.PrivateKey, error) {
	n := 0
	return func() (*rsa.PrivateKey, error) {
		k := testRSAKey(t, n)
		n++
		return k, nil
	}
}

type oidcFixture struct {
	store  *kvstore.Store
	box    *secretbox.Box
	clock  *fakeClock
	newKey func() (*rsa.PrivateKey, error)
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	store, err := kvstore.Open(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	box, err := secretbox.New(bytes.Repeat([]byte{7}, secretbox.KeyLen))
	require.NoError(t, err)
	return &oidcFixture{store: store, box: box, clock: &fakeClock{t: now}, newKey: keySeq(t)}
}

func (f *oidcFixture) open(t *testing.T) *usecase.OIDC {
	t.Helper()
	o, err := usecase.NewOIDC(context.Background(), usecase.OIDCConfig{
		Store: f.store, Sealer: f.box, Tokens: f.store.CI(), Clock: f.clock, PublicURL: "https://forge.test", NewKey: f.newKey,
	})
	require.NoError(t, err)
	require.NotNil(t, o)
	return o
}

// jwksKids lists the kids of a JWKS, and checks each is the RFC 7638
// thumbprint of its key.
func jwksKids(t *testing.T, jwks []byte) []string {
	t.Helper()
	var set struct {
		Keys []struct{ Kty, Alg, Use, Kid, N, E string } `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(jwks, &set))
	var kids []string
	for _, k := range set.Keys {
		assert.Equal(t, "RSA", k.Kty)
		assert.Equal(t, "RS256", k.Alg)
		assert.Equal(t, "sig", k.Use)
		assert.Equal(t, k.Kid, usecase.Thumbprint(jwkPublic(t, k.N, k.E)))
		kids = append(kids, k.Kid)
	}
	return kids
}

func jwkPublic(t *testing.T, n, e string) *rsa.PublicKey {
	t.Helper()
	nb, err := base64.RawURLEncoding.DecodeString(n)
	require.NoError(t, err)
	eb, err := base64.RawURLEncoding.DecodeString(e)
	require.NoError(t, err)
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
}

// The example of RFC 7638, section 3.1.
func TestThumbprintRFC7638(t *testing.T) {
	t.Parallel()
	pub := jwkPublic(t, "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FDW2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw", "AQAB")
	assert.Equal(t, "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs", usecase.Thumbprint(pub))
}

func TestNoSecretsKeyNoOIDC(t *testing.T) {
	t.Parallel()
	f := newOIDCFixture(t)
	o, err := usecase.NewOIDC(context.Background(), usecase.OIDCConfig{Store: f.store, Tokens: f.store.CI(), Clock: f.clock, PublicURL: "https://forge.test"})
	require.NoError(t, err)
	require.NotNil(t, o)
	assert.False(t, o.Enabled())
	keys, err := f.store.ListOIDCKeys(context.Background())
	require.NoError(t, err)
	assert.Empty(t, keys, "no key is generated")
}

// The key is generated once, stored sealed (never as a PEM or DER in the
// clear) and bound to its path, and survives a restart.
func TestOIDCKeyIsSealedAndKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOIDCFixture(t)
	o := f.open(t)
	assert.Equal(t, "https://forge.test/-/oidc", o.Issuer())
	assert.Equal(t, "https://forge.test/-/oidc/token?api-version=2.0", o.TokenURL())
	jwks, err := o.JWKS()
	require.NoError(t, err)
	kids := jwksKids(t, jwks)
	require.Len(t, kids, 1)
	assert.Equal(t, usecase.Thumbprint(&testRSAKey(t, 0).PublicKey), kids[0])

	raw, ok := f.store.RawOIDCKey(ctx, kids[0])
	require.True(t, ok)
	assert.NotContains(t, string(raw), "PRIVATE KEY", "not a PEM")
	priv := testRSAKey(t, 0)
	assert.False(t, bytes.Contains(raw, priv.D.Bytes()), "the private exponent is not in the clear")
	assert.False(t, bytes.Contains(raw, priv.Primes[0].Bytes()), "nor a prime")

	// It survives a restart: no second key.
	o2 := f.open(t)
	jwks2, err := o2.JWKS()
	require.NoError(t, err)
	assert.Equal(t, kids, jwksKids(t, jwks2))

	// A sealed key moved to another id does not open: the server refuses to
	// start rather than serve a key it cannot check.
	stored, err := f.store.ListOIDCKeys(ctx)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	moved := stored[0]
	moved.ID = "another"
	require.NoError(t, f.store.SaveOIDCKeys(ctx, []domain.OIDCKey{moved}, []string{stored[0].ID}))
	_, err = usecase.NewOIDC(ctx, usecase.OIDCConfig{Store: f.store, Sealer: f.box, Tokens: f.store.CI(), Clock: f.clock, PublicURL: "https://forge.test", NewKey: f.newKey})
	require.Error(t, err)
}

// Rotation: the new key signs at once, the old one stays published 24 hours
// and is forgotten afterwards. Only the owner rotates.
func TestOIDCRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOIDCFixture(t)
	o := f.open(t)
	first := usecase.Thumbprint(&testRSAKey(t, 0).PublicKey)
	second := usecase.Thumbprint(&testRSAKey(t, 1).PublicKey)

	_, err := o.RotateKey(ctx, domain.Anonymous())
	require.ErrorIs(t, err, domain.ErrUnauthenticated)
	limited := domain.TokenPrincipal(domain.Token{ID: "t", Scopes: domain.AllScopes(), Repos: []domain.RepoName{name(t, "proj")}})
	_, err = o.RotateKey(ctx, limited)
	require.ErrorIs(t, err, domain.ErrForbidden)

	r, err := o.RotateKey(ctx, domain.Root())
	require.NoError(t, err)
	assert.Equal(t, second, r.Kid)
	assert.Equal(t, first, r.RetiredKid)
	assert.Equal(t, now.Add(24*time.Hour), r.RetiredUntil)
	jwks, err := o.JWKS()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{first, second}, jwksKids(t, jwks))

	// A token signed now names the new key.
	token := signWith(t, o)
	assert.Equal(t, second, jwtHeader(t, token)["kid"])

	// A restart within the 24 hours still publishes both, the new one signs.
	f.clock.t = now.Add(23 * time.Hour)
	o = f.open(t)
	jwks, err = o.JWKS()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{first, second}, jwksKids(t, jwks))

	// After 24 hours the old one is no longer published, and a restart
	// deletes it.
	f.clock.t = now.Add(24 * time.Hour)
	jwks, err = o.JWKS()
	require.NoError(t, err)
	assert.Equal(t, []string{second}, jwksKids(t, jwks))
	f.open(t)
	stored, err := f.store.ListOIDCKeys(ctx)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, second, stored[0].ID)
}

// signWith signs a token through the exported path of the tests.
func signWith(t *testing.T, o *usecase.OIDC) string {
	t.Helper()
	tok, err := o.SignForTest(map[string]any{"sub": "x"})
	require.NoError(t, err)
	verifyJWT(t, tok, o)
	return tok
}

func jwtHeader(t *testing.T, token string) map[string]any {
	t.Helper()
	h, _, _ := strings.Cut(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(h)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// verifyJWT checks the RS256 signature of token against the JWKS of o, with
// the standard library only, and returns its claims.
func verifyJWT(t *testing.T, token string, o *usecase.OIDC) map[string]any {
	t.Helper()
	jwks, err := o.JWKS()
	require.NoError(t, err)
	var set struct {
		Keys []struct{ Kid, N, E string } `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(jwks, &set))
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	h := jwtHeader(t, token)
	assert.Equal(t, "RS256", h["alg"])
	var pub *rsa.PublicKey
	for _, k := range set.Keys {
		if k.Kid == h["kid"] {
			pub = jwkPublic(t, k.N, k.E)
		}
	}
	require.NotNil(t, pub, "the kid is in the JWKS")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	require.NoError(t, rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig))
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	return claims
}

// The contexts of a job (Job.contexts), as the planner computes them.
func jobContexts(event, ref, headRef, baseRef, jobWorkflowRef string) string {
	b, _ := json.Marshal(map[string]any{
		"github": map[string]any{
			"ref": ref, "sha": "abc123", "repository": "owner/proj", "repository_owner": "owner",
			"repository_owner_id": "1048577", "run_id": "42", "run_number": "7", "run_attempt": "2",
			"repository_visibility": "private", "repository_id": "1048999", "actor_id": "1048577", "actor": "owner",
			"workflow": "CI", "head_ref": headRef, "base_ref": baseRef, "event_name": event, "ref_protected": false,
			"ref_type": "branch", "workflow_ref": "owner/proj/.github/workflows/ci.yml@" + ref, "workflow_sha": "abc123",
		},
		"job": map[string]any{
			"check_run_id": 9007199254740991, "workflow_ref": jobWorkflowRef, "workflow_sha": "def456",
		},
	})
	return string(b)
}

// The claims follow GitHub's token: sub per event and environment, the job's
// workflow in job_workflow_ref, numbers as strings, the times around iat.
func TestIDTokenClaimsPerEvent(t *testing.T) {
	t.Parallel()
	own := "owner/proj/.github/workflows/ci.yml@refs/heads/main"
	called := "owner/tools/.github/workflows/deploy.yml@refs/tags/v1"
	tests := []struct {
		name, event, ref, head, base, env, jobWF string
		sub                                      string
	}{
		{"push", "push", "refs/heads/main", "", "", "", own, "repo:owner/proj:ref:refs/heads/main"},
		{"tag", "push", "refs/tags/v1.0.0", "", "", "", own, "repo:owner/proj:ref:refs/tags/v1.0.0"},
		{"dispatch", "workflow_dispatch", "refs/heads/main", "", "", "", own, "repo:owner/proj:ref:refs/heads/main"},
		{"schedule", "schedule", "refs/heads/main", "", "", "", own, "repo:owner/proj:ref:refs/heads/main"},
		{"pull_request", "pull_request", "refs/pull/3/merge", "feat", "main", "", own, "repo:owner/proj:pull_request"},
		{"pull_request_target", "pull_request_target", "refs/heads/main", "feat", "main", "", own, "repo:owner/proj:pull_request"},
		{"environment wins", "pull_request", "refs/pull/3/merge", "feat", "main", "prod", own, "repo:owner/proj:environment:prod"},
		{"push to an environment", "push", "refs/heads/main", "", "", "staging", own, "repo:owner/proj:environment:staging"},
		{"reusable workflow", "push", "refs/heads/main", "", "", "", called, "repo:owner/proj:ref:refs/heads/main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, keys, err := usecase.IDTokenClaims(jobContexts(tt.event, tt.ref, tt.head, tt.base, tt.jobWF), tt.env, "https://forge.test/-/oidc", "https://forge.test/owner", now)
			require.NoError(t, err)
			assert.Equal(t, tt.sub, c["sub"])
			assert.Equal(t, "jti", keys[0])
			assert.Equal(t, "https://forge.test/-/oidc", c["iss"])
			assert.Equal(t, "https://forge.test/owner", c["aud"])
			assert.Equal(t, tt.ref, c["ref"])
			assert.Equal(t, tt.event, c["event_name"])
			assert.Equal(t, tt.head, c["head_ref"])
			assert.Equal(t, tt.base, c["base_ref"])
			assert.Equal(t, "2", c["run_attempt"])
			assert.Equal(t, "42", c["run_id"])
			assert.Equal(t, "false", c["ref_protected"])
			assert.Equal(t, "owner/proj/.github/workflows/ci.yml@"+tt.ref, c["workflow_ref"], "the caller's workflow")
			assert.Equal(t, tt.jobWF, c["job_workflow_ref"], "the workflow that defines the job")
			assert.Equal(t, "def456", c["job_workflow_sha"])
			assert.Equal(t, "9007199254740991", c["check_run_id"], "exact, as a string")
			assert.Equal(t, "self-hosted", c["runner_environment"])
			assert.Equal(t, now.Unix(), c["iat"])
			assert.Equal(t, now.Unix()-600, c["nbf"])
			assert.Equal(t, now.Unix()+300, c["exp"])
			if tt.env != "" {
				assert.Equal(t, tt.env, c["environment"])
			} else {
				assert.NotContains(t, c, "environment", "GitHub leaves it out without an environment")
			}
		})
	}
}

func TestDisabledOIDCRotationAuthorization(t *testing.T) {
	o, err := usecase.NewOIDC(context.Background(), usecase.OIDCConfig{})
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		p    domain.Principal
		want error
	}{
		{"anonymous", domain.Anonymous(), domain.ErrForbidden},
		{"limited", domain.TokenPrincipal(domain.Token{ID: "limited"}), domain.ErrForbidden},
		{"root", domain.Root(), domain.ErrPreconditionFailed},
		{"owner", domain.Owner(), domain.ErrPreconditionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := o.RotateKey(context.Background(), tc.p)
			require.ErrorIs(t, err, tc.want)
			if tc.want == domain.ErrPreconditionFailed {
				assert.EqualError(t, err, "the server issues no OIDC ID tokens: it has no ERMITE_SECRETS_KEY")
			}
		})
	}
	_, err = o.Discovery()
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = o.JWKS()
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
