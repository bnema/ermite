// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

// CI-4 D5 / AC5: the OIDC ID tokens of CI jobs.

// oidcKeys are the RSA keys the test servers sign with, generated once for the
// package: a 3072-bit key takes a while, more so under the race detector.
var oidcKeys struct {
	sync.Mutex
	keys []*rsa.PrivateKey
}

// oidcTestKeys hands out the prepared keys in order, one per call, to one
// server.
func oidcTestKeys(t *testing.T) func() (*rsa.PrivateKey, error) {
	n := 0
	return func() (*rsa.PrivateKey, error) {
		oidcKeys.Lock()
		defer oidcKeys.Unlock()
		for len(oidcKeys.keys) <= n {
			k, err := rsa.GenerateKey(rand.Reader, usecase.OIDCKeyBits)
			if err != nil {
				return nil, err
			}
			oidcKeys.keys = append(oidcKeys.keys, k)
		}
		n++
		return oidcKeys.keys[n-1], nil
	}
}

const oidcWorkflow = `on: push
jobs:
  oidc:
    runs-on: ubuntu-latest
    permissions:
      id-token: write
      contents: read
    steps:
      - run: echo oidc
  deploy:
    runs-on: ubuntu-latest
    environment: prod
    permissions:
      id-token: write
    steps:
      - run: echo deploy
  plain:
    runs-on: ubuntu-latest
    steps:
      - run: echo plain
`

// idToken asks the token endpoint for an ID token with the bearer, as
// @actions/core does with ACTIONS_ID_TOKEN_REQUEST_URL (rebased on the test
// server) and "&audience=".
func (c *ciHarness) idToken(j *pb.Job, bearer, audience string) (int, string) {
	c.t.Helper()
	u, err := url.Parse(j.GetIdTokenUrl())
	require.NoError(c.t, err)
	path := u.RequestURI()
	if audience != "" {
		path += "&audience=" + url.QueryEscape(audience)
	}
	status, body, hdr := c.get(path, "Bearer "+bearer)
	if status != http.StatusOK {
		return status, body
	}
	assert.Equal(c.t, "no-store", hdr.Get("Cache-Control"))
	var res struct {
		Value string `json:"value"`
	}
	require.NoError(c.t, json.Unmarshal([]byte(body), &res))
	require.NotEmpty(c.t, res.Value)
	return status, res.Value
}

