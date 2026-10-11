// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/url"
	"strings"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// resolveRef finds the ref a URL path starts with and the path that follows.
// Ref names may contain "/" ("feature/x"), so the longest prefix of the path
// that is the short name of a branch or tag wins. A branch wins over a tag of
// the same name. rest has no leading slash and is "" when the path is the ref
// alone. ok is false when no ref matches; the caller may then try a commit id.
func resolveRef(refs []*pb.Ref, p string) (ref *pb.Ref, rest string, ok bool) {
	names := refsByName(refs)
	p = strings.Trim(p, "/")
	for end := len(p); end > 0; end = strings.LastIndex(p[:end], "/") {
		if r, found := names[p[:end]]; found {
			return r, strings.TrimPrefix(p[end:], "/"), true
		}
	}
	return nil, "", false
}

// refsByName indexes the branches and tags by short name. A branch wins over
// a tag of the same name.
func refsByName(refs []*pb.Ref) map[string]*pb.Ref {
	names := make(map[string]*pb.Ref, len(refs))
	for _, r := range refs {
		k := r.GetKind()
		if k != pb.RefKind_REF_KIND_BRANCH && k != pb.RefKind_REF_KIND_TAG {
			continue
		}
		cur, dup := names[r.GetShortName()]
		if !dup || cur.GetKind() != pb.RefKind_REF_KIND_BRANCH {
			names[r.GetShortName()] = r
		}
	}
	return names
}

// escapePath escapes every segment of a slash-separated path for a URL path,
// keeping the slashes.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// repoHref is the URL of a page of a repository: /<repo>/<kind>/<ref>/<path>.
// kind, ref and path may be empty.
func repoHref(repo string, parts ...string) string {
	var b strings.Builder
	b.WriteString("/" + url.PathEscape(repo))
	for _, p := range parts {
		if p != "" {
			b.WriteString("/" + escapePath(strings.Trim(p, "/")))
		}
	}
	return b.String()
}

// target is what the URL of a tree, blob, raw or commits page designates: a
// commit, the name it was reached by, and a path in its tree.
type target struct {
	// rev is what the API is asked for: the id of the commit of a ref, or
	// the commit id of the URL itself.
	rev string
	// name is how the URL spells the ref: a branch or a tag, or the commit id
	// when detached.
	name string
	path string
	// detached is true when the URL names a commit and not a ref.
	detached bool
}

// locate reads "<ref>[/<path>]" or "<commit>[/<path>]". The ref is the
// longest prefix of the path that is the short name of a branch or a tag
// (names may contain "/"); a branch wins over a tag of the same name. An
// empty rest designates the head of the repository. Whether the commit or
// the path exists is for the API to say.
func (d *repoData) locate(rest string) (target, bool) {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		if d.head == nil {
			return target{}, false
		}
		return target{rev: d.head.GetCommitId(), name: d.head.GetShortName()}, true
	}
	if ref, p, ok := resolveRef(d.allRefs(), rest); ok && ref.GetCommitId() != "" {
		return target{rev: ref.GetCommitId(), name: ref.GetShortName(), path: p}, true
	}
	first, p, _ := strings.Cut(rest, "/")
	if isCommitID(first) {
		return target{rev: first, name: first, path: p, detached: true}, true
	}
	return target{}, false
}

// isCommitID reports whether s can be a commit id or an abbreviation of one
// (7 to 40 lowercase hexadecimal digits).
func isCommitID(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isBranchHead reports whether t is a branch at its tip, as opposed to a tag
// or a commit id.
func (d *repoData) isBranchHead(t target) bool {
	if t.detached || !isFullID(t.rev) {
		return false
	}
	for _, b := range d.branches {
		if b.GetShortName() == t.name && b.GetCommitId() == t.rev {
			return true
		}
	}
	return false
}

// pinRef points the ref switcher of the layout at what a page shows: a
// branch or a tag, which is then the current one of the lists, or a commit,
// which makes none current.
func pinRef(l *Layout, t target) {
	if t.detached {
		l.Ref, l.RefPath, l.Short, l.SHA = "detached", "", short(t.rev), ""
		if len(t.rev) == 40 {
			l.SHA = t.rev
		}
		for i := range l.Heads {
			l.Heads[i].Current = false
		}
		for i := range l.Tags {
			l.Tags[i].Current = false
		}
		return
	}
	l.Ref, l.RefPath, l.SHA, l.Short = t.name, t.name, t.rev, short(t.rev)
	for i := range l.Heads {
		l.Heads[i].Current = l.Heads[i].Name == t.name
	}
	for i := range l.Tags {
		l.Tags[i].Current = l.Tags[i].Name == t.name && !hasName(l.Heads, t.name)
	}
}

func hasName(items []RefItem, name string) bool {
	for _, it := range items {
		if it.Name == name {
			return true
		}
	}
	return false
}

// pathCrumbs is the path in the header: repo, kind ("tree", "blob" or
// "commits"), ref, then the segments of the path. The directories link to
// their tree; the last segment links to the file for a blob, and to nothing
// for a history.
func pathCrumbs(repo, kind string, t target) []Crumb {
	home := repoHref(repo, "tree", t.name)
	if kind == "commits" {
		home = repoHref(repo, "commits", t.name)
	}
	cs := []Crumb{
		{Label: repo, Href: repoHref(repo)},
		{Label: kind, Href: home},
		{Label: t.name, Href: home, Current: t.path == ""},
	}
	if t.path == "" {
		return cs
	}
	segs := strings.Split(t.path, "/")
	for i, seg := range segs {
		last := i == len(segs)-1
		c := Crumb{Label: visibleText(seg), Current: last}
		switch {
		case !last || kind == "tree":
			c.Href = repoHref(repo, "tree", t.name, strings.Join(segs[:i+1], "/"))
		case kind == "blob":
			c.Href = repoHref(repo, "blob", t.name, t.path)
		}
		cs = append(cs, c)
	}
	return cs
}
