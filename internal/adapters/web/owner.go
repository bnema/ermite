// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"

	"bnema.dev/ermite/internal/ports"

	"bnema.dev/zerowrap"
)

// ownerNav is the navigation of the owner's spaces with current shown, or nil
// when the request is not the signed-in owner's or the inbox is not there. A
// failed read of the counts leaves the links without a number.
func (s *Server) ownerNav(ctx context.Context, current string) *OwnerNav {
	if !SignedIn(ctx) || s.cfg.Inbox == nil {
		return nil
	}
	c, err := s.cfg.Inbox.Counts(ctx, principal(ctx))
	if err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Msg("web owner nav counts failed")
		c = ports.OwnerCounts{Attention: -1, Work: -1}
	}
	return s.ownerNavWith(ctx, current, c)
}

// ownerNavWith is ownerNav with counts already read; negative counts show no
// number.
func (s *Server) ownerNavWith(ctx context.Context, current string, c ports.OwnerCounts) *OwnerNav {
	if !SignedIn(ctx) || s.cfg.Inbox == nil {
		return nil
	}
	n := &OwnerNav{Current: current, Deps: s.cfg.Deps != nil, Attention: c.Attention, Work: c.Work}
	if _, ok := s.cfg.Traffic.(TrafficForgeReader); ok {
		n.Insights = true
	}
	return n
}
