// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/usecase"
)

func TestMatchWorkflowRun(t *testing.T) {
	def := parsePR(t, "  workflow_run:\n    workflows: [ci, 'Build and test']\n")
	assert.True(t, usecase.MatchWorkflowRun(def, "ci", "completed", "main"))
	assert.True(t, usecase.MatchWorkflowRun(def, "Build and test", "requested", "x/y"), "no types: both activities")
	assert.False(t, usecase.MatchWorkflowRun(def, "other", "completed", "main"), "the name must be listed")
	assert.False(t, usecase.MatchWorkflowRun(def, "CI", "completed", "main"), "names are exact")
	assert.False(t, usecase.MatchWorkflowRun(def, "ci", "in_progress", "main"), "ermite has no in_progress activity")
	assert.False(t, usecase.MatchWorkflowRun(parsePR(t, "  push:\n"), "ci", "completed", "main"))

	typed := parsePR(t, "  workflow_run:\n    workflows: [ci]\n    types: [completed]\n")
	assert.True(t, usecase.MatchWorkflowRun(typed, "ci", "completed", "main"))
	assert.False(t, usecase.MatchWorkflowRun(typed, "ci", "requested", "main"))

	branches := parsePR(t, "  workflow_run:\n    workflows: [ci]\n    branches: [main, 'release/**']\n")
	assert.True(t, usecase.MatchWorkflowRun(branches, "ci", "completed", "main"))
	assert.True(t, usecase.MatchWorkflowRun(branches, "ci", "completed", "release/1.0"))
	assert.False(t, usecase.MatchWorkflowRun(branches, "ci", "completed", "feature"), "the head branch of the triggering run is filtered")

	ignore := parsePR(t, "  workflow_run:\n    workflows: [ci]\n    branches-ignore: [wip/**]\n")
	assert.True(t, usecase.MatchWorkflowRun(ignore, "ci", "completed", "main"))
	assert.False(t, usecase.MatchWorkflowRun(ignore, "ci", "completed", "wip/x"))
}