// verifyIDToken checks the RS256 signature of token against the JWKS the
// server publishes, with the standard library only, and returns its header
// and claims.
func (c *ciHarness) verifyIDToken(token string) (header, claims map[string]any) {
	c.t.Helper()
	status, body, hdr := c.get("/-/oidc/.well-known/jwks", "")
	require.Equal(c.t, http.StatusOK, status, body)
	assert.Contains(c.t, hdr.Get("Cache-Control"), "public")
	var set struct {
		Keys []struct{ Kty, Kid, N, E string } `json:"keys"`
	}
	require.NoError(c.t, json.Unmarshal([]byte(body), &set))
	parts := strings.Split(token, ".")
	require.Len(c.t, parts, 3)
	decode := func(s string, v any) {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		require.NoError(c.t, err)
		require.NoError(c.t, json.Unmarshal(raw, v))
	}
	decode(parts[0], &header)
	decode(parts[1], &claims)
	require.Equal(c.t, "RS256", header["alg"])
	var pub *rsa.PublicKey
	for _, k := range set.Keys {
		if k.Kid != header["kid"] {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		require.NoError(c.t, err)
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		require.NoError(c.t, err)
		pub = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	require.NotNil(c.t, pub, "the kid of the token is in the JWKS")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(c.t, err)
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	require.NoError(c.t, rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig), "the signature verifies against the JWKS")
	return header, claims
}

func TestOIDCIDTokens(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	sha := c.push("main", map[string]string{".github/workflows/o.yml": oidcWorkflow}, "")
	jobs := runner.jobsByKey(3)
	job, deploy, plain := jobs["oidc"], jobs["deploy"], jobs["plain"]

	// Only the jobs with id-token: write get the endpoint and a request token,
	// and the token is nowhere in what the runner stores.
	assert.Equal(t, "http://example.test/-/oidc/token?api-version=2.0", job.GetIdTokenUrl())
	assert.True(t, domain.IsOIDCRequestToken(job.GetIdTokenRequestToken()))
	assert.NotEqual(t, job.GetIdTokenRequestToken(), deploy.GetIdTokenRequestToken())
	assert.NotContains(t, job.GetContexts(), job.GetIdTokenRequestToken())
	assert.Empty(t, plain.GetIdTokenUrl(), "no id-token: write")
	assert.Empty(t, plain.GetIdTokenRequestToken())
	ver, err := c.root.server.GetVersion(c.ctx, connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err)
	assert.Contains(t, ver.Msg.GetFeatures(), "ci.oidc")

	// Discovery, anonymous and cacheable.
	status, body, hdr := c.get("/-/oidc/.well-known/openid-configuration", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, hdr.Get("Cache-Control"), "max-age")
	var disc map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &disc))
	assert.Equal(t, "http://example.test/-/oidc", disc["issuer"])
	assert.Equal(t, "http://example.test/-/oidc/.well-known/jwks", disc["jwks_uri"])
	assert.Equal(t, []any{"RS256"}, disc["id_token_signing_alg_values_supported"])

	// The default audience, and the claims of a push.
	before := time.Now().Unix()
	status, tok := c.idToken(job, job.GetIdTokenRequestToken(), "")
	require.Equal(t, http.StatusOK, status, tok)
	header, claims := c.verifyIDToken(tok)
	assert.Equal(t, "JWT", header["typ"])
	assert.Equal(t, "http://example.test/-/oidc", claims["iss"])
	assert.Equal(t, "http://example.test/owner", claims["aud"])
	assert.Equal(t, "repo:owner/proj:ref:refs/heads/main", claims["sub"])
	assert.Equal(t, "refs/heads/main", claims["ref"])
	assert.Equal(t, "branch", claims["ref_type"])
	assert.Equal(t, sha, claims["sha"])
	assert.Equal(t, "owner/proj", claims["repository"])
	assert.Equal(t, "owner", claims["repository_owner"])
	assert.Equal(t, "private", claims["repository_visibility"])
	assert.Equal(t, "push", claims["event_name"])
	assert.Equal(t, "1", claims["run_attempt"])
	assert.Equal(t, "self-hosted", claims["runner_environment"])
	assert.Equal(t, "owner/proj/.github/workflows/o.yml@refs/heads/main", claims["job_workflow_ref"])
	assert.Equal(t, sha, claims["job_workflow_sha"])
	assert.NotEmpty(t, claims["jti"])
	assert.NotContains(t, claims, "environment")
	iat := int64(claims["iat"].(float64))
	assert.GreaterOrEqual(t, iat, before)
	assert.Equal(t, iat-600, int64(claims["nbf"].(float64)))
	assert.Equal(t, iat+300, int64(claims["exp"].(float64)))

	// A requested audience; every token has its own jti.
	status, tok2 := c.idToken(job, job.GetIdTokenRequestToken(), "sts.amazonaws.com")
	require.Equal(t, http.StatusOK, status, tok2)
	_, claims2 := c.verifyIDToken(tok2)
	assert.Equal(t, "sts.amazonaws.com", claims2["aud"])
	assert.NotEqual(t, claims["jti"], claims2["jti"])
	status, _ = c.idToken(job, job.GetIdTokenRequestToken(), strings.Repeat("a", domain.MaxOIDCAudienceLen+1))
	assert.Equal(t, http.StatusBadRequest, status)

	// The environment of a job is its subject.
	status, tok3 := c.idToken(deploy, deploy.GetIdTokenRequestToken(), "")
	require.Equal(t, http.StatusOK, status, tok3)
	_, claims3 := c.verifyIDToken(tok3)
	assert.Equal(t, "repo:owner/proj:environment:prod", claims3["sub"])
	assert.Equal(t, "prod", claims3["environment"])

	// No other secret gets a token: the job's repository token, its job token,
	// a token of the right shape that does not exist, nothing.
	for name, bearer := range map[string]string{
		"github token": job.GetGithubToken(), "job token": job.GetJobToken(), "admin token": adminToken,
		"unknown": domain.OIDCRequestTokenPrefix + strings.Repeat("A", domain.CITokenRandomLen),
	} {
		status, _ := c.idToken(job, bearer, "")
		assert.Equal(t, http.StatusUnauthorized, status, name)
	}
	status, _, _ = c.get("/-/oidc/token?api-version=2.0", "")
	assert.Equal(t, http.StatusUnauthorized, status, "no credentials")

	// The request token authenticates nothing else: not the API, not Git.
	status, _, _ = c.get("/-/github/api/repos/owner/proj", "Bearer "+job.GetIdTokenRequestToken())
	assert.Equal(t, http.StatusUnauthorized, status)
	out, err := c.clone(job.GetIdTokenRequestToken(), c.url("/owner/proj"))
	require.Error(t, err, out)

	// The key is stored sealed: the raw value is not a private key.
	kid, _ := header["kid"].(string)
	raw, ok := c.forge.store.RawOIDCKey(c.ctx, kid)
	require.True(t, ok)
	assert.NotContains(t, string(raw), "PRIVATE KEY", "not a PEM")
	oidcKeys.Lock()
	for _, k := range oidcKeys.keys {
		assert.False(t, bytes.Contains(raw, k.D.Bytes()), "no private exponent in the clear")
	}
	oidcKeys.Unlock()

	// Once the job ended, its request token is refused, while another job's
	// still works.
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	status, _ = c.idToken(job, job.GetIdTokenRequestToken(), "")
	assert.Equal(t, http.StatusUnauthorized, status, "the job ended")
	status, _ = c.idToken(deploy, deploy.GetIdTokenRequestToken(), "")
	assert.Equal(t, http.StatusOK, status)
}

