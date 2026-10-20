// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestMirrorBadge(t *testing.T) {
	repo := repoName(t, "proj")
	admin := domain.TokenPrincipal(domain.Token{ID: "admin", Scopes: []domain.Scope{domain.ScopeRepoAdmin}, Repos: []domain.RepoName{repo}})
	reader := domain.TokenPrincipal(domain.Token{ID: "reader", Scopes: []domain.Scope{domain.ScopeRepoRead}})
	for _, tc := range []struct {
		name   string
		p      domain.Principal
		state  domain.MirrorState
		shown  bool
		lookup bool
	}{
		{"owner failing", domain.Owner(), domain.MirrorFailing, true, true},
		{"admin paused", admin, domain.MirrorPaused, true, true},
		{"anonymous", domain.Anonymous(), domain.MirrorFailing, false, false},
		{"reader", reader, domain.MirrorFailing, false, false},
		{"ok", admin, domain.MirrorOK, false, true},
		{"disabled", admin, domain.MirrorDisabled, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := mocks.NewMirrorWarner(t)
			if tc.lookup {
				w.EXPECT().Badge(mock.Anything, tc.p, repo).Return(tc.state, "main diverged on remote")
			}
			s := (&Server{}).WithMirrorWarner(w)
			badge := s.mirrorBadge(domain.ContextWithPrincipal(t.Context(), tc.p), repo)
			out := renderRail(t, SummaryPage{Mirror: badge})
			if tc.shown {
				assert.Contains(t, out, `aria-labelledby="r-mirror"`)
				assert.Contains(t, out, "Mirror "+string(tc.state))
				assert.Contains(t, out, "main diverged on remote")
				assert.Contains(t, out, "ermite mirror status proj")
			} else {
				assert.Nil(t, badge)
				assert.NotContains(t, out, `id="r-mirror"`)
			}
		})
	}
	assert.Nil(t, (&Server{}).mirrorBadge(domain.ContextWithPrincipal(t.Context(), domain.Owner()), repo))
}
