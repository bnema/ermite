// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// AuthorOf exposes authorOf to the tests.
func AuthorOf(p domain.Principal) string { return authorOf(p) }

// Sweep exposes the daily sweep to the tests.
func (c *CI) Sweep(ctx context.Context) { c.sweep(ctx) }

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
	return matchPullRequest(wf, action, base, files, truncated)
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
