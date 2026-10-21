// SPDX-License-Identifier: AGPL-3.0-or-later

package githubapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/bnema/zerowrap"
)

func releaseRoute(rest string) (kind, owner, repo, ref string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	upload := len(parts) > 0 && parts[0] == "uploads"
	if upload {
		parts = parts[1:]
	}
	if len(parts) < 4 || parts[0] != "repos" || parts[1] == "" || parts[2] == "" || parts[3] != "releases" {
		return
	}
	owner, repo = parts[1], parts[2]
	if upload {
		if len(parts) == 6 && isNumber(parts[4]) && parts[5] == "assets" {
			return "release-upload", owner, repo, parts[4], true
		}
		return
	}
	switch {
	case len(parts) == 4:
		return "release-list", owner, repo, "", true
	case len(parts) == 5 && parts[4] == "latest":
		return "release-latest", owner, repo, "", true
	case len(parts) >= 6 && parts[4] == "tags" && strings.Join(parts[5:], "/") != "":
		return "release-tag", owner, repo, strings.Join(parts[5:], "/"), true
	case len(parts) == 5 && isNumber(parts[4]):
		return "release-id", owner, repo, parts[4], true
	case len(parts) == 6 && isNumber(parts[4]) && parts[5] == "assets":
		return "release-assets", owner, repo, parts[4], true
	case len(parts) == 6 && parts[4] == "assets" && isNumber(parts[5]):
		return "release-asset", owner, repo, parts[5], true
	}
	return
}
func releaseMethods(kind string) string {
	switch kind {
	case "release-list":
		return "GET, HEAD, POST"
	case "release-id", "release-asset":
		return "GET, HEAD, PATCH, DELETE"
	case "release-upload":
		return "POST"
	default:
		return "GET, HEAD"
	}
}
func methodAllowed(method, allow string) bool {
	for _, m := range strings.Split(allow, ", ") {
		if method == m {
			return true
		}
	}
	return false
}
func releaseJSON(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
func releaseMessage(w http.ResponseWriter, status int, message string) {
	b, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{message})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
func releaseBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			releaseMessage(w, 422, "Body too large")
		} else {
			releaseMessage(w, 422, "Validation Failed")
		}
		return nil, false
	}
	return b, true
}
func (h *Handler) releaseRequest(w http.ResponseWriter, r *http.Request, p domain.Principal, kind, owner, repo, ref, client string) {
	ctx := r.Context()
	api := h.cfg.Releases
	id, _ := strconv.ParseInt(ref, 10, 64)
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	var body []byte
	var err error
	var link string
	status := http.StatusOK
	if kind == "release-asset" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/octet-stream") {
		if h.cfg.ReleaseDownload == nil {
			notFound(w)
			return
		}
		h.downloadRelease(w, r, p, owner, repo, id, client)
		return
	}
	switch kind {
	case "release-list":
		if r.Method == http.MethodPost {
			b, ok := releaseBody(w, r)
			if !ok {
				return
			}
			body, err = api.CreateRelease(ctx, p, owner, repo, b)
			status = 201
		} else {
			body, link, err = api.ListReleases(ctx, p, owner, repo, page, perPage)
		}
	case "release-latest":
		body, err = api.GetRelease(ctx, p, owner, repo, ports.ReleaseRef{Latest: true})
	case "release-tag":
		body, err = api.GetRelease(ctx, p, owner, repo, ports.ReleaseRef{Tag: ref})
	case "release-id":
		switch r.Method {
		case http.MethodPatch:
			b, ok := releaseBody(w, r)
			if !ok {
				return
			}
			body, err = api.UpdateRelease(ctx, p, owner, repo, id, b)
		case http.MethodDelete:
			err = api.DeleteRelease(ctx, p, owner, repo, id)
			status = 204
		default:
			body, err = api.GetRelease(ctx, p, owner, repo, ports.ReleaseRef{ID: id})
		}
	case "release-assets":
		body, link, err = api.ListAssets(ctx, p, owner, repo, id, page, perPage)
	case "release-upload":
		if r.ContentLength < 0 || (r.ContentLength == 0 && r.Header.Get("Content-Length") == "") {
			releaseMessage(w, 411, "Length Required")
			return
		}
		if q.Get("name") == "" {
			h.failRelease(ctx, w, p, domain.ErrInvalid, "release-upload", "name")
			return
		}
		ct := r.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/octet-stream"
		}
		body, err = api.UploadAsset(ctx, p, owner, repo, id, q.Get("name"), q.Get("label"), ct, r.ContentLength, io.LimitReader(r.Body, r.ContentLength))
		status = 201
	case "release-asset":
		switch r.Method {
		case http.MethodPatch:
			b, ok := releaseBody(w, r)
			if !ok {
				return
			}
			body, err = api.UpdateAsset(ctx, p, owner, repo, id, b)
		case http.MethodDelete:
			err = api.DeleteAsset(ctx, p, owner, repo, id)
			status = 204
		default:
			body, err = api.GetAsset(ctx, p, owner, repo, id)
		}
	}
	if err != nil {
		h.failRelease(ctx, w, p, err, kind, "")
		return
	}
	if link != "" {
		w.Header().Set("Link", link)
	}
	if status == 204 {
		w.WriteHeader(status)
		return
	}
	releaseJSON(w, r, status, body)
}

