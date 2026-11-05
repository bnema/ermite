// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func TestChecksSectionRuns(t *testing.T) {
	t.Parallel()
	repo := domain.Repo{Name: repoName(t, "proj")}
	check := func(run int64, name, event string) ports.CheckView {
		return ports.CheckView{CICheck: domain.CICheck{RunID: run, Name: name, WorkflowName: "Smoke", Status: domain.CICompleted, Conclusion: domain.CISuccess}, Event: event}
	}
	a, b, c := check(7, "build", "push"), check(8, "build", "pull_request"), check(7, "test", "push")
	single := checksSection(repo, []ports.CheckView{a, c}, nil)
	require.Len(t, single.Rows, 2)
	assert.Equal(t, []CheckRow{
		{Name: "build", Kind: "ok", State: "success", Href: runHref("proj", 7)},
		{Name: "test", Kind: "ok", State: "success", Href: runHref("proj", 7)},
	}, single.Rows)
	var flat bytes.Buffer
	require.NoError(t, MRChecks(single).Render(context.Background(), &flat))
	assert.NotContains(t, flat.String(), "ck-group")

	multi := checksSection(repo, []ports.CheckView{a, b, c}, nil)
	require.Len(t, multi.Rows, 3)
	assert.Equal(t, "Smoke · push", multi.Rows[0].GroupLabel)
	assert.Equal(t, "test", multi.Rows[1].Name)
	assert.Empty(t, multi.Rows[1].GroupLabel)
	assert.Equal(t, "Smoke · pull_request", multi.Rows[2].GroupLabel)
	assert.Equal(t, runHref("proj", 8), multi.Rows[2].GroupHref)
	var html bytes.Buffer
	require.NoError(t, MRChecks(multi).Render(context.Background(), &html))
	assert.Contains(t, html.String(), `Checks <span class="count">3</span>`)
	assert.Contains(t, html.String(), `<li class="ck-group"><a href="`+runHref("proj", 7)+`">Smoke · push</a></li>`)
	t.Log(html.String())
	b.Event = ""
	fallback := checksSection(repo, []ports.CheckView{a, b}, nil)
	assert.Equal(t, "Smoke · run 8", fallback.Rows[1].GroupLabel)
}

func TestChecksSectionDeps(t *testing.T) {
	t.Parallel()
	name := repoName(t, "proj")
	repo := domain.Repo{Name: name}
	batch := func(st domain.DepsState, run int64) domain.DepBatch {
		return domain.DepBatch{State: st, RunID: run, MR: 4}
	}
	tests := []struct {
		name string
		b    domain.DepBatch
		pos  int
		want *CheckRow
		note string
	}{
		{"queued", batch(domain.DepsQueued, 0), 2, &CheckRow{Name: "Dependencies", Kind: "queued", State: "queued (#2)"}, "CI runs after the dependency run finishes."},
		{"drafted", batch(domain.DepsDrafted, 0), 1, &CheckRow{Name: "Dependencies", Kind: "queued", State: "queued (#1)"}, "CI runs after the dependency run finishes."},
		{"applying", batch(domain.DepsApplying, 9), 0, &CheckRow{Name: "Dependencies", Kind: "running", State: "running", Href: runHref("proj", 9)}, "CI runs after the dependency run finishes."},
		{"failed", batch(domain.DepsFailed, 9), 0, &CheckRow{Name: "Dependencies", Kind: "bad", State: "failed", Href: runHref("proj", 9)}, "the dependency run failed: Bernard holds CI on this commit."},
		{"failed without run", batch(domain.DepsFailed, 0), 0, &CheckRow{Name: "Dependencies", Kind: "bad", State: "failed"}, "the dependency run failed: Bernard holds CI on this commit."},
		{"idle", batch(domain.DepsIdle, 9), 0, nil, "no check on this commit: nothing blocks the merge."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row := depsCheckRow(name, tt.b, tt.pos)
			assert.Equal(t, tt.want, row)
			sec := checksSection(repo, nil, row)
			assert.Equal(t, tt.note, sec.Note)
			var html bytes.Buffer
			require.NoError(t, MRChecks(sec).Render(context.Background(), &html))
			if row != nil {
				assert.Contains(t, html.String(), ">Dependencies<")
				assert.NotContains(t, html.String(), "nothing blocks the merge")
				sum := checksSummary(name, "", nil)
				sum.Deps = row
				var bar bytes.Buffer
				require.NoError(t, MRChecksSum(sum).Render(context.Background(), &bar))
				assert.Contains(t, bar.String(), "dependencies "+row.State)
			}
		})
	}
}
