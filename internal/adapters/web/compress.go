// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// minCompress is the size under which a page is sent as it is: the gzip
// header and the CPU cost are not worth it for a few hundred bytes.
const minCompress = 1024

// gzipLevel is the level of the pages, compressed on each request: a good
// ratio at a cost small enough for a render that already takes longer. The
// assets, compressed once at start, use the best level.
const gzipLevel = 5

// gzipWriters recycles the writers of the pages: a gzip.Writer holds about
// 800 KiB of state.
var gzipWriters = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(nil, gzipLevel) // the level is valid
	return zw
}}

// gzipPage compresses b with a pooled writer.
func gzipPage(b []byte) ([]byte, error) {
	var out bytes.Buffer
	zw := gzipWriters.Get().(*gzip.Writer)
	defer func() {
		// Drop the reference to out, so a pooled writer does not keep the
		// last page alive.
		zw.Reset(io.Discard)
		gzipWriters.Put(zw)
	}()
	zw.Reset(&out)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// gzipBest compresses b once, at the best level (for the assets).
func gzipBest(b []byte) ([]byte, error) {
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// acceptsGzip reports whether the Accept-Encoding of a request allows gzip
// (RFC 9110 §12.5.3): gzip, or its alias x-gzip, with a quality above 0, or
// "*" when gzip is not named. A quality of 0 refuses the coding; a malformed
// quality is a refusal as well.
func acceptsGzip(h http.Header) bool {
	star, starSet := false, false
	for _, line := range h.Values("Accept-Encoding") {
		for part := range strings.SplitSeq(line, ",") {
			coding, params, _ := strings.Cut(part, ";")
			coding = strings.ToLower(strings.TrimSpace(coding))
			if coding != "gzip" && coding != "x-gzip" && coding != "*" {
				continue
			}
			ok := qualityPositive(params)
			switch coding {
			case "*":
				star, starSet = ok, true
			default:
				return ok
			}
		}
	}
	return starSet && star
}

// qualityPositive reads the q parameter of a coding (1 when absent) and
// reports whether it is above 0.
func qualityPositive(params string) bool {
	for p := range strings.SplitSeq(params, ";") {
		k, v, found := strings.Cut(p, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil && q > 0
	}
	return true
}

// addVary adds a field to Vary unless it is already there (or Vary is *).
func addVary(h http.Header, field string) {
	for _, v := range h.Values("Vary") {
		for f := range strings.SplitSeq(v, ",") {
			if t := strings.TrimSpace(f); t == "*" || strings.EqualFold(t, field) {
				return
			}
		}
	}
	h.Add("Vary", field)
}

// noTransform tells whether Cache-Control forbids changing the body.
func noTransform(h http.Header) bool {
	return hasCacheDirective(h, "no-transform")
}

// hasCacheDirective tells whether Cache-Control holds the directive.
func hasCacheDirective(h http.Header, name string) bool {
	for _, v := range h.Values("Cache-Control") {
		for d := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(d), name) {
				return true
			}
		}
	}
	return false
}

// pageETag is the strong validator of a rendered page: a hash of its
// identity bytes and of the session state, so that the page of the owner
// never shares a tag with the page of a visitor. The gzip representation
// has a tag of its own (RFC 9110 §8.8.3).
func pageETag(body []byte, signedIn, gzipped bool) string {
	h := sha256.New()
	if signedIn {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write(body)
	tag := hex.EncodeToString(h.Sum(nil)[:16])
	if gzipped {
		tag += "-gz"
	}
	return `"` + tag + `"`
}

// etagMatches implements the weak comparison of If-None-Match (RFC 9110
// §13.1.2): a list of entity tags, each optionally weak, or "*".
func etagMatches(header, etag string) bool {
	want := strings.TrimPrefix(etag, "W/")
	s := header
	for {
		s = strings.TrimLeft(s, " \t,")
		if s == "" {
			return false
		}
		if s[0] == '*' {
			return true
		}
		s = strings.TrimPrefix(s, "W/")
		if s == "" || s[0] != '"' {
			return false // malformed: nothing after this can be trusted
		}
		end := strings.IndexByte(s[1:], '"')
		if end < 0 {
			return false
		}
		if s[:end+2] == want {
			return true
		}
		s = s[end+2:]
	}
}
