// SPDX-License-Identifier: AGPL-3.0-or-later

package httpserver

import (
	"net/http"
	"strconv"
)

// robotsPath is where robots.txt is served. "robots.txt" is a reserved
// repository name, so no repository page can sit there.
const robotsPath = "/robots.txt"

// DefaultRobotsTxt keeps crawlers on the home page, the repository summaries,
// issues, merge requests and releases, and off the views that are endless
// (every file and directory at every commit, every pair of commits, every
// diff layout) or costly (raw files, downloads, CI logs, Git transfers).
//
// In RFC 9309 patterns "*" matches any characters, slashes included, so
// "/*/blob/" also covers the /<owner>/<repo>/blob/ form. For the same reason
// a bare "/*/tree" would match any path containing "/tree" (a release tag
// "tree-v1"); each such view is the triplet "X$", "X/" and "X?" instead. The
// RFC has no single-segment class, so a path whose last segment is exactly
// "tree", "commits", "refs" or "actions" (a release tag named "tree") is
// still blocked.
//
// "/*/merge-requests/*?" blocks the query variants of a merge request page
// (diff layout, file, comment anchor), which each render a full diff; the
// bare page and the list (/<repo>/merge-requests?page=) stay allowed.
const DefaultRobotsTxt = `User-agent: *
Crawl-delay: 5
Allow: /-/static/
Disallow: /-/
Disallow: /*/blob/
Disallow: /*/raw/
Disallow: /*/tree$
Disallow: /*/tree/
Disallow: /*/tree?
Disallow: /*/commit/
Disallow: /*/commits$
Disallow: /*/commits/
Disallow: /*/commits?
Disallow: /*/compare/
Disallow: /*/refs$
Disallow: /*/refs/
Disallow: /*/refs?
Disallow: /*/paths/
Disallow: /*/context/
Disallow: /*/merge-requests/*?
Disallow: /*/merge-requests/*/files
Disallow: /*/merge-requests/*/since
Disallow: /*/issues/new
Disallow: /*/issues/submitted
Disallow: /*/actions$
Disallow: /*/actions/
Disallow: /*/actions?
Disallow: /*/releases/download/
Disallow: /*/releases/latest/download/
Disallow: /*/releases/assets/
Disallow: /*/info/refs
Disallow: /*/git-upload-pack
Disallow: /*/git-receive-pack
`

// robotsHandler serves body as robots.txt, cached for an hour.
func robotsHandler(body []byte) http.HandlerFunc {
	length := strconv.Itoa(len(body))
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=3600")
		h.Set("Content-Length", length)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	}
}
