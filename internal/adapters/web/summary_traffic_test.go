// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

type summaryTrafficReader struct {
	trafficRecorder
	counts domain.TrafficCounts
	err    error
	repos  []domain.RepoName
}

func (f *summaryTrafficReader) RepoSummary(_ context.Context, repo domain.RepoName) (domain.TrafficCounts, error) {
	f.repos = append(f.repos, repo)
	return f.counts, f.err
}

func TestSummaryTrafficRows(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"enabled", "zero", "nil", "failed", "recorder only"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			repo := publicRepo(t, "proj")
			browse := mocks.NewBrowseService(t)
			browse.EXPECT().ListRefs(mock.Anything, mock.Anything, repo.Name, mock.Anything).Return(ports.RefPage{Refs: []domain.Ref{{Name: "refs/heads/main", ShortName: "main", Kind: domain.RefKindBranch, CommitID: objectID(t, mainTip)}}}, nil)
			browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repo.Name, mock.Anything).Return(ports.HistoryPage{}, nil)
			browse.EXPECT().GetTree(mock.Anything, mock.Anything, repo.Name, mock.Anything, mock.Anything, false).Return(domain.Tree{}, nil).Maybe()
			f := &summaryTrafficReader{counts: domain.TrafficCounts{Views: 1204, ViewUniques: 312, Clones: 87, CloneUniques: 14}}
			cfg := Config{Repos: reposServing(t, repo), Browse: browse}
			switch mode {
			case "enabled":
				cfg.Traffic = f
			case "zero":
				f.counts = domain.TrafficCounts{}
				cfg.Traffic = f
			case "failed":
				f.err = errors.New("unavailable")
				cfg.Traffic = f
			case "recorder only":
				cfg.Traffic = &trafficRecorder{}
			}
			srv := newTestServer(t, cfg)
			r := httptest.NewRequest(http.MethodGet, "/proj", nil)
			r.SetPathValue("repo", "proj")
			component, status, err := srv.summary(t.Context(), r)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, status)
			out := renderToString(t, component)
			switch mode {
			case "enabled":
				assert.Contains(t, out, "<dt>views (30d)</dt><dd>1\u202f204 · 312 unique</dd>")
				assert.Contains(t, out, "<dt>clones (30d)</dt><dd>87 · 14 unique</dd>")
			case "zero":
				assert.Contains(t, out, "<dt>views (30d)</dt><dd>0 · 0 unique</dd>")
				assert.Contains(t, out, "<dt>clones (30d)</dt><dd>0 · 0 unique</dd>")
			default:
				assert.NotContains(t, out, "views (30d)")
				assert.NotContains(t, out, "clones (30d)")
			}
			if mode == "enabled" || mode == "zero" || mode == "failed" {
				assert.Equal(t, []domain.RepoName{repo.Name}, f.repos)
			}
		})
	}
}
