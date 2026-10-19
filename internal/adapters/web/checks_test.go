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
	single := checksSection(repo, []ports.CheckView{a, c})
	require.Len(t, single.Rows, 2)
	assert.Equal(t, []CheckRow{
		{Name: "build", Kind: "ok", State: "success", Href: runHref("proj", 7)},
		{Name: "test", Kind: "ok", State: "success", Href: runHref("proj", 7)},
	}, single.Rows)
	var flat bytes.Buffer
	require.NoError(t, MRChecks(single).Render(context.Background(), &flat))
	assert.NotContains(t, flat.String(), "ck-group")

	multi := checksSection(repo, []ports.CheckView{a, b, c})
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
	fallback := checksSection(repo, []ports.CheckView{a, b})
	assert.Equal(t, "Smoke · run 8", fallback.Rows[1].GroupLabel)
}
