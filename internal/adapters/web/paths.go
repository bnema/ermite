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
	"strings"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
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
// first (ResolveRevision runs the access rule) and only then served from the
// cache of lists. Caching follows raw: a visitor revalidates every time, with
// an ETag that makes it a 304; the owner's copy is private and never stored.
func (s *Server) paths(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	repo, sha := r.PathValue("repo"), r.PathValue("sha")
	if !isFullID(sha) {
		s.fail(ctx, w, r, errNotFound)
		return
	}
	res, err := s.cfg.Git.ResolveRevision(ctx, connect.NewRequest(&pb.ResolveRevisionRequest{Repo: repo, Revision: sha}))
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	// A ref can be named like a commit id: only the commit itself is served,
	// since the cache is keyed by it.
	if res.Msg.GetCommitId() != sha {
		s.fail(ctx, w, r, errNotFound)
		return
	}
	body, err := s.pathCache.load(ctx, sha, func() ([]byte, error) { return s.buildPaths(ctx, repo, sha) })
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Vary", "Accept-Encoding")
	h.Set("ETag", `"paths-`+sha+`"`)
	// Revalidated every time, as raw is: a repository can turn private, and a
	// shared cache must not keep serving its files. The ETag keeps it cheap.
	cacheHeaders(ctx, h, "no-cache", sessionCache)
	if etagMatches(r.Header.Get("If-None-Match"), h.Get("ETag")) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		h.Set("Content-Encoding", "gzip")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, zr)
	}
}

// buildPaths lists the paths of a commit and returns them as gzipped JSON.
// The build is shared by the requests that wait for it, so it does not stop
// when the request that started it ends.
func (s *Server) buildPaths(ctx context.Context, repo, sha string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
	defer cancel()
	res, err := s.cfg.Git.ListPaths(ctx, connect.NewRequest(&pb.ListPathsRequest{Repo: repo, Revision: sha}))
	if err != nil {
		return nil, err
	}
	if res.Msg.GetCommitId() != sha {
		return nil, errNotFound
	}
	paths := res.Msg.GetPaths()
	if paths == nil {
		paths = []string{}
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(zw).Encode(pathList{Paths: paths, Truncated: res.Msg.GetTruncated()}); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip.
func acceptsGzip(h string) bool {
	for f := range strings.SplitSeq(h, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(f), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		if q, ok := strings.CutPrefix(strings.ReplaceAll(strings.TrimSpace(params), " ", ""), "q="); ok {
			if v, err := strconv.ParseFloat(q, 64); err == nil && v <= 0 {
				return false
			}
		}
		return true
	}
	return false
}

// etagMatches reports whether an If-None-Match header lists etag (weak
// comparison, or "*").
func etagMatches(header, etag string) bool {
	for f := range strings.SplitSeq(header, ",") {
		f = strings.TrimSpace(f)
		if f == "*" || strings.TrimPrefix(f, "W/") == etag {
			return true
		}
	}
	return false
}