// failRelease translates use-case failures to GitHub's status and error JSON;
// authorization and limits have already been decided by ReleaseService.
//
//hexcheck:ignore suspicious-business-logic-in-adapter maps domain failures and the principal kind to the GitHub HTTP error contract; no access or quota decision is made here
func (h *Handler) failRelease(ctx context.Context, w http.ResponseWriter, p domain.Principal, err error, kind, field string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		notFound(w)
	case errors.Is(err, domain.ErrUnauthenticated):
		releaseMessage(w, 401, "Bad credentials")
	case errors.Is(err, domain.ErrForbidden):
		msg := "Forbidden"
		if p.ViaJobToken() {
			msg = "Resource not accessible by integration"
		}
		releaseMessage(w, 403, msg)
	case errors.Is(err, domain.ErrResourceExhausted):
		if strings.Contains(err.Error(), "too many uploads in progress") {
			w.Header().Set("Retry-After", retryAfter)
			releaseMessage(w, 429, err.Error())
		} else {
			releaseMessage(w, 422, err.Error())
		}
	case errors.Is(err, domain.ErrAlreadyExists), domain.IsInvalid(err), errors.Is(err, domain.ErrPreconditionFailed):
		writeReleaseValidation(w, err, kind, field)
	case errors.Is(err, context.Canceled):
	default:
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Msg("github release request failed")
		releaseMessage(w, 500, "Server Error")
	}
}

type releaseValidationError struct {
	Resource string `json:"resource"`
	Code     string `json:"code"`
	Field    string `json:"field,omitempty"`
}

func writeReleaseValidation(w http.ResponseWriter, err error, kind, field string) {
	resource, code, doc := "Release", "invalid", ""
	if errors.Is(err, domain.ErrAlreadyExists) {
		code = "already_exists"
		field = "tag_name"
		if kind == "release-upload" || kind == "release-asset" {
			resource = "ReleaseAsset"
			field = "name"
		}
		if kind == "release-upload" {
			doc = "https://docs.github.com/rest/releases/assets#upload-a-release-asset"
		}
	} else if errors.Is(err, domain.ErrPreconditionFailed) {
		field = "target_commitish"
	} else if field == "" {
		field = releaseErrorField(err.Error())
	}
	b, _ := json.Marshal(struct {
		Message          string                   `json:"message"`
		Errors           []releaseValidationError `json:"errors"`
		DocumentationURL string                   `json:"documentation_url,omitempty"`
	}{"Validation Failed", []releaseValidationError{{resource, code, field}}, doc})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(422)
	_, _ = w.Write(b)
}

// releaseErrorField recognises the wire fields and domain validation vocabulary.
// Errors without a named field omit it rather than guessing.
func releaseErrorField(text string) string {
	for _, f := range []string{"target_commitish", "make_latest", "tag_name", "prerelease", "draft", "content_type", "label", "body"} {
		if strings.Contains(text, f) {
			return f
		}
	}
	if strings.Contains(text, "tag name") {
		return "tag_name"
	}
	if strings.Contains(text, "media type") {
		return "content_type"
	}
	if strings.Contains(text, "name") {
		return "name"
	}
	return ""
}

