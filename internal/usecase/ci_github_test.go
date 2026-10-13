// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

// The title of a run is the run-name when it evaluates to something, else the
// commit title; a run-name is cut to its first line and 256 characters.
func TestRunTitle(t *testing.T) {
	t.Parallel()
	const commit = "fix: the commit title"
	const githubJSON = `{"ref_name":"main","event":{"head_commit":{"message":"one\ntwo"}}}`
	tests := []struct {
		name    string
		runName string // the run-name expression, "" : the workflow has none
		want    string
	}{
		{"no run-name", "", commit},
		{"a run-name", "deploy ${{ github.ref_name }}", "deploy main"},
		{"an evaluation error", "${{ fromJSON('{') }}", commit},
		{"an empty result", "${{ '' }}", commit},
		{"a blank result", "${{ '   ' }}", commit},
		{"a first line only", "${{ github.event.head_commit.message }}", "one"},
		{"cut at 256 characters", strings.Repeat("é", 300), strings.Repeat("é", 256)},
		{"exactly 256 characters", strings.Repeat("a", 256), strings.Repeat("a", 256)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := newCIUnit(t, nil)
			yaml := "on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: 'true'\n"
			if tt.runName != "" {
				yaml = "run-name: \"" + strings.ReplaceAll(strings.ReplaceAll(tt.runName, `\`, `\\`), `"`, `\"`) + "\"\n" + yaml
			}
			wf, errs := workflow.Parse(".github/workflows/a.yml", []byte(yaml))
			require.Empty(t, errs)
			require.NotNil(t, wf)
			assert.Equal(t, tt.want, u.ci.RunTitle(context.Background(), wf, githubJSON, commit))
		})
	}
}

// The check run id keeps the run, its attempt, the job and the leg apart, stays
// below 2^53, and refuses what does not fit instead of wrapping.
func TestCheckRunID(t *testing.T) {
	t.Parallel()
	run := func(id int64, attempt int32) domain.CIRun { return domain.CIRun{ID: id, Attempt: attempt} }
	seen := map[int64]string{}
	for _, id := range []int64{1, 2, 1<<27 - 1} {
		for _, attempt := range []int32{1, 2, 127} {
			for _, index := range []int{0, 1, 1023} {
				for _, combo := range []int{0, 1, 255, 511} {
					got, err := usecase.CheckRunID(run(id, attempt), index, combo)
					require.NoError(t, err)
					assert.Less(t, got, int64(1)<<53)
					key := fmt.Sprintf("run %d attempt %d job %d leg %d", id, attempt, index, combo)
					assert.NotContains(t, seen, got, "%s collides", key)
					seen[got] = key
				}
			}
		}
	}
	a, _ := usecase.CheckRunID(run(7, 1), 0, 0)
	b, _ := usecase.CheckRunID(run(7, 2), 0, 0)
	assert.NotEqual(t, a, b, "a re-run gets new ids")
	again, _ := usecase.CheckRunID(run(7, 1), 0, 0)
	assert.Equal(t, a, again, "stable per attempt")

	for name, c := range map[string]struct {
		run          domain.CIRun
		index, combo int
	}{
		"job index":    {run(1, 1), 1024, 0},
		"negative job": {run(1, 1), -1, 0},
		"leg":          {run(1, 1), 0, 512},
		"attempt":      {run(1, 128), 0, 0},
		"run id":       {run(1<<27, 1), 0, 0},
		"huge run id":  {run(math.MaxInt64, 1), 0, 0},
		"negative run": {run(-1, 1), 0, 0},
	} {
		_, err := usecase.CheckRunID(c.run, c.index, c.combo)
		assert.Error(t, err, name)
	}
}
