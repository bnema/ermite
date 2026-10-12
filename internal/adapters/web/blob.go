// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
)

const (
	// maxCodeBytes is the largest file shown in the page; a larger one gets
	// a notice and the link to raw.
	maxCodeBytes = 1 << 20
	// maxRenderBytes is the largest Markdown file that is rendered; above it
	// the file is shown as source only.
	maxRenderBytes = 512 << 10
)

// BlobPage is a file.
type BlobPage struct {
	Name   string
	Notice string
	// Code is the highlighted source, shown when HasCode.
	Code    template.HTML
	HasCode bool
	// Rendered is a Markdown file as HTML; the source is under a fold.
	Rendered *Readme
	Outline  []Heading
	// Image says the file is shown as an image loaded from RawHref.
	Image    bool
	Facts    []Fact
	Last     *CommitRow
	RawHref  string
	HistHref string
	DirHref  string
}

// blobStream is an open file: its metadata and the content, read on demand.
type blobStream struct {
	body io.ReadCloser
	info domain.BlobInfo
	// buf holds the content already read and not yet handed out.
	buf []byte
	// eof is true once body has nothing more.
	eof bool
}

// openBlob opens a file of the repository.
func (s *Server) openBlob(ctx context.Context, repo domain.RepoName, t target) (*blobStream, error) {
	info, body, err := s.openBlobAt(ctx, repo, t.rev, t.path)
	if err != nil {
		return nil, err
	}
	return &blobStream{body: body, info: info}, nil
}

func (b *blobStream) close() { _ = b.body.Close() }

// fill reads until buf holds n bytes or the content ends.
func (b *blobStream) fill(n int) error {
	if b.eof || len(b.buf) >= n {
		return nil
	}
	// One allocation for the whole content, not the doublings of append.
	if cap(b.buf) < n {
		b.buf = append(make([]byte, 0, n), b.buf...)
	}
	m, err := io.ReadFull(b.body, b.buf[len(b.buf):n])
	b.buf = b.buf[:len(b.buf)+m]
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		b.eof = true
		return nil
	}
	return err
}

// image types: the only images the pages show and raw serves as images.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// sniffImage is the image type of the first bytes of a file, "" when it is
// not a PNG, JPEG, GIF or WebP. SVG is never one: it can carry scripts.
func sniffImage(head []byte) string {
	if len(head) > 512 {
		head = head[:512]
	}
	if t := http.DetectContentType(head); imageTypes[t] {
		return t
	}
	return ""
}

// blob shows a file: highlighted source, rendered Markdown with its source
// under a fold, an image, or a notice when the file is too large or binary.
func (s *Server) blob(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	t, ok := d.locate(r.PathValue("rest"))
	if !ok {
		return nil, 0, errNotFound
	}
	b, err := s.openBlob(ctx, d.repo.Name, t)
	if err != nil {
		return nil, 0, err
	}
	defer b.close()

	name := path.Base(t.path)
	dir := path.Dir(t.path)
	if dir == "." {
		dir = ""
	}
	size := b.info.Size
	p := BlobPage{
		Name:     visibleText(name),
		RawHref:  repoHref(d.name(), "raw", t.name, t.path),
		HistHref: repoHref(d.name(), "commits", t.name, t.path),
		DirHref:  repoHref(d.name(), "tree", t.name, dir),
		Facts:    []Fact{{Key: "size", Val: humanSize(size)}},
	}
	typ := b.info.MediaType
	lang := ""

	// An image is recognised by its bytes, whatever Git's text rule says.
	if err := b.fill(512); err != nil {
		return nil, 0, err
	}
	img := sniffImage(b.buf)

	var src []byte
	switch {
	case img != "":
		p.Image, typ = true, img
	case b.info.Binary:
		p.Notice = fmt.Sprintf("binary file (%s, %s): not shown. use raw.", typ, humanSize(size))
	case size > maxCodeBytes:
		p.Notice = fmt.Sprintf("file too large to show (%s, the limit is %s). use raw.", humanSize(size), humanSize(maxCodeBytes))
	case size == 0:
		p.Notice = "empty file."
	default:
		if err := b.fill(int(size)); err != nil {
			return nil, 0, err
		}
		src = b.buf
	}

	if src != nil {
		text := string(src)
		lines := countLines(src)
		p.Facts = append(p.Facts, Fact{Key: "lines", Val: itoa(lines)})
		if lines > maxViewLines {
			text = firstLines(text, maxViewLines)
			p.Notice = fmt.Sprintf("%d lines: showing the first %d. use raw for the rest.", lines, maxViewLines)
		}
		cv := highlightCtx(ctx, t.path, text)
		p.Code, p.HasCode, lang = cv.html, true, cv.lang
		if isMarkdown(name) && size <= maxRenderBytes {
			h, outline, err := s.md.render(src, d.name(), t.name, dir)
			if err == nil {
				p.Rendered, p.Outline = &Readme{Name: p.Name, HTML: h}, outline
			} else {
				log := zerowrap.FromCtx(ctx)
				log.Warn().Err(err).Str("repo", d.name()).Msg("render Markdown file, showing source")
			}
		}
	}
	if lang == "" {
		lang = typ
	}
	p.Facts = append(p.Facts, Fact{Key: "type", Val: lang})

	if last := s.lastChange(ctx, d.repo.Name, t); last != nil {
		p.Last = last
	}

	l := s.repoLayout(ctx, d, "tree")
	pinRef(&l, t)
	l.Crumbs = pathCrumbs(d.name(), "blob", t)
	l.Title = visibleText(t.path) + " · " + d.name() + " · " + s.instance
	l.HasRail = true
	return Blob(l, p), http.StatusOK, nil
}

// lastChange is the latest commit that touched the path, nil when it cannot
// be found (the page does not depend on it).
func (s *Server) lastChange(ctx context.Context, repo domain.RepoName, t target) *CommitRow {
	res, err := s.commitList(ctx, repo, t.rev, t.path, 1, "")
	if err != nil || len(res.Commits) != 1 {
		return nil
	}
	row := commitRow(repo.String(), res.Commits[0], s.cfg.Now())
	return &row
}

// isMarkdown says whether a file name is a Markdown file.
func isMarkdown(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".md") || strings.HasSuffix(n, ".markdown")
}

// firstLines is the first n lines of s.
func firstLines(s string, n int) string {
	idx := 0
	for i := 0; i < n; i++ {
		j := strings.IndexByte(s[idx:], '\n')
		if j < 0 {
			return s
		}
		idx += j + 1
	}
	return s[:idx]
}