// assetRange returns a single byte range. Multiple ranges and unknown units are
// ignored. Unsatisfiable or malformed byte ranges return false.
func assetRange(header string, size int64) (offset, length int64, partial, ok bool) {
	if header == "" || !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, size, false, true
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	a, b, found := strings.Cut(spec, "-")
	if !found || size <= 0 {
		return 0, 0, false, false
	}
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		n = min(n, size)
		return size - n, n, true, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, false
	}
	end := size - 1
	if b != "" {
		end, err = strconv.ParseInt(b, 10, 64)
		if err != nil || end < start {
			return 0, 0, false, false
		}
		end = min(end, size-1)
	}
	return start, end - start + 1, true, true
}
func releaseContentDisposition(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' || r == '\\' || r == '/' || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		name = "asset"
	}
	ascii := true
	fallback := strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII {
			ascii = false
			return '_'
		}
		return r
	}, name)
	// Quote even token-safe names, keeping a consistent RFC 6266 representation.
	cd := "attachment; filename=" + strconv.Quote(fallback)
	if !ascii {
		const digits = "0123456789ABCDEF"
		var b strings.Builder
		for i := 0; i < len(name); i++ {
			c := name[i]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
				b.WriteByte(c)
			} else {
				b.WriteByte('%')
				b.WriteByte(digits[c>>4])
				b.WriteByte(digits[c&15])
			}
		}
		cd += "; filename*=UTF-8''" + b.String()
	}
	return cd
}
func setReleaseDownloadHeaders(w http.ResponseWriter, a domain.ReleaseAsset) string {
	ct := a.ContentType
	media, _, err := mime.ParseMediaType(ct)
	media = strings.ToLower(media)
	if err != nil || strings.HasPrefix(media, "text/html") || media == "application/javascript" || media == "image/svg+xml" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", releaseContentDisposition(a.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Accept-Ranges", "bytes")
	etag := `"sha256-` + strings.TrimPrefix(a.Digest, "sha256:") + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", a.UpdatedAt.UTC().Format(http.TimeFormat))
	return etag
}
func (h *Handler) downloadRelease(w http.ResponseWriter, r *http.Request, p domain.Principal, owner, repo string, id int64, client string) {
	ctx := r.Context()
	a, err := h.cfg.Releases.AssetByID(ctx, p, owner, repo, id)
	if err != nil {
		h.failRelease(ctx, w, p, err, "release-asset", "")
		return
	}
	if r.Method != http.MethodHead {
		release, ok := h.acquire(w, client)
		if !ok {
			return
		}
		defer release()
	}
	etag := setReleaseDownloadHeaders(w, a)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	offset, length, partial, ok := assetRange(r.Header.Get("Range"), a.Size)
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(a.Size, 10))
		w.WriteHeader(416)
		return
	}
	status := 200
	if partial {
		status = 206
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(offset+length-1, 10)+"/"+strconv.FormatInt(a.Size, 10))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	// Slot failures and pre-stream failures are JSON, not a binary response.
	clearHeaders := func() {
		for _, k := range []string{"Content-Length", "Content-Range", "Content-Disposition", "ETag", "Last-Modified", "Accept-Ranges", "Content-Security-Policy"} {
			w.Header().Del(k)
		}
	}
	writer := &idleWriter{w: w, rc: http.NewResponseController(w), idle: h.idle}
	started := false
	err = h.cfg.ReleaseDownload.DownloadAsset(ctx, p, a.Repo, ports.AssetRef{ID: id}, offset, length, func(chunk ports.AssetChunk) error {
		if len(chunk.Data) == 0 {
			return nil
		}
		if !started {
			w.WriteHeader(status)
			started = true
		}
		_, e := writer.Write(chunk.Data)
		return e
	})
	// BlobStore.Open reports offset >= size as out of range, including the
	// valid empty object at offset zero. It has still checked existence.
	if err == nil || (a.Size == 0 && errors.Is(err, domain.ErrOutOfRange)) {
		if !started {
			w.WriteHeader(status)
		}
		return
	}
	if started {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Msg("github release download cut short")
		panic(http.ErrAbortHandler)
	}
	clearHeaders()
	if errors.Is(err, domain.ErrOutOfRange) {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(a.Size, 10))
		w.WriteHeader(416)
		return
	}
	h.failRelease(ctx, w, p, err, "release-asset", "")
}
