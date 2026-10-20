// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The diff of a merge request is read a window of files at a time. The page
// reads the files of one window (the first, or the one ?from= names) with
// their hunks and lists the others as placeholders; a placeholder holds the
// address of its window, which the browser fetches when it comes near the
// screen and swaps in (mr.js). The window carries the base and the head the
// page shows: when they moved, the use case refuses (ErrConflict) and the
// placeholder offers to reload instead of mixing two diffs in one page.

// MRFilesPage is a window of files of a merge request, for /files: the files
// of the window, rendered like in the page, and nothing else.
type MRFilesPage struct {
	Files []DiffFile
	DiffView
}

// windowStart is the first file of the window that holds file i.
func windowStart(i int) int { return i - i%mrWindow }

// withFrom adds the window of a page to its address: the page of the files
// [from, from+mrWindow) when from is not the first window.
func withFrom(href string, from int) string {
	if from <= 0 {
		return href
	}
	sep := "?"
	if strings.Contains(href, "?") {
		sep = "&"
	}
	return href + sep + "from=" + itoa(from)
}

// formFrom reads the window a form was posted from (a hidden field "from"):
// 0 when absent or not a window start, so that a redirect never carries
// anything else.
func formFrom(r *http.Request) int {
	s := r.PostFormValue("from")
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n%mrWindow != 0 || strconv.Itoa(n) != s {
		return 0
	}
	return n
}

// parseFrom reads ?from=: the start of a window, 0 when absent. Anything else
// (not a number, negative, not a window start) is not a page.
func parseFrom(r *http.Request) (int, error) {
	s := r.URL.Query().Get("from")
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n%mrWindow != 0 || strconv.Itoa(n) != s {
		return 0, errNotFound
	}
	return n, nil
}

// parseOne reads ?one=1&from=i of a one-file fragment: the index i of the
// file, any non-negative number.
func parseOne(r *http.Request) (int, error) {
	q := r.URL.Query()
	s := q.Get("from")
	n, err := strconv.Atoi(s)
	if q.Get("one") != "1" || err != nil || n < 0 || strconv.Itoa(n) != s {
		return 0, errNotFound
	}
	return n, nil
}

// lazyFiles marks the files outside [from, from+mrWindow) lazy: the page shows
// a placeholder that loads their window. src is the URL of /files with the
// base and the head of the diff; self the page, for the links of a page
// without scripts.
func lazyFiles(files []DiffFile, from int, src string) {
	for i := range files {
		f := &files[i]
		if i >= from && i < from+mrWindow {
			continue
		}
		f.Lazy = true
		f.From = windowStart(i)
		f.Src = src + "&from=" + itoa(f.From)
		f.Height = lazyHeight(f.Add + f.Del)
		f.Open = !f.Bin && f.Add+f.Del > 0
	}
}

// lazyHeight is the size class of the placeholder of a file with n changed
// lines: about the room its diff takes, so that the page does not jump much
// when it comes in.
func lazyHeight(n int) int {
	switch {
	case n <= 10:
		return 1
	case n <= 50:
		return 2
	case n <= 200:
		return 3
	default:
		return 4
	}
}

// filesSrc is the address of the windows of a diff.
func filesSrc(repo string, n int64, since bool, base, head string) string {
	q := url.Values{}
	if since {
		q.Set("since", "1")
	}
	q.Set("base", base)
	q.Set("head", head)
	return mrHref(repo, n, "files") + "?" + q.Encode()
}

// countNotes sets, on each file, the number of comments on its lines and the
// ids of those comments.
func countNotes(files []DiffFile, comments []domain.Comment) {
	at := make(map[string]int, len(files))
	for i, f := range files {
		at[f.Key] = i
	}
	ids := make([][]string, len(files))
	for _, c := range comments {
		i, ok := at[c.Path]
		if c.Path == "" || !ok {
			continue
		}
		if c.ReplyTo == 0 && !c.Deleted {
			files[i].Notes++
		}
		if c.ReplyTo == 0 && !c.Deleted && !c.Resolved {
			files[i].OpenNotes++
		}
		ids[i] = append(ids[i], "c"+strconv.FormatInt(c.ID, 10))
	}
	for i := range files {
		files[i].Comments = strings.Join(ids[i], " ")
	}
}

