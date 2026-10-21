// SPDX-License-Identifier: AGPL-3.0-or-later

// Package httprange selects the byte range of a download from the Range and
// If-Range headers of a request. The HTTP adapters that serve release assets
// share it, so they answer the same way.
package httprange

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// ErrUnsatisfiable means the request asked for a range the content cannot give.
// The caller answers 416 with "Content-Range: bytes */<size>".
var ErrUnsatisfiable = errors.New("httprange: unsatisfiable byte range")

// Select returns the part of a size-byte body to send.
//
// Without a usable Range it returns the whole body (0, size, false, nil):
//   - the method is not GET (HEAD answers with the headers of the full body);
//   - there is no Range header, its unit is not "bytes", or it lists several
//     ranges (ADR 0034: multipart ranges are not served);
//   - If-Range is present and is not exactly etag. The comparison is strong:
//     a weak validator or an HTTP date never matches.
//
// A single "bytes=a-b", "bytes=a-" or "bytes=-n" gives (offset, length, true,
// nil); an end past the content is cut to it. A malformed range, a start at or
// past size, a suffix of zero, or any range of an empty body returns
// ErrUnsatisfiable.
func Select(r *http.Request, size int64, etag string) (offset, length int64, partial bool, err error) {
	whole := func() (int64, int64, bool, error) { return 0, size, false, nil }
	if r.Method != http.MethodGet {
		return whole()
	}
	value := strings.Join(r.Header.Values("Range"), ",")
	if value == "" || strings.Contains(value, ",") || !strings.HasPrefix(value, "bytes=") {
		return whole()
	}
	if ifRange := r.Header.Get("If-Range"); ifRange != "" && (etag == "" || ifRange != etag || strings.HasPrefix(ifRange, "W/")) {
		return whole()
	}
	a, b, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(value, "bytes=")), "-")
	if !ok || size <= 0 {
		return 0, 0, false, ErrUnsatisfiable
	}
	if a == "" {
		n, e := strconv.ParseInt(b, 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, false, ErrUnsatisfiable
		}
		n = min(n, size)
		return size - n, n, true, nil
	}
	start, e := strconv.ParseInt(a, 10, 64)
	if e != nil || start < 0 || start >= size {
		return 0, 0, false, ErrUnsatisfiable
	}
	end := size - 1
	if b != "" {
		end, e = strconv.ParseInt(b, 10, 64)
		if e != nil || end < start {
			return 0, 0, false, ErrUnsatisfiable
		}
		end = min(end, size-1)
	}
	return start, end - start + 1, true, nil
}

// ContentRange is the Content-Range value of a partial answer.
func ContentRange(offset, length, size int64) string {
	return "bytes " + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(offset+length-1, 10) + "/" + strconv.FormatInt(size, 10)
}

// Unsatisfied is the Content-Range value of a 416 answer.
func Unsatisfied(size int64) string { return "bytes */" + strconv.FormatInt(size, 10) }
