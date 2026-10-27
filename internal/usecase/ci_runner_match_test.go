// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestRunnerMatches(t *testing.T) {
	job := domain.CIJob{Labels: []string{"macos-latest"}}
	assert.False(t, runnerMatches(job, nil))
	assert.False(t, runnerMatches(job, []domain.CIRunner{{Labels: []string{"ubuntu-latest"}}}))
	assert.True(t, runnerMatches(job, []domain.CIRunner{{Labels: []string{"ubuntu-latest"}}, {Labels: []string{"MacOS-Latest"}}}))
}

const unmatchedMsg = "no registered runner matches runs-on: [gpu]"

// advanceWith plans the waiting jobs of a run of the workflow, with the given
// registered runners, and returns the jobs by key (the first leg of a matrix,
// "key#n" for the others).
func advanceWith(t *testing.T, yaml string, keys []string, runners []domain.CIRunner, listErr error) map[string]domain.CIJob {
	t.Helper()
	store := mocks.NewCIStore(t)
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).Maybe()
	store.EXPECT().ListRunners(mock.Anything).Return(runners, listErr).Maybe()
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	var jobs []domain.CIJob
	for i, k := range keys {
		jobs = append(jobs, domain.CIJob{
			ID: domain.NewJobID(1, i, 0), RunID: 1, Attempt: 1, RunAttempt: 1, Repo: repo,
			Key: k, Name: k, Phase: domain.CIPhaseWaiting, Index: i,
		})
	}
	rs := &runState{
		c:       &CI{cfg: CIConfig{Store: store, Clock: clock}, flows: newWfCache(4)},
		run:     domain.CIRun{ID: 1, Attempt: 1, Repo: repo, Event: "push", WorkflowPath: ".github/workflows/w.yml"},
		attempt: 1, jobs: jobs, src: &domain.CIRunSource{RunID: 1, Repo: repo, WorkflowYAML: yaml, GithubJSON: "{}"},
		dirty: map[string]bool{}, checks: map[string]bool{},
	}
	require.NoError(t, rs.advance(context.Background()))
	out := map[string]domain.CIJob{}
	seen := map[string]int{}
	for _, j := range rs.jobs {
		k := j.Key
		if n := seen[k]; n > 0 {
			k += "#" + string(rune('0'+n))
		}
		seen[k]++
		out[k] = j
	}
	return out
}

func TestAdvanceUnmatchedRunsOn(t *testing.T) {
	single := "on: push\njobs:\n  a:\n    runs-on: gpu\n    steps:\n      - run: echo a\n"
	chain := single + "  b:\n    needs: a\n    if: always()\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo b\n"
	matrix := "on: push\njobs:\n  a:\n    runs-on: gpu\n    strategy:\n      matrix:\n        n: [1, 2]\n    steps:\n      - run: echo a\n"
	gpu := domain.CIRunner{Name: "gpu", Labels: []string{"gpu"}}
	ubuntu := domain.CIRunner{Name: "u", Labels: []string{"ubuntu-latest"}}

	for _, tc := range []struct {
		name    string
		yaml    string
		keys    []string
		runners []domain.CIRunner
		listErr error
		check   func(t *testing.T, jobs map[string]domain.CIJob)
	}{
		{
			name: "an offline runner with the labels keeps the job queued", yaml: single, keys: []string{"a"}, runners: []domain.CIRunner{gpu},
			check: func(t *testing.T, jobs map[string]domain.CIJob) {
				assert.Equal(t, domain.CIPhaseReady, jobs["a"].Phase)
				assert.Empty(t, jobs["a"].Error)
			},
		},
		{
			name: "a failed listing keeps the job queued", yaml: single, keys: []string{"a"}, listErr: errors.New("db down"),
			check: func(t *testing.T, jobs map[string]domain.CIJob) {
				assert.Equal(t, domain.CIPhaseReady, jobs["a"].Phase)
				assert.Empty(t, jobs["a"].Error)
			},
		},
		{
			name: "no runner at all fails the job", yaml: single, keys: []string{"a"},
			check: func(t *testing.T, jobs map[string]domain.CIJob) {
				assert.Equal(t, domain.CIPhaseEnded, jobs["a"].Phase)
				assert.Equal(t, domain.CIFailure, jobs["a"].Conclusion)
				assert.Equal(t, unmatchedMsg, jobs["a"].Error)
			},
		},
		{
			name: "runners with other labels fail the job", yaml: single, keys: []string{"a"}, runners: []domain.CIRunner{ubuntu},
			check: func(t *testing.T, jobs map[string]domain.CIJob) {
				assert.Equal(t, domain.CIFailure, jobs["a"].Conclusion)
				assert.Equal(t, unmatchedMsg, jobs["a"].Error)
			},
		},
		{
			name: "a downstream if: always() job still runs", yaml: chain, keys: []string{"a", "b"}, runners: []domain.CIRunner{ubuntu},
			check: func(t *testing.T, jobs map[string]domain.CIJob) {
				assert.Equal(t, domain.CIFailure, jobs["a"].Conclusion)
				assert.Equal(t, domain.CIPhaseReady, jobs["b"].Phase)
				assert.Empty(t, jobs["b"].Error)
			},
		},
		{
			name: "a downstream job without always() is skipped", yaml: single + "  b:\n    needs: a\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo b\n",
			keys: []string{"a", "b"}, runners: []domain.CIRunner{ubuntu},
			check: func(t *testing.T, jobs map[string]domain.CIJob) {
				assert.Equal(t, domain.CIFailure, jobs["a"].Conclusion)
				assert.Equal(t, domain.CISkipped, jobs["b"].Conclusion)
			},
		},
		{
			name: "fail-fast cancels the other legs of a matrix", yaml: matrix, keys: []string{"a"},
			check: func(t *testing.T, jobs map[string]domain.CIJob) {
				require.Len(t, jobs, 2)
				assert.Equal(t, domain.CIFailure, jobs["a"].Conclusion)
				assert.Equal(t, unmatchedMsg, jobs["a"].Error)
				assert.Equal(t, domain.CICancelled, jobs["a#1"].Conclusion)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, advanceWith(t, tc.yaml, tc.keys, tc.runners, tc.listErr))
		})
	}
}
