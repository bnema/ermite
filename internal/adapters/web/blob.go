// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"path"
	"strings"

	"connectrpc.com/connect"
	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	pb "bnema.dev/ermite-api/gen/ermite"
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

// blobStream is an open GetBlob stream whose first message has been read.
type blobStream struct {
	stream *connect.ServerStreamForClient[pb.GetBlobResponse]
	info   *pb.BlobInfo
	// buf holds the content already received and not yet handed out.
	buf []byte
}

// openBlob starts the stream of a file and reads its metadata.
func (s *Server) openBlob(ctx context.Context, repo string, t target) (*blobStream, error) {
	if t.path == "" {
		return nil, errNotFound
	}
	st, err := s.cfg.Git.GetBlob(ctx, connect.NewRequest(&pb.GetBlobRequest{Repo: repo, Revision: t.rev, Path: t.path}))
	if err != nil {
		return nil, err
	}
	if !st.Receive() {
		err := st.Err()
		_ = st.Close()
		if err == nil {
			err = errNotFound
		}
		return nil, err
	}
	m := st.Msg()
	return &blobStream{stream: st, info: m.GetInfo(), buf: append([]byte(nil), m.GetChunk()...)}, nil
}

func (b *blobStream) close() { _ = b.stream.Close() }

// fill reads from the stream until buf holds n bytes or the content ends.
func (b *blobStream) fill(n int) error {
	for len(b.buf) < n && b.stream.Receive() {
		b.buf = append(b.buf, b.stream.Msg().GetChunk()...)
	}
	return b.stream.Err()
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
	b, err := s.openBlob(ctx, d.name(), t)
	if err != nil {
		return nil, 0, err
	}
	defer b.close()

	name := path.Base(t.path)
	dir := path.Dir(t.path)
	if dir == "." {
		dir = ""
	}
	size := b.info.GetSize()
	p := BlobPage{
		Name:     markedText(name),
		RawHref:  repoHref(d.name(), "raw", t.name, t.path),
		HistHref: repoHref(d.name(), "commits", t.name, t.path),
		DirHref:  repoHref(d.name(), "tree", t.name, dir),
		Facts:    []Fact{{Key: "size", Val: humanSize(size)}},
	}
	typ := b.info.GetMediaType()
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
	case b.info.GetBinary():
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

	if last := s.lastChange(ctx, d.name(), t); last != nil {
		p.Last = last
	}

	l := s.repoLayout(ctx, d, "tree")
	pinRef(&l, t)
	l.Crumbs = fileCrumbs(d.name(), "blob", t)
	l.Title = markedText(t.path) + " · " + d.name() + " · " + s.instance
	l.Status = Status{
		Mode: "blob", Context: p.Name,
		Hints: "click a line number · y permalink · ? help", Blob: p.HasCode,
	}
	l.HasRail, l.Read, l.RailSum = true, p.Rendered != nil, "file · last change · selection"
	return Blob(l, p), http.StatusOK, nil
}

// lastChange is the latest commit that touched the path, nil when it cannot
// be found (the page does not depend on it).
func (s *Server) lastChange(ctx context.Context, repo string, t target) *CommitRow {
	res, err := s.cfg.Git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{
		Repo: repo, Revision: t.rev, Path: t.path, PageSize: 1,
	}))
	if err != nil || len(res.Msg.GetCommits()) != 1 {
		return nil
	}
	c := res.Msg.GetCommits()[0]
	row := &CommitRow{
		Short: short(c.GetId()), Subject: markedText(c.GetSummary()), Author: c.GetAuthor().GetName(),
		CHref: repoHref(repo, "commit", c.GetId()),
	}
	if ts := c.GetCommitter().GetTime(); ts.IsValid() {
		row.When, row.WhenAt = ago(s.cfg.Now(), ts.AsTime()), rfc3339(ts.AsTime())
	}
	return row
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
