// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"path"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
)

// Entry kinds of a TreeRow; the stylesheet draws the node of a row by its kind.
const (
	kindDir    = "dir"
	kindFile   = "file"
	kindLink   = "link"
	kindModule = "module"
)

// TreeRow is one entry of a directory.
type TreeRow struct {
	Kind string // kindDir, kindFile, kindLink or kindModule
	Name string
	Href string // "" for a submodule
	// Expand is the URL of the rows of a directory (Href?rows); "" for any
	// other entry.
	Expand string
	// Hint says what a directory, a link or a submodule is, in words, for the
	// screen reader (the node drawn for it is aria-hidden) and the tooltip; ""
	// for a file.
	Hint    string
	Size    string
	Subject string
	CHref   string
	Author  string
	When    string
	WhenAt  string
	Cursor  bool
	// Pos and Set are the aria-posinset and aria-setsize of the row among the
	// entries of its directory; Set is -1 when the listing is cut, as the
	// directory has more entries than are listed.
	Pos, Set int
}

// TreePage is a directory.
type TreePage struct {
	Rows []TreeRow
	// Path is the directory, "/" for the root.
	Path string
	// Up is the page of the parent directory, "" at the root: the h key of the
	// tree goes there from a top-level entry.
	Up string
	// Notes say what the listing leaves out.
	Notes []string
	// More, for the rows of a directory expanded in place, is the page of the
	// directory when it has more entries than are listed; "" otherwise.
	More string
	// MorePos is the position of the row that says there are more.
	MorePos int
}

// Unit is the noun of the entry count in the head of the tree.
func (p TreePage) Unit() string {
	if len(p.Rows) == 1 {
		return "entry"
	}
	return "entries"
}

// tree lists a directory of a ref, with the last commit of each entry. With
// ?rows it answers the entries alone, for a directory opened in place.
//
//hexcheck:ignore suspicious-business-logic-in-adapter builds the directory page from the use case results; no rule is decided here
func (s *Server) tree(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	t, ok := d.locate(r.PathValue("rest"))
	if !ok {
		return nil, 0, errNotFound
	}
	res, err := s.getTree(ctx, d.repo.Name, t.rev, t.path, true)
	if err != nil {
		return nil, 0, err
	}
	entries := res.Entries
	now := s.cfg.Now()

	rowsOnly := r.URL.Query().Has("rows")
	p := TreePage{Path: "/"}
	if t.path != "" {
		p.Path = visibleText(t.path) + "/"
	}
	for i, e := range entries {
		row := TreeRow{Kind: kindFile, Name: visibleText(e.Name), Size: humanSize(e.Size), Cursor: i == 0 && !rowsOnly}
		switch e.Kind {
		case domain.EntryDirectory:
			row.Kind, row.Hint, row.Name, row.Size = kindDir, "directory", row.Name+"/", ""
			row.Href = repoHref(d.name(), "tree", t.name, e.Path)
			row.Expand = row.Href + "?rows"
		case domain.EntrySymlink:
			row.Kind, row.Hint = kindLink, "symbolic link"
			row.Href = repoHref(d.name(), "blob", t.name, e.Path)
		case domain.EntrySubmodule:
			row.Kind, row.Hint, row.Size = kindModule, "submodule", ""
		default:
			row.Href = repoHref(d.name(), "blob", t.name, e.Path)
		}
		if c := e.LastCommit; c != nil {
			last := commitRow(d.name(), *c, now)
			row.Subject, row.Author, row.CHref, row.When, row.WhenAt = last.Subject, last.Author, last.CHref, last.When, last.WhenAt
		}
		p.Rows = append(p.Rows, row)
	}
	for i := range p.Rows {
		p.Rows[i].Pos, p.Rows[i].Set = i+1, len(p.Rows)
		if res.Truncated {
			p.Rows[i].Set = -1
		}
	}
	if rowsOnly {
		// The rows alone, for a directory opened in place (files.js).
		if res.Truncated {
			p.More, p.MorePos = repoHref(d.name(), "tree", t.name, t.path), len(p.Rows)+1
		}
		return TreeRows(p), http.StatusOK, nil
	}
	if res.Truncated {
		p.Notes = append(p.Notes, "this directory has more entries than are listed here: the first "+itoa(len(entries))+" are shown.")
	}
	if res.LastCommitsOmitted {
		p.Notes = append(p.Notes, "the last commit is shown for the first entries only.")
	}
	if t.path != "" {
		up := path.Dir(t.path)
		if up == "." {
			up = ""
		}
		p.Up = repoHref(d.name(), "tree", t.name, up)
	}

	l := s.repoLayout(ctx, d, "tree")
	// The finder searches the commit this page lists, whichever way the URL
	// spelled it (an abbreviated id included): the listing knows it.
	t.commit = res.CommitID.String()
	pinRef(&l, t)
	l.Crumbs = pathCrumbs(d.name(), "tree", t)
	if t.path == "" {
		l.Title = "tree · " + d.name() + " · " + s.instance
	} else {
		l.Title = visibleText(t.path) + " · " + d.name() + " · " + s.instance
	}
	return Tree(l, p), http.StatusOK, nil
}
