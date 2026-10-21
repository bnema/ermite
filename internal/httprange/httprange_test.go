// SPDX-License-Identifier: AGPL-3.0-or-later

package httprange_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/httprange"
)

func TestSelect(t *testing.T) {
	const etag = `"sha256-abc"`
	for _, tc := range []struct {
		name                 string
		method, rng, ifRange string
		size                 int64
		offset, length       int64
		partial              bool
		err                  error
	}{
		{name: "no range", size: 10, length: 10},
		{name: "closed", rng: "bytes=2-4", size: 10, offset: 2, length: 3, partial: true},
		{name: "open", rng: "bytes=7-", size: 10, offset: 7, length: 3, partial: true},
		{name: "suffix", rng: "bytes=-4", size: 10, offset: 6, length: 4, partial: true},
		{name: "suffix longer than body", rng: "bytes=-40", size: 10, offset: 0, length: 10, partial: true},
		{name: "end cut to size", rng: "bytes=8-99", size: 10, offset: 8, length: 2, partial: true},
		{name: "spaces", rng: "bytes= 1-2 ", size: 10, offset: 1, length: 2, partial: true},
		{name: "start past end", rng: "bytes=10-", size: 10, err: httprange.ErrUnsatisfiable},
		{name: "end before start", rng: "bytes=5-4", size: 10, err: httprange.ErrUnsatisfiable},
		{name: "zero suffix", rng: "bytes=-0", size: 10, err: httprange.ErrUnsatisfiable},
		{name: "garbage", rng: "bytes=a-b", size: 10, err: httprange.ErrUnsatisfiable},
		{name: "no dash", rng: "bytes=5", size: 10, err: httprange.ErrUnsatisfiable},
		{name: "empty body", rng: "bytes=0-", size: 0, err: httprange.ErrUnsatisfiable},
		{name: "multiple ignored", rng: "bytes=0-1,3-4", size: 10, length: 10},
		{name: "other unit ignored", rng: "items=0-1", size: 10, length: 10},
		{name: "head ignores range", method: http.MethodHead, rng: "bytes=2-4", size: 10, length: 10},
		{name: "head ignores bad range", method: http.MethodHead, rng: "bytes=99-", size: 10, length: 10},
		{name: "matching if-range", rng: "bytes=2-4", ifRange: etag, size: 10, offset: 2, length: 3, partial: true},
		{name: "stale if-range", rng: "bytes=2-4", ifRange: `"other"`, size: 10, length: 10},
		{name: "date if-range", rng: "bytes=2-4", ifRange: "Wed, 21 Oct 2015 07:28:00 GMT", size: 10, length: 10},
		{name: "weak if-range", rng: "bytes=2-4", ifRange: "W/" + etag, size: 10, length: 10},
		{name: "stale if-range with bad range", rng: "bytes=99-", ifRange: `"other"`, size: 10, length: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			r := httptest.NewRequest(method, "/x", nil)
			if tc.rng != "" {
				r.Header.Set("Range", tc.rng)
			}
			if tc.ifRange != "" {
				r.Header.Set("If-Range", tc.ifRange)
			}
			offset, length, partial, err := httprange.Select(r, tc.size, etag)
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, tc.offset, offset)
			require.Equal(t, tc.length, length)
			require.Equal(t, tc.partial, partial)
		})
	}
}

func TestContentRange(t *testing.T) {
	require.Equal(t, "bytes 2-4/10", httprange.ContentRange(2, 3, 10))
	require.Equal(t, "bytes */10", httprange.Unsatisfied(10))
}
