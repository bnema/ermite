// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"bnema.dev/ermite/internal/domain"
)

// errBuildPanicked is what the waiters of a path list get when its build
// panicked.
var errBuildPanicked = errors.New("web: building the path list failed")

// pathList is the JSON body of GET /{repo}/paths/{sha}.
type pathList struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
}

// paths is GET /{repo}/paths/{sha}: the file paths of a commit as JSON, for
// the file finder (files.js). The commit must be a full id; the answer never
// changes for it, but who may read it does, so every request is authorized
// first (ResolveRevision runs the access rule, as the caller of the request) and only then served from the
// cache of lists. Caching follows raw: a visitor revalidates every time, with
// an ETag that makes it a 304 (answered before the list is loaded, so it never
// waits for a build); the owner's copy is private and never stored. The ETag
// differs for the gzip and the plain variant.
func (s *Server) paths(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	sha := r.PathValue("sha")
	if !isFullID(sha) {
		s.fail(ctx, w, r, errNotFound)
		return
	}
	repo, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	res, err := s.resolveRevision(ctx, repo, sha)
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	// A ref can be named like a commit id: only the commit itself is served,
	// since the cache is keyed by it.
	if res.CommitID.String() != sha {
		s.fail(ctx, w, r, errNotFound)
		return
	}

	// The validator names the variant that would be served: the gzip bytes
	// and the plain JSON are different representations.
	gz := acceptsGzip(r.Header)
	etag := `"paths-` + sha + `"`
	if gz {
		etag = `"paths-` + sha + `-gz"`
	}
	// Revalidated every time, as raw is: a repository can turn private, and a
	// shared cache must not keep serving its files. The ETag keeps it cheap.
	validators := func(h http.Header) {
		h.Set("Vary", "Accept-Encoding")
		h.Set("ETag", etag)
		cacheHeaders(ctx, h, "no-cache", sessionCache)
	}
	// Access is settled: a revalidation is answered without the list, so it
	// never waits for a build.
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		validators(w.Header())
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body, err := s.pathCache.load(ctx, repo.String(), sha, func() ([]byte, error) { return s.buildPaths(ctx, repo, sha) })
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	// Authorized, not modified and loaded: all that is left is to send it.
	validators(w.Header())
	if err := writePathList(w, r, body, gz); err != nil {
		s.fail(ctx, w, r, err)
	}
}

// writePathList sends a list (gzipped JSON, as the cache keeps it): as it is
// to a client that accepts gzip, unzipped to the others. The validators are
// already set. It fails only before anything is written.
func writePathList(w http.ResponseWriter, r *http.Request, body []byte, gz bool) error {
	var plain *gzip.Reader
	if !gz {
		var err error
		if plain, err = gzip.NewReader(bytes.NewReader(body)); err != nil {
			return err
		}
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	if gz {
		h.Set("Content-Encoding", "gzip")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return nil
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, plain)
	}
	return nil
}

// buildPaths lists the paths of a commit and returns them as gzipped JSON.
// The build is shared by the requests that wait for it, so it does not stop
// when the request that started it ends.
func (s *Server) buildPaths(ctx context.Context, repo domain.RepoName, sha string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
	defer cancel()
	list, err := s.listPaths(ctx, repo, sha)
	if err != nil {
		return nil, err
	}
	if list.CommitID.String() != sha {
		return nil, errNotFound
	}
	paths := list.Paths
	if paths == nil {
		paths = []string{}
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false) // a path is not HTML; "<", ">" and "&" stay as they are
	if err := enc.Encode(pathList{Paths: paths, Truncated: list.Truncated}); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	// Not buf.Bytes(): the buffer's spare capacity would stay in the cache
	// without being counted in its budget. (bytes.Clone may round the
	// capacity up, so the copy is exact.)
	body := make([]byte, buf.Len())
	copy(body, buf.Bytes())
	return body, nil
}
