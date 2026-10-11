// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// rawTimeout bounds the stream of one file to the visitor.
const rawTimeout = 5 * time.Minute

// rawCSP makes a raw file inert if a browser opens it as a document: it can
// load nothing and run nothing, whatever it contains.
const rawCSP = "default-src 'none'; sandbox"

// raw streams the content of a file. Text is served as text/plain, the four
// image types (by their bytes, never SVG) as images, and anything else as an
// attachment of unknown type. A file is never served as HTML or SVG, so what
// a repository holds cannot run in the origin of the forge.
func (s *Server) raw(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), rawTimeout)
	defer cancel()
	t, repo, err := s.rawTarget(ctx, r)
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	// A revalidation of an unchanged file ends here, before the content is
	// opened: for a large blob, opening starts a Git process.
	if match := r.Header.Get("If-None-Match"); match != "" && s.rawUnchanged(ctx, repo, t, match) {
		h := w.Header()
		h.Set("ETag", match)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", rawCSP)
		cacheHeaders(ctx, h, "no-cache", sessionCache)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	b, err := s.openBlob(ctx, repo, t)
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	defer b.close()

	// The type needs the first bytes; headers must not be sent before.
	if err := b.fill(512); err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	typ, attach := rawType(b)
	h := w.Header()
	h.Set("Content-Type", typ)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", rawCSP)
	h.Set("Content-Length", strconv.FormatInt(b.info.GetSize(), 10))
	if attach {
		name := path.Base(t.path)
		cd := mime.FormatMediaType("attachment", map[string]string{"filename": name})
		if cd == "" {
			cd = "attachment"
		}
		h.Set("Content-Disposition", cd)
	}
	if etag := b.info.GetId(); etag != "" {
		h.Set("ETag", `"`+etag+`"`)
	}
	// Revalidated every time, even at a full commit id: a repository can turn
	// private, and a shared cache must not keep serving its files (AC2). The
	// ETag keeps the revalidation cheap.
	cacheHeaders(ctx, h, "no-cache", sessionCache)
	if match := r.Header.Get("If-None-Match"); match != "" && h.Get("ETag") != "" && match == h.Get("ETag") {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(b.buf); err != nil {
		return
	}
	b.buf = nil
	for b.stream.Receive() {
		if _, err := w.Write(b.stream.Msg().GetChunk()); err != nil {
			return
		}
	}
	if err := b.stream.Err(); err != nil {
		// Headers are gone: all that is left is to cut the answer short, so
		// that the client sees it is incomplete.
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("repo", repo).Str("path", logPath(r.URL.Path)).Msg("raw stream failed")
		panic(http.ErrAbortHandler)
	}
}

// rawTarget finds the file a raw URL designates and the name of its repository.
func (s *Server) rawTarget(ctx context.Context, r *http.Request) (target, string, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return target{}, "", err
	}
	t, ok := d.locate(r.PathValue("rest"))
	if !ok || t.path == "" {
		return target{}, "", errNotFound
	}
	return t, d.name(), nil
}

// rawUnchanged reports whether match is the ETag of the file, which is the id
// of its blob. The id comes from the entry of the file in its directory, so
// nothing of the content is read. Any doubt (the entry is not found, the
// listing is cut, the call fails) answers false: the file is then opened and
// the real outcome, error included, follows.
func (s *Server) rawUnchanged(ctx context.Context, repo string, t target, match string) bool {
	dir, _ := path.Split(t.path)
	res, err := s.cfg.Git.GetTree(ctx, connect.NewRequest(&pb.GetTreeRequest{
		Repo: repo, Revision: t.rev, Path: strings.TrimSuffix(dir, "/"),
	}))
	if err != nil {
		return false
	}
	for _, e := range res.Msg.GetEntries() {
		if e.GetPath() != t.path {
			continue
		}
		switch e.GetKind() {
		case pb.EntryKind_ENTRY_KIND_FILE, pb.EntryKind_ENTRY_KIND_EXECUTABLE, pb.EntryKind_ENTRY_KIND_SYMLINK:
			return e.GetId() != "" && match == `"`+e.GetId()+`"`
		}
		return false
	}
	return false
}

// rawType is the Content-Type of a file and whether it is an attachment.
// b.buf holds the first bytes (up to 512 when the file has that many).
func rawType(b *blobStream) (typ string, attachment bool) {
	if img := sniffImage(b.buf); img != "" {
		return img, false
	}
	if !b.info.GetBinary() {
		return "text/plain; charset=utf-8", false
	}
	return "application/octet-stream", true
}
