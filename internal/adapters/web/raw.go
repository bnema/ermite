// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
)

// rawTimeout bounds the stream of one file to the visitor.
const rawTimeout = 5 * time.Minute

// rawChunkSize is the size of the writes that stream a file to the visitor.
const rawChunkSize = 64 << 10

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
	h.Set("Content-Length", strconv.FormatInt(b.info.Size, 10))
	if attach {
		name := path.Base(t.path)
		cd := mime.FormatMediaType("attachment", map[string]string{"filename": name})
		if cd == "" {
			cd = "attachment"
		}
		h.Set("Content-Disposition", cd)
	}
	if etag := b.info.ID.String(); etag != "" {
		h.Set("ETag", `"`+etag+`"`)
	}
	// Revalidated every time, even at a full commit id: a repository can turn
	// private, and a shared cache must not keep serving its files (AC2). The
	// ETag spares the body: the blob id comes with the metadata, before the
	// content, and a match ends the answer there. No cheaper call gives the
	// id of the blob at a path: a tree listing of its directory costs more
	// than opening a small file.
	cacheHeaders(ctx, h, "no-cache", sessionCache)
	if etag := h.Get("ETag"); etag != "" && etagMatches(r.Header.Get("If-None-Match"), etag) {
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
	if b.eof {
		return
	}
	chunk := make([]byte, rawChunkSize)
	for {
		n, rerr := b.body.Read(chunk)
		if n > 0 {
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
		}
		if errors.Is(rerr, io.EOF) {
			return
		}
		if rerr != nil {
			// Headers are gone: all that is left is to cut the answer short,
			// so that the client sees it is incomplete.
			log := zerowrap.FromCtx(ctx)
			log.Error().Err(rerr).Str("repo", repo.String()).Str("path", logPath(r.URL.Path)).Msg("raw stream failed")
			panic(http.ErrAbortHandler)
		}
	}
}

// rawTarget finds the file a raw URL designates and the name of its repository.
func (s *Server) rawTarget(ctx context.Context, r *http.Request) (target, domain.RepoName, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return target{}, domain.RepoName{}, err
	}
	t, ok := d.locate(r.PathValue("rest"))
	if !ok || t.path == "" {
		return target{}, domain.RepoName{}, errNotFound
	}
	return t, d.repo.Name, nil
}

// rawType is the Content-Type of a file and whether it is an attachment.
// b.buf holds the first bytes (up to 512 when the file has that many).
func rawType(b *blobStream) (typ string, attachment bool) {
	if img := sniffImage(b.buf); img != "" {
		return img, false
	}
	if !b.info.Binary {
		return "text/plain; charset=utf-8", false
	}
	return "application/octet-stream", true
}
