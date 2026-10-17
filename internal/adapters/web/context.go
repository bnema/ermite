// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
)

// maxContextLines bounds the lines one gap loads: the rest stays behind a
// smaller gap that loads the next lines.
const maxContextLines = 2000

// ContextRows is the hidden lines of a gap, for both layouts: they replace the
// rows of the gap. More is the gap that is left, when the file goes on.
type ContextRows struct {
	Lines []DiffLine
	More  *DiffGap
}

// diffContext is GET /<repo>/context/<commit>?path=&old=&new=&n=: the lines of
// a file of a diff that its hunks hide, as rows of the diff tables. commit is
// the full id of the commit of the new side; path the file there; old and new
// the numbers of the first hidden line on each side, which move together (the
// lines are unchanged); n the number of lines, 0 for the rest of the file.
//
//hexcheck:ignore suspicious-business-logic-in-adapter cuts a file the use case opened into rows of the diff; no rule is decided here
func (s *Server) diffContext(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	sha := r.PathValue("sha")
	if !isFullID(sha) {
		return nil, 0, errNotFound
	}
	q := r.URL.Query()
	num := func(k string, least int) (int, bool) {
		v, err := strconv.Atoi(q.Get(k))
		return v, err == nil && v >= least && strconv.Itoa(v) == q.Get(k)
	}
	oldFrom, ok1 := num("old", 1)
	newFrom, ok2 := num("new", 1)
	n, ok3 := num("n", 0)
	path := q.Get("path")
	if !ok1 || !ok2 || !ok3 || path == "" {
		return nil, 0, errNotFound
	}
	repo, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	info, body, err := s.openBlobAt(ctx, repo, sha, path)
	if err != nil {
		return nil, 0, err
	}
	b := &blobStream{body: body, info: info}
	defer b.close()
	if info.Binary || info.Size > maxCodeBytes {
		return nil, 0, errNotFound
	}
	if err := b.fill(int(info.Size)); err != nil {
		return nil, 0, err
	}
	// Lines as Git numbers them: split on LF only (a lone CR is a character of
	// its line, shown as a marker; the CR of a CRLF is dropped by segments).
	text := strings.ToValidUTF8(string(b.buf), "\uFFFD")
	lines := strings.Split(text, "\n")
	noEOL := text != "" && !strings.HasSuffix(text, "\n")
	if !noEOL {
		lines = lines[:len(lines)-1]
	}
	if newFrom > len(lines)+1 {
		return nil, 0, errNotFound
	}
	end := len(lines)
	if n > 0 {
		end = min(end, newFrom-1+n)
	}
	cut := min(end, newFrom-1+maxContextLines)

	var out ContextRows
	for i := newFrom - 1; i < cut; i++ {
		o, nw := oldFrom+i-(newFrom-1), i+1
		out.Lines = append(out.Lines, DiffLine{
			Kind: "ctx", Mark: " ", Segs: segments(lines[i]),
			Old: itoa(o), New: itoa(nw), oldN: o, newN: nw,
			NoEOL: noEOL && i == len(lines)-1,
		})
	}
	if cut < end {
		rest := 0
		if n > 0 {
			rest = end - cut
		}
		next := gapHref(r, oldFrom+cut-(newFrom-1), cut+1, rest)
		out.More = &DiffGap{Key: q.Get("key"), Lines: rest, Src: next}
	}
	return DiffContext(out), http.StatusOK, nil
}

// gapHref is the address of the gap that starts at the lines old and new.
func gapHref(r *http.Request, old, new, n int) string {
	q := r.URL.Query()
	q.Set("old", itoa(old))
	q.Set("new", itoa(new))
	q.Set("n", itoa(n))
	return r.URL.Path + "?" + q.Encode()
}
