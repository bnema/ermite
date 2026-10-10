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