// A run that is not trusted (pushed by a token limited to branches) gets no ID
// tokens, whatever its permissions say.
func TestOIDCUntrustedRunGetsNoToken(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{"README.md": "hi"}, "")
	secret, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), []string{"agent/"}, "proj")
	c.pushAs(secret, "agent/x", map[string]string{".github/workflows/o.yml": oidcWorkflow})
	for key, j := range runner.jobsByKey(3) {
		assert.False(t, j.GetTrusted(), key)
		assert.Empty(t, j.GetIdTokenUrl(), key)
		assert.Empty(t, j.GetIdTokenRequestToken(), key)
	}
}

// Without ERMITE_SECRETS_KEY there is no OIDC: no variables, no provider, no
// rotation.
func TestOIDCWithoutSecretsKey(t *testing.T) {
	c := newSecretsHarness(t, "")
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/o.yml": oidcWorkflow}, "")
	for key, j := range runner.jobsByKey(3) {
		assert.Empty(t, j.GetIdTokenUrl(), key)
		assert.Empty(t, j.GetIdTokenRequestToken(), key)
	}
	for _, path := range []string{"/-/oidc/.well-known/openid-configuration", "/-/oidc/.well-known/jwks", "/-/oidc/token?api-version=2.0"} {
		status, _, _ := c.get(path, "")
		assert.Equal(t, http.StatusNotFound, status, path)
	}
	_, err := c.root.server.RotateOIDCKey(c.ctx, connect.NewRequest(&pb.RotateOIDCKeyRequest{}))
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	ver, err := c.root.server.GetVersion(c.ctx, connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err)
	assert.NotContains(t, ver.Msg.GetFeatures(), "ci.oidc")
}

// `ermite oidc rotate-key`: owner only; the new key signs at once and the old
// one still verifies the tokens it signed.
func TestOIDCRotateKey(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/o.yml": oidcWorkflow}, "")
	job := runner.jobsByKey(3)["oidc"]
	status, before := c.idToken(job, job.GetIdTokenRequestToken(), "")
	require.Equal(t, http.StatusOK, status)
	oldHeader, _ := c.verifyIDToken(before)

	limited := c.api(c.token(scopes(domain.ScopeRepoRead, domain.ScopeCIWrite), "proj"))
	_, err := limited.server.RotateOIDCKey(c.ctx, connect.NewRequest(&pb.RotateOIDCKeyRequest{}))
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	res := runCLI(t, map[string]string{"ERMITE_URL": c.srv.URL, "ERMITE_TOKEN": adminToken}, "", "oidc", "rotate-key", "--json").ok(t)
	var rot struct {
		Issuer       string  `json:"issuer"`
		Kid          string  `json:"kid"`
		RetiredKid   *string `json:"retired_kid"`
		RetiredUntil *string `json:"retired_until"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.stdout), &rot), res.stdout)
	assert.Equal(t, "http://example.test/-/oidc", rot.Issuer)
	require.NotNil(t, rot.RetiredKid)
	assert.Equal(t, oldHeader["kid"], *rot.RetiredKid)
	assert.NotEqual(t, *rot.RetiredKid, rot.Kid)
	require.NotNil(t, rot.RetiredUntil)

	c.verifyIDToken(before) // the old key is still published
	status, after := c.idToken(job, job.GetIdTokenRequestToken(), "")
	require.Equal(t, http.StatusOK, status)
	newHeader, _ := c.verifyIDToken(after)
	assert.Equal(t, rot.Kid, newHeader["kid"])
}
