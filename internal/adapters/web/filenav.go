// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"path"
	"strings"

	"bnema.dev/ermite/internal/domain"
)

// NavItem is one entry of the file tree of the rail of a file page.
type NavItem struct {
	Kind string // kindDir, kindFile, kindLink or kindModule
	Name string
	// Path is the URL-escaped repository-relative path.
	Path string
	// Href is the page of the entry: the file, or the directory; "" for a
	// submodule.
	Href string
	// Src is the URL of the items of a directory (its tree page with ?nav);
	// "" for any other entry.
	Src string
	// Open says the directory is an ancestor of the current file: it is
	// shown open with its items.
	Open bool
	// Current marks the file of the page.
	Current bool
	// Items are the entries of an open directory.
	Items NavList
}

// NavList is the entries of one directory in the file tree.
type NavList struct {
	Items []NavItem
	// More is the page of the directory when it has more entries than are
	// listed; "" otherwise.
	More string
}

// navItem is the item of an entry of a directory.
func navItem(repo, ref string, e domain.TreeEntry) NavItem {
	it := NavItem{Kind: kindFile, Name: visibleText(e.Name), Path: escapePath(e.Path)}
	switch e.Kind {
	case domain.EntryDirectory:
		it.Kind = kindDir
		it.Href = repoHref(repo, "tree", ref, e.Path)
		it.Src = it.Href + "?nav"
	case domain.EntrySymlink:
		it.Kind = kindLink
		it.Href = repoHref(repo, "blob", ref, e.Path)
	case domain.EntrySubmodule:
		it.Kind = kindModule
	default:
		it.Href = repoHref(repo, "blob", ref, e.Path)
	}
	return it
}

// navList lists one directory for the file tree: names and kinds only, no
// last commit.
func (s *Server) navList(ctx context.Context, d *repoData, t target, dir string) (NavList, []domain.TreeEntry, error) {
	res, err := s.getTree(ctx, d.repo.Name, t.rev, dir, false)
	if err != nil {
		return NavList{}, nil, err
	}
	var l NavList
	for _, e := range res.Entries {
		l.Items = append(l.Items, navItem(d.name(), t.name, e))
	}
	if res.Truncated {
		l.More = repoHref(d.name(), "tree", t.name, dir)
	}
	return l, res.Entries, nil
}

// fileNav is the file tree of the rail of the page of the file t.path: the
// root entries, and the entries of each directory above the file, open and
// nested, down to the file, marked current.
func (s *Server) fileNav(ctx context.Context, d *repoData, t target) (NavList, error) {
	// dirs are the directories from the root down to the one of the file.
	dirs := []string{""}
	segs := strings.Split(t.path, "/")
	for i := 1; i < len(segs); i++ {
		dirs = append(dirs, strings.Join(segs[:i], "/"))
	}
	levels := make([]NavList, len(dirs))
	for i, dir := range dirs {
		l, entries, err := s.navList(ctx, d, t, dir)
		if err != nil {
			return NavList{}, err
		}
		// The next directory down, or the file at the last level.
		want := t.path
		if i+1 < len(dirs) {
			want = dirs[i+1]
		}
		found := false
		for _, e := range entries {
			if e.Path == want {
				found = true
				break
			}
		}
		if !found {
			// A cut listing leaves it out: it is added so the way to the file
			// stays whole.
			kind := domain.EntryFile
			if i+1 < len(dirs) {
				kind = domain.EntryDirectory
			}
			l.Items = append(l.Items, navItem(d.name(), t.name, domain.TreeEntry{Name: path.Base(want), Path: want, Kind: kind}))
		}
		levels[i] = l
	}
	// Nest from the bottom: each level goes into its directory, one up.
	for i := len(levels) - 1; i >= 0; i-- {
		want := t.path
		if i+1 < len(dirs) {
			want = dirs[i+1]
		}
		key := escapePath(want)
		for j := range levels[i].Items {
			it := &levels[i].Items[j]
			if it.Path != key {
				continue
			}
			if i+1 < len(dirs) {
				it.Open, it.Items = true, levels[i+1]
			} else {
				it.Current = true
			}
			break
		}
	}
	return levels[0], nil
}
