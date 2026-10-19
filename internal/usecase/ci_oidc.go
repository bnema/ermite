// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The ID tokens of CI jobs (CI-4, D5). A job that may have them (a trusted run
// and id-token: write, domain.IDTokenAllowed) gets with its assignment the
// token endpoint and a request token of its own: a job repository token
// (CIRepoToken.OIDC) that lives and dies with the job as its github.token does,
// and that nothing but the token endpoint accepts. The runner gives both to
// the steps as ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN,
// as GitHub's runner does; never ACTIONS_RUNTIME_TOKEN.

var _ ports.OIDCService = (*CI)(nil)

// issueIDTokenRequest adds the ID token request token of j to the next flush
// and returns its secret, or "" when the job may not request ID tokens or the
// server issues none. j must be assigned (its AssignedAt is set).
func (rs *runState) issueIDTokenRequest(j *domain.CIJob) string {
	c := rs.c
	if !c.cfg.OIDC.Enabled() || !domain.IDTokenAllowed(j.Permissions, rs.run.Trusted) {
		return ""
	}
	now := c.now()
	at := *j
	if at.AssignedAt.IsZero() {
		at.AssignedAt = now
	}
	secret := newToken(domain.OIDCRequestTokenPrefix)
	t := c.repoTokenExpiry(at)
	t.ID, t.Hash, t.Repo, t.OIDC = newRepoTokenID(), domain.HashTokenSecret(secret), rs.run.Repo, true
	t.RunID, t.RunAttempt, t.JobID, t.CreatedAt = j.RunID, j.RunAttempt, j.ID, now
	rs.tokens = append(rs.tokens, t)
	return secret
}

// OIDCEnabled reports whether the server issues ID tokens.
func (c *CI) OIDCEnabled() bool { return c.cfg.OIDC.Enabled() }

// OIDCDiscovery returns the OpenID Provider configuration.
func (c *CI) OIDCDiscovery() ([]byte, error) {
	return c.cfg.OIDC.Discovery()
}

// OIDCKeys returns the JWKS.
func (c *CI) OIDCKeys() ([]byte, error) {
	return c.cfg.OIDC.JWKS()
}

// IDToken signs an ID token for the job whose request token is secret. The
// token must exist (it is deleted when the job ends), not have expired, and
// its job must be assigned or running, still allowed to have ID tokens.
// audience "" is the default, "<public URL>/<owner>" as GitHub's
// "https://github.com/<owner>".
func (c *CI) IDToken(ctx context.Context, secret, audience string) (string, error) {
	o := c.cfg.OIDC
	if !o.Enabled() {
		return "", domain.ErrNotFound
	}
	if len(audience) > domain.MaxOIDCAudienceLen {
		return "", &domain.InvalidFieldsError{Fields: []domain.FieldViolation{{Field: "audience", Reason: fmt.Sprintf("must be at most %d bytes", domain.MaxOIDCAudienceLen)}}}
	}
	if !domain.IsOIDCRequestToken(secret) {
		return "", domain.ErrUnauthenticated
	}
	t, err := o.cfg.Tokens.FindRepoTokenByHash(ctx, domain.HashTokenSecret(secret))
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return "", domain.ErrUnauthenticated
	case err != nil:
		return "", fmt.Errorf("find the id token request token: %w", err)
	}
	now := c.now()
	if !t.OIDC || t.Expired(now) {
		return "", domain.ErrUnauthenticated
	}
	rs, err := c.loadRun(ctx, t.RunID, t.RunAttempt)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return "", domain.ErrUnauthenticated
	case err != nil:
		return "", err
	}
	j := rs.find(t.JobID)
	if j == nil || !j.Active() || !domain.IDTokenAllowed(j.Permissions, rs.run.Trusted) {
		return "", domain.ErrUnauthenticated
	}
	if audience == "" {
		audience = c.cfg.PublicURL + "/" + c.cfg.Owner
	}
	claims, err := idTokenClaims(j.ContextsJSON, j.Environment, idTokenMeta{
		issuer: o.Issuer(), audience: audience, jti: newJTI(), now: now,
	})
	if err != nil {
		return "", err
	}
	return o.sign(claims)
}

// idTokenMeta are the claims of a token that do not come from the job.
type idTokenMeta struct {
	issuer, audience, jti string
	now                   time.Time
}

// idTokenClaims builds the claims of a job's ID token from the contexts the
// job was given (Job.contexts): the github context for the run, the job
// context for the workflow that defines the job (the called one for a job of a
// reusable workflow: job_workflow_ref), as GitHub's tokens. The order is the one
// of GitHub's example token.
func idTokenClaims(contextsJSON, environment string, t idTokenMeta) (ordered, error) {
	var ctxs struct {
		GitHub map[string]any `json:"github"`
		Job    map[string]any `json:"job"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(contextsJSON)))
	dec.UseNumber()
	if err := dec.Decode(&ctxs); err != nil {
		return nil, fmt.Errorf("the contexts of the job are not valid: %w", err)
	}
	gh := func(k string) string { return claimString(ctxs.GitHub[k]) }
	job := func(k string) string { return claimString(ctxs.Job[k]) }
	claims := ordered{
		{"jti", t.jti},
		{"sub", domain.OIDCSubject(gh("repository"), gh("event_name"), environment, gh("ref"))},
	}
	if environment != "" {
		claims = append(claims, pair{"environment", environment})
	}
	iat := t.now.Unix()
	claims = append(claims, ordered{
		{"aud", t.audience},
		{"ref", gh("ref")}, {"sha", gh("sha")},
		{"repository", gh("repository")}, {"repository_owner", gh("repository_owner")},
		{"repository_owner_id", gh("repository_owner_id")},
		{"run_id", gh("run_id")}, {"run_number", gh("run_number")}, {"run_attempt", gh("run_attempt")},
		{"repository_visibility", gh("repository_visibility")}, {"repository_id", gh("repository_id")},
		{"actor_id", gh("actor_id")}, {"actor", gh("actor")}, {"workflow", gh("workflow")},
		{"head_ref", gh("head_ref")}, {"base_ref", gh("base_ref")}, {"event_name", gh("event_name")},
		{"ref_protected", gh("ref_protected")}, {"ref_type", gh("ref_type")},
		{"workflow_ref", gh("workflow_ref")}, {"workflow_sha", gh("workflow_sha")},
		{"job_workflow_ref", job("workflow_ref")}, {"job_workflow_sha", job("workflow_sha")},
		{"check_run_id", job("check_run_id")},
		{"runner_environment", domain.OIDCRunnerEnvironment},
		{"iss", t.issuer},
		{"nbf", iat - int64(domain.OIDCNotBeforeSkew/time.Second)},
		{"exp", iat + int64(domain.OIDCTokenLifetime/time.Second)},
		{"iat", iat},
	}...)
	return claims, nil
}

// claimString is a context value as GitHub puts it in a claim: a string, numbers
// and booleans written out, nothing for null.
func claimString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

// newJTI is a random UUID (version 4), the shape of GitHub's jti.
func newJTI() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
