// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
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
