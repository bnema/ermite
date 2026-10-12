// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"sort"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
)

// refs lists the branches and the tags of a repository with the commit each
// designates: branches by name with the default first, tags newest first.
func (s *Server) refs(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	p := s.refsPage(ctx, d)

	l := s.repoLayout(ctx, d, "refs")
	l.Title = "refs · " + d.name() + " · " + s.instance
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=refs")
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs, Crumb{Label: "refs", Current: true})
	l.HasRail = true
	return Refs(l, p), http.StatusOK, nil
}

// refsPage is the content of the refs page, which the live updates of the
// page render again.
func (s *Server) refsPage(ctx context.Context, d *repoData) RefsPage {
	commits := s.commitsOf(ctx, d.repo.Name, d.allRefs())

	def := d.repo.DefaultBranch
	p := RefsPage{Default: def}
	branches := append([]domain.Ref(nil), d.branches...)
	sort.SliceStable(branches, func(i, j int) bool {
		a, b := branches[i].ShortName == def, branches[j].ShortName == def
		return a && !b
	})
	for i, b := range branches {
		p.Branches = append(p.Branches, s.refRow(d, b, commits, b.ShortName == def, i == 0))
	}
	tags := append([]domain.Ref(nil), d.tags...)
	sort.SliceStable(tags, func(i, j int) bool {
		return commits[tags[i].CommitID.String()].at > commits[tags[j].CommitID.String()].at
	})
	for i, t := range tags {
		p.Tags = append(p.Tags, s.refRow(d, t, commits, false, len(p.Branches) == 0 && i == 0))
	}
	if len(tags) > 0 {
		p.Latest = tags[0].ShortName
	}
	if d.cut {
		p.Note = "this repository has more refs than are listed here."
	}
	return p
}

func (s *Server) refRow(d *repoData, r domain.Ref, commits map[string]refCommit, isDefault, cursor bool) RefRow {
	id := r.CommitID.String()
	c := commits[id]
	row := RefRow{
		Name: r.ShortName, Href: repoHref(d.name(), "tree", r.ShortName),
		Short: short(id), Subject: c.subject, When: c.when, WhenAt: c.whenAt,
		Default: isDefault, Cursor: cursor,
	}
	if id != "" {
		row.CHref = repoHref(d.name(), "commit", id)
	}
	return row
}
