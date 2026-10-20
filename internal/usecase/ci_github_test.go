// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
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
	ctx := context.Background()
	run := func(id int64, attempt int32) domain.CIRun { return domain.CIRun{ID: id, Attempt: attempt} }
	seen := map[int64]string{}
	for _, id := range []int64{1, 2, 1<<27 - 1} {
		for _, attempt := range []int32{1, 2, 127} {
			for _, index := range []int{0, 1, 1023} {
				for _, combo := range []int{0, 1, 255, 511} {
					got := usecase.CheckRunID(ctx, run(id, attempt), index, combo)
					require.NotZero(t, got)
					assert.Less(t, got, int64(1)<<53)
					key := fmt.Sprintf("run %d attempt %d job %d leg %d", id, attempt, index, combo)
					assert.NotContains(t, seen, got, "%s collides", key)
					seen[got] = key
				}
			}
		}
	}
	a := usecase.CheckRunID(ctx, run(7, 1), 0, 0)
	b := usecase.CheckRunID(ctx, run(7, 2), 0, 0)
	assert.NotEqual(t, a, b, "a re-run gets new ids")
	again := usecase.CheckRunID(ctx, run(7, 1), 0, 0)
	assert.Equal(t, a, again, "stable per attempt")

	// In range, the layout is fixed: leg in the low bits, then job, attempt, run.
	assert.Equal(t, int64(5)<<26|int64(3)<<19|int64(2)<<9|1, usecase.CheckRunID(ctx, run(5, 3), 2, 1))

	// Out of range, the id is 0 and the overflow is logged at warn, never an
	// error: the key is informational.
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
		var logs bytes.Buffer
		lctx := zerowrap.WithCtx(ctx, zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &logs}))
		assert.Zero(t, usecase.CheckRunID(lctx, c.run, c.index, c.combo), name)
		assert.Contains(t, logs.String(), `"level":"warn"`, name)
		assert.Contains(t, logs.String(), `"run_id":`+strconv.FormatInt(c.run.ID, 10), name)
		assert.Contains(t, logs.String(), `"attempt":`+strconv.Itoa(int(c.run.Attempt)), name)
		assert.Contains(t, logs.String(), `"job_index":`+strconv.Itoa(c.index), name)
	}
}

func TestGitHubRepoReportsAllowAutoMerge(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			repos := mocks.NewGitRepos(t)
			u := newCIUnit(t, func(cfg *usecase.CIConfig) { cfg.Repos = repos })
			repo := domain.Repo{Name: name(t, "proj"), Visibility: domain.VisibilityPublic, Settings: domain.DefaultRepoSettings()}
			repo.Settings.AllowAutoMerge = allow
			u.meta.EXPECT().Get(mock.Anything, repo.Name).Return(repo, nil)
			repos.EXPECT().HeadBranch(mock.Anything, repo.Name).Return("main", nil)
			data, err := u.ci.GetRepo(context.Background(), domain.Root(), "owner", "proj")
			require.NoError(t, err)
			var obj map[string]any
			require.NoError(t, json.Unmarshal(data, &obj))
			assert.Equal(t, allow, obj["allow_auto_merge"])
		})
	}
}
