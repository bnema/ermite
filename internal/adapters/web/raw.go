// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/bnema/zerowrap"
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
	// ETag spares the body: the blob id comes with the first message of
	// the stream, and a match ends the answer there. No cheaper call gives the
	// id of the blob at a path: a tree listing of its directory costs more
	// than opening a small file.
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
