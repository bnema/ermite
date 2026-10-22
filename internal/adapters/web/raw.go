// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/httprange"
)

// rawTimeout bounds the stream of one file to the visitor.
const rawTimeout = 5 * time.Minute

// rawChunkSize is the size of the writes that stream a file to the visitor.
const rawChunkSize = 64 << 10

// rawCSP makes a raw file inert if a browser opens it as a document: it can
// load nothing and run nothing, whatever it contains.
const rawCSP = "default-src 'none'; sandbox"

// raw streams the content of a file. Text is served as text/plain, the four
// image types and videos (by their bytes, never SVG) inline, and anything else as an
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

	// Git blob readers are forward-only. Seeking discards the prefix for each
	// request: bounded memory, but O(offset) I/O, intended for small video clips.
	// Reuse the single-range policy; multipart ranges are ignored, never buffered.
	h.Set("Accept-Ranges", "bytes")
	offset, length, partial, err := httprange.Select(r, b.info.Size, h.Get("ETag"))
	if err != nil {
		h.Set("Content-Range", httprange.Unsatisfied(b.info.Size))
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		h.Set("Content-Range", httprange.ContentRange(offset, length, b.info.Size))
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	reader := io.MultiReader(bytes.NewReader(b.buf), b.body)
	if _, err = io.CopyN(io.Discard, reader, offset); err == nil {
		var written int64
		written, err = io.CopyBuffer(w, io.LimitReader(reader, length), make([]byte, rawChunkSize))
		if err == nil && written != length {
			err = io.ErrUnexpectedEOF
		}
	}
	if err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("repo", repo.String()).Str("path", logPath(r.URL.Path)).Msg("raw stream failed")
		panic(http.ErrAbortHandler)
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
	if video := sniffVideo(b.buf); video != "" {
		return video, false
	}
	if !b.info.Binary {
		return "text/plain; charset=utf-8", false
	}
	return "application/octet-stream", true
}

// sniffVideo admits known video signatures only, never HTML or SVG by extension.
func sniffVideo(head []byte) string {
	switch http.DetectContentType(head) {
	case "video/mp4":
		return "video/mp4"
	case "video/webm":
		return "video/webm"
	}
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		switch string(head[8:12]) {
		case "qt  ":
			return "video/quicktime"
		case "M4V ", "isom", "iso2", "mp41", "mp42", "avc1":
			return "video/mp4"
		}
	}
	// Ogg is a container for audio as well: require the Theora identification packet.
	if bytes.HasPrefix(head, []byte("OggS\x00")) && bytes.Contains(head, []byte("\x80theora")) {
		return "video/ogg"
	}
	return ""
}
