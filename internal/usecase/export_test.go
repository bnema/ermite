// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"

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
	if name, ok := c.evalRunName(ctx, wf, githubJSON); ok {
		return name
	}
	return commitTitle
}

// CheckRunID exposes checkRunID to the tests.
func CheckRunID(ctx context.Context, run domain.CIRun, index, combo int) int64 {
	return checkRunID(ctx, run, index, combo)
}