// splitPath is the last element of a path and the directory before it.
func splitPath(p string) (name, dir string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return visibleText(p), ""
	}
	return visibleText(p[i+1:]), visibleText(p[:i])
}

// mrFiles is GET /<repo>/merge-requests/<n>/files?from=&base=&head=[&since=1]:
// the files of a window with their hunks and their comments, for a
// placeholder of the page. A base or a head that is not the one of the diff
// any more answers 409: the page says to reload.
func (s *Server) mrFiles(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	n, err := mrNumber(r)
	if err != nil {
		return nil, 0, err
	}
	q := r.URL.Query()
	since := q.Get("since") == "1"
	if since && !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	one := q.Get("one") == "1"
	var from int
	if one {
		from, err = parseOne(r)
	} else {
		from, err = parseFrom(r)
	}
	if err != nil {
		return nil, 0, err
	}
	base, errB := domain.ParseObjectID(q.Get("base"))
	head, errH := domain.ParseObjectID(q.Get("head"))
	if errB != nil || errH != nil {
		return nil, 0, errNotFound
	}
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	win := &ports.FileWindow{From: from, To: from + mrWindow}
	if one {
		win = &ports.FileWindow{From: from, To: from + 1, Full: true}
	}
	res, err := s.cfg.MergeRequests.Get(ctx, principal(ctx), d.repo.Name, n, ports.GetMergeRequestOptions{
		SinceLastRead: since, DiffOnly: true, Base: base, Head: head, Files: win,
	})
	if errors.Is(err, domain.ErrConflict) {
		return MRFilesMoved(), http.StatusConflict, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if from >= len(res.Diff.Files) {
		return nil, 0, errNotFound
	}
	m := res.MergeRequest
	signed := SignedIn(ctx)
	canWrite := signed && m.State == domain.MRStateOpen && !since
	p := MRPage{Files: s.hl.files(ctx, res.Diff.Files), LooseInFiles: true, NoteAuthor: domain.AuthorName(principal(ctx))}
	contextGaps(p.Files, res.Diff.Files, repoHref(d.name(), "context", head.String()))
	links := fileLinks{src: filesSrc(d.name(), n, since, base.String(), head.String()), from: from, repo: d.name(), rev: head.String()}
	if one {
		// A one-file read holds file from alone, at index from: the load
		// addresses of a window do not apply, only the raw link.
		p.Files[from].Raw = rawLink(res.Diff.Files[from], links)
	} else {
		collapseFiles(p.Files, res.Diff.Files, links)
	}
	s.placeComments(&p, d.name(), n, res.Comments, m.HeadID.String(), !since, canWrite, nil)
	window := p.Files[from:min(from+mrWindow, len(p.Files))]
	if one {
		window = p.Files[from : from+1]
	}
	// The layouts follow the page: it sends ?view= (the layout shown) and
	// ?layouts=both when it holds both layouts and switches them itself, so
	// that every window of the page renders the same way.
	self := mrHref(d.name(), n)
	if since {
		self = mrHref(d.name(), n, "since")
	}
	lines, _ := diffLines(window)
	v := newDiffView(r, lines, withFrom(self+"?view=unified", from), withFrom(self+"?view=split", from))
	switch r.URL.Query().Get("layouts") {
	case "both":
		v.Both = true
	case "one":
		v.Both = false
	}
	v.ShowUnified, v.ShowSplit = v.Both || v.View == "unified", v.Both || v.View == "split"
	return MRFiles(MRFilesPage{Files: window, DiffView: v}), http.StatusOK, nil
}
