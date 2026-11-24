// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"time"

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

// ownerLayout is the layout of an owner page: its title, the navigation with
// key current, the command palette and the live stream of the view; query
// follows view=key in the stream URL. counts, when given, are the navigation
// counts the page already read.
func (s *Server) ownerLayout(ctx context.Context, key, title, query string, counts ...ports.OwnerCounts) Layout {
	l := s.base()
	l.Title = title + " · " + s.instance
	if len(counts) > 0 {
		l.OwnerNav = s.ownerNavWith(ctx, key, counts[0])
	} else {
		l.OwnerNav = s.ownerNav(ctx, key)
	}
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	if s.cfg.Events != nil {
		q := "view=" + key
		if query != "" {
			q += "&" + query
		}
		l.Live, l.LiveURL = true, liveURL("owner", q)
	}
	return l
}

// pageRange is a window of an owner page, chosen by ?range=.
type pageRange struct {
	param, label, long string
	window             time.Duration
}

// parseRange reads ?range=: its index in rs, the first for none; an unknown
// range is missing.
func parseRange(rs []pageRange, raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	for i, r := range rs {
		if r.param == raw {
			return i, nil
		}
	}
	return 0, errNotFound
}

// rangeLinks are the links of the ranges of the page at path, cur selected;
// the first range is the page without ?range=.
func rangeLinks(path string, rs []pageRange, cur int) []NavLink {
	links := make([]NavLink, 0, len(rs))
	for i, r := range rs {
		href := path
		if i != 0 {
			href += "?range=" + r.param
		}
		links = append(links, NavLink{Label: r.label, Href: href, Current: i == cur})
	}
	return links
}
