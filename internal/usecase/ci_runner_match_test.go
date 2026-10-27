// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/domain"
)

func TestRunnerMatches(t *testing.T) {
	job := domain.CIJob{Labels: []string{"macos-latest"}}
	assert.False(t, runnerMatches(job, nil))
	assert.False(t, runnerMatches(job, []domain.CIRunner{{Labels: []string{"ubuntu-latest"}}}))
	assert.True(t, runnerMatches(job, []domain.CIRunner{{Labels: []string{"ubuntu-latest"}}, {Labels: []string{"MacOS-Latest"}}}))
	assert.Equal(t, "no registered runner matches runs-on: [a, b]", noRunnerMessage([]string{"a", "b"}))
}
