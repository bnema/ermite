// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// MirrorBadge is an administrative diagnostic in the summary rail.
type MirrorBadge struct{ State, Reason, Command string }

// WithMirrorWarner enables mirror badges. Configure before serving requests.
func (s *Server) WithMirrorWarner(w ports.MirrorWarner) *Server {
	s.mirrors = w
	return s
}

// mirrorBadge selects presentation only; the source authorizes the lookup and reason.
//
//hexcheck:ignore suspicious-business-logic-in-adapter controls visibility of an administrative UI indicator
func (s *Server) mirrorBadge(ctx context.Context, repo domain.RepoName) *MirrorBadge {
	p := principal(ctx)
	if s.mirrors == nil || !p.Allows(domain.ScopeRepoAdmin, repo) {
		return nil
	}
	state, reason := s.mirrors.Badge(ctx, p, repo)
	if state != domain.MirrorFailing && state != domain.MirrorPaused {
		return nil
	}
	return &MirrorBadge{State: string(state), Reason: reason, Command: "ermite mirror status " + repo.String()}
}
