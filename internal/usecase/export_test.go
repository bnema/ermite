// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/rsa"
	"slices"
	"strconv"
	"time"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Sweep exposes the daily sweep to the tests.
func (c *CI) Sweep(ctx context.Context) { c.sweep(ctx) }

// TouchPushedAt exposes touchPushedAt to the tests.
func TouchPushedAt(ctx context.Context, meta ports.RepoMetaStore, name domain.RepoName, now time.Time) (domain.Repo, error) {
	return touchPushedAt(ctx, meta, name, now)
}

// TouchActiveAt exposes touchActiveAt to the tests.
func TouchActiveAt(ctx context.Context, meta ports.RepoMetaStore, name domain.RepoName, now time.Time) (domain.Repo, error) {
	return touchActiveAt(ctx, meta, name, now)
}

// RunTitle gives the title of a run as startRun sets it: the evaluated
// run-name, else commitTitle.
func (c *CI) RunTitle(ctx context.Context, wf *workflow.Workflow, githubJSON, commitTitle string) string {
	if name, ok := c.evalRunName(ctx, wf, githubJSON, expr.NewDictionary()); ok {
		return name
	}
	return commitTitle
}

// CheckRunID exposes checkRunID to the tests.
func CheckRunID(ctx context.Context, run domain.CIRun, index, combo int) int64 {
	return checkRunID(ctx, run, index, combo)
}

// SecretUse is the result of SecretsUsedBy.
type SecretUse = secretUse

// SecretsUsedBy exposes secretsUsedBy to the tests.
func SecretsUsedBy(workflowYAML, jobID string) SecretUse {
	return secretsUsedBy(workflowYAML, jobID)
}

// SecretWalks is the number of YAML walks made so far.
func SecretWalks() int64 { return secretWalks.Load() }

// SecretUseCache is a wfCache exposed to the tests.
type SecretUseCache struct{ c *wfCache }

// NewSecretUseCache builds a cache for up to max runs.
func NewSecretUseCache(max int) *SecretUseCache { return &SecretUseCache{newWfCache(max)} }

// Use is wfCache.secretUse.
func (s *SecretUseCache) Use(runID int64, yaml, jobKey string) SecretUse {
	return s.c.secretUse(runID, yaml, jobKey)
}

// Drop is wfCache.drop.
func (s *SecretUseCache) Drop(runID int64) { s.c.drop(runID) }

// EvalConcurrency exposes evalConcurrency to the tests.
func EvalConcurrency(ctx context.Context, tok template.Token, env template.Env) (domain.CIConcurrency, error) {
	return evalConcurrency(ctx, tok, env)
}

// MatchPullRequest exposes the pull_request filter to the tests.
func MatchPullRequest(wf *workflow.Workflow, action, base string, files []string, truncated bool) bool {
	return matchPullRequest(wf, "pull_request", action, base, files, truncated)
}

// MatchPullRequestTarget exposes the pull_request_target filter to the tests.
func MatchPullRequestTarget(wf *workflow.Workflow, action, base string, files []string, truncated bool) bool {
	return matchPullRequest(wf, "pull_request_target", action, base, files, truncated)
}

// MatchWorkflowRun exposes the workflow_run filter to the tests.
func MatchWorkflowRun(wf *workflow.Workflow, name, action, headBranch string) bool {
	return matchWorkflowRun(wf, name, action, headBranch)
}

// ResolveDispatchInputs exposes resolveDispatchInputs to the tests.
func ResolveDispatchInputs(defs []workflow.Input, given map[string]string) (map[string]string, error) {
	return resolveDispatchInputs(defs, given)
}

// PullFilesLink exposes the Link header of the files endpoint to the tests.
func PullFilesLink(base string, page, perPage, total int) string {
	return pullFilesLink(base, page, perPage, total)
}

// FilePatch exposes the patch text of a file of the files endpoint to the tests.
func FilePatch(f domain.FileDiff) (string, bool) { return filePatch(f) }

// PullTipsTrusted exposes the trust check of an activity of a merge request to
// the tests, on a CI that has cfg and no workers.
func PullTipsTrusted(ctx context.Context, cfg CIConfig, repo domain.RepoName, ev ports.PullRequestEvent) (bool, domain.ObjectID) {
	return (&CI{cfg: cfg}).pullTipsTrusted(ctx, repo, ev)
}

// Thumbprint exposes thumbprint to the tests.
func Thumbprint(pub *rsa.PublicKey) string { return thumbprint(pub) }

// IDTokenClaims gives the claims idTokenClaims builds, decoded.
func IDTokenClaims(contextsJSON, environment, issuer, audience string, at time.Time) (map[string]any, []string, error) {
	claims, err := idTokenClaims(contextsJSON, environment, idTokenMeta{issuer: issuer, audience: audience, jti: "jti", now: at})
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]any, len(claims))
	keys := make([]string, 0, len(claims))
	for _, p := range claims {
		out[p.k] = p.v
		keys = append(keys, p.k)
	}
	return out, keys, nil
}

// SignForTest signs claims (in the order of their keys) with the signing key.
func (o *OIDC) SignForTest(claims map[string]any) (string, error) {
	keys := make([]string, 0, len(claims))
	for k := range claims {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make(ordered, 0, len(keys))
	for _, k := range keys {
		out = append(out, pair{k, claims[k]})
	}
	return o.sign(out)
}

// DifficultyFor exposes the adaptive difficulty with the production base and
// cap to the tests.
func DifficultyFor(perAddr, global int) int {
	return difficultyFor(baseDifficulty, maxDifficulty, perAddr, global)
}

// Production parameters of the submission, for the tests.
const (
	BaseDifficulty = baseDifficulty
	MaxDifficulty  = maxDifficulty
	MinFillTime    = minFillTime
	ChallengeTTL   = challengeTTL
)

// SpentLen is the number of nonces the submission remembers as spent.
func (s *IssueSubmissions) SpentLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.spent)
}

// FillSpent fills the spent set with n unexpired nonces.
func (s *IssueSubmissions) FillSpent(n int, exp time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range n {
		s.spent["fill"+strconv.Itoa(i)] = exp
	}
}

// MaxSpent is the bound of the spent set.
const MaxSpent = maxSpent

// SetAfterMergeGate makes the post-merge goroutine of m run f before anything
// else, to place a test between a merge and its side effects.
func (m *MergeRequests) SetAfterMergeGate(f func()) { m.afterGate = f }

// ScheduledRetries gives the number of retries waiting in the scheduler.
func (s *Mergeability) ScheduledRetries() int {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	return len(s.heap)
}

// RetryAttempts gives the number of keys whose failed attempts are counted.
func (s *Mergeability) RetryAttempts() int {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	return len(s.retries)
}
