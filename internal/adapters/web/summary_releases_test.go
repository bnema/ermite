// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestSummaryRelease(t *testing.T) {
	repo := repoName(t, "proj")
	for _, tc := range []struct {
		name    string
		release domain.Release
		err     error
		visible bool
	}{
		{name: "published", release: domain.Release{Tag: "v0.9.1", PublishedAt: fixedNow.Add(-6 * time.Minute)}, visible: true},
		{name: "none", err: domain.ErrNotFound},
		{name: "draft", release: domain.Release{Tag: "secret", Draft: true, PublishedAt: fixedNow}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := mocks.NewReleaseService(t)
			svc.EXPECT().GetRelease(mock.Anything, mock.Anything, repo, ports.ReleaseRef{Latest: true}).Return(tc.release, tc.err).Once()
			srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "proj")), Browse: mocks.NewBrowseService(t), Releases: svc})
			rel := srv.summaryRelease(t.Context(), repo, fixedNow)
			out := strings.Join(strings.Fields(renderRail(t, SummaryPage{Latest: &LatestTag{Name: "v1", Count: 1}, Release: rel})), " ")
			if !tc.visible {
				assert.Nil(t, rel)
				assert.NotContains(t, out, "<dt>release</dt>")
				return
			}
			require.NotNil(t, rel)
			assert.Contains(t, out, `<a href="/proj/releases/tag/v0.9.1">v0.9.1</a> · <time`)
			assert.Contains(t, out, `datetime="`+rfc3339(tc.release.PublishedAt)+`"`)
			assert.Contains(t, out, `>`+ago(fixedNow, tc.release.PublishedAt)+`</time> · <a href="/proj/releases">all releases</a>`)
			assert.Regexp(t, `(?s)<dt>latest tag</dt>.*<dt>release</dt>`, out)
		})
	}
}
