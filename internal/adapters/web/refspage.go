// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"sort"

	"github.com/a-h/templ"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// refs lists the branches and the tags of a repository with the commit each
// designates: branches by name with the default first, tags newest first.
func (s *Server) refs(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	all := append(append([]*pb.Ref(nil), d.branches...), d.tags...)
	commits := s.commitsOf(ctx, d.name(), all)

	def := d.repo.GetDefaultBranch()
	p := RefsPage{Default: def}
	branches := append([]*pb.Ref(nil), d.branches...)
	sort.SliceStable(branches, func(i, j int) bool {
		a, b := branches[i].GetShortName() == def, branches[j].GetShortName() == def
		return a && !b
	})
	for i, b := range branches {
		p.Branches = append(p.Branches, s.refRow(d, b, commits, b.GetShortName() == def, i == 0))
	}
	tags := append([]*pb.Ref(nil), d.tags...)
	sort.SliceStable(tags, func(i, j int) bool {
		return commits[tags[i].GetCommitId()].at > commits[tags[j].GetCommitId()].at
	})
	for i, t := range tags {
		p.Tags = append(p.Tags, s.refRow(d, t, commits, false, len(p.Branches) == 0 && i == 0))
	}
	if len(tags) > 0 {
		p.Latest = tags[0].GetShortName()
	}
	if d.cut {
		p.Note = "this repository has more refs than are listed here."
	}

	l := s.repoLayout(ctx, d, "refs")
	l.Title = "refs · " + d.name() + " · " + s.instance
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs, Crumb{Label: "refs", Current: true})
	l.Status = Status{
		Mode: "refs", Context: plural(len(p.Branches), "branch", "branches") + " · " + plural(len(p.Tags), "tag", "tags"),
		Hints: "j/k move · enter open · / filter · ? help", Cursor: true, Count: len(p.Branches) + len(p.Tags),
	}
	l.HasRail, l.RailSum = true, "refs · legend"
	return Refs(l, p), http.StatusOK, nil
}

func (s *Server) refRow(d *repoData, r *pb.Ref, commits map[string]refCommit, isDefault, cursor bool) RefRow {
	c := commits[r.GetCommitId()]
	row := RefRow{
		Name: r.GetShortName(), Href: repoHref(d.name(), "tree", r.GetShortName()),
		Short: short(r.GetCommitId()), Subject: c.subject, When: c.when, WhenAt: c.whenAt,
		Default: isDefault, Cursor: cursor,
	}
	if r.GetCommitId() != "" {
		row.CHref = repoHref(d.name(), "commit", r.GetCommitId())
	}
	return row
}
