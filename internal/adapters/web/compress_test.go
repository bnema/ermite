// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/ports/mocks"
)

func TestAcceptsGzip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		header []string
		want   bool
	}{
		{"absent", nil, false},
		{"gzip", []string{"gzip"}, true},
		{"list", []string{"deflate, gzip, br"}, true},
		{"case", []string{"GZip"}, true},
		{"x-gzip alias", []string{"x-gzip"}, true},
		{"q=1", []string{"gzip;q=1.0"}, true},
		{"q spaces", []string{"gzip ; q=0.5"}, true},
		{"q=0 refuses", []string{"gzip;q=0"}, false},
		{"q=0.0 refuses", []string{"br, gzip;q=0.0"}, false},
		{"bad q refuses", []string{"gzip;q=abc"}, false},
		{"identity only", []string{"identity"}, false},
		{"br only", []string{"br"}, false},
		{"star", []string{"*"}, true},
		{"star q=0", []string{"*;q=0"}, false},
		{"gzip refused beats star", []string{"gzip;q=0, *"}, false},
		{"gzip accepted beats star q=0", []string{"*;q=0, gzip"}, true},
		{"several lines", []string{"br", "gzip"}, true},
		{"substring is not a coding", []string{"gzipped"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			for _, v := range tc.header {
				h.Add("Accept-Encoding", v)
			}
			assert.Equal(t, tc.want, acceptsGzip(h))
		})
	}
}

func TestEtagMatches(t *testing.T) {
	t.Parallel()
	const tag = `"abc"`
	for _, tc := range []struct {
		name, header, etag string
		want               bool
	}{
		{"empty", "", tag, false},
		{"same", `"abc"`, tag, true},
		{"other", `"abd"`, tag, false},
		{"weak client tag", `W/"abc"`, tag, true},
		{"list", `"x", "abc", "y"`, tag, true},
		{"list no space", `"x","abc"`, tag, true},
		{"list without it", `"x", W/"y"`, tag, false},
		{"star", `*`, tag, true},
		{"unquoted", `abc`, tag, false},
		{"unterminated", `"abc`, tag, false},
		{"prefix is not equal", `"ab"`, tag, false},
		{"weak server tag", `"abc"`, `W/"abc"`, true},
		{"comma in a tag", `"a,b", "abc"`, tag, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, etagMatches(tc.header, tc.etag))
		})
	}
}

func TestPageETag(t *testing.T) {
	t.Parallel()
	body := []byte("<html>x</html>")
	a := pageETag(body, false, false)
	assert.Equal(t, a, pageETag(body, false, false), "stable")
	assert.NotEqual(t, a, pageETag(body, true, false), "owner and visitor do not share a tag")
	assert.NotEqual(t, a, pageETag(body, false, true), "gzip is another representation")
	assert.NotEqual(t, a, pageETag([]byte("<html>y</html>"), false, false))
	assert.True(t, strings.HasPrefix(a, `"`) && strings.HasSuffix(a, `"`))
}

func newBareServer(t *testing.T) *Server {
	t.Helper()
	return newTestServer(t, Config{Repos: reposFailing(t, errors.New("x")), Browse: mocks.NewBrowseService(t)})
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return string(out)
}

// write: gzip by size and Accept-Encoding, validators, 304.
func TestWritePage(t *testing.T) {
	t.Parallel()
	big := "<p>" + strings.Repeat("ermite page ", 400) + "</p>"
	small := "<p>tiny</p>"

	for _, tc := range []struct {
		name     string
		method   string
		ae       string
		body     string
		status   int
		cc       string // Cache-Control set by the handler beforehand
		wantGzip bool
		wantTag  bool
	}{
		{"big page, gzip", "GET", "gzip", big, 200, "", true, true},
		{"big page, no Accept-Encoding", "GET", "", big, 200, "", false, true},
		{"big page, gzip refused", "GET", "gzip;q=0", big, 200, "", false, true},
		{"big page, br only", "GET", "br", big, 200, "", false, true},
		{"small page not compressed", "GET", "gzip", small, 200, "", false, true},
		{"HEAD keeps headers, no body", "HEAD", "gzip", big, 200, "", true, true},
		{"404 page compressed, no tag", "GET", "gzip", big, 404, "", true, false},
		{"no-store handler: no tag", "GET", "gzip", big, 200, "no-store", true, false},
		{"no-transform: untouched", "GET", "gzip", big, 200, "no-transform", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newBareServer(t)
			rec := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(context.Background(), tc.method, "/", nil)
			if tc.ae != "" {
				r.Header.Set("Accept-Encoding", tc.ae)
			}
			if tc.cc != "" {
				rec.Header().Set("Cache-Control", tc.cc)
			}
			srv.write(r.Context(), rec, r, tc.status, templ.Raw(tc.body))

			res := rec.Result()
			t.Cleanup(func() { _ = res.Body.Close() })
			raw, _ := io.ReadAll(res.Body)
			assert.Equal(t, tc.status, res.StatusCode)
			assert.Contains(t, res.Header.Values("Vary"), "Accept-Encoding")
			assert.Equal(t, tc.wantTag, res.Header.Get("ETag") != "")
			switch {
			case tc.method == "HEAD":
				assert.Empty(t, raw)
				assert.Equal(t, tc.wantGzip, res.Header.Get("Content-Encoding") == "gzip")
			case tc.wantGzip:
				assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
				assert.Equal(t, itoa(len(raw)), res.Header.Get("Content-Length"))
				assert.Less(t, len(raw), len(tc.body))
				assert.Equal(t, tc.body, gunzip(t, raw))
			default:
				assert.Empty(t, res.Header.Get("Content-Encoding"))
				assert.Equal(t, tc.body, string(raw))
			}
		})
	}
}

func TestWritePageConditional(t *testing.T) {
	t.Parallel()
	big := "<p>" + strings.Repeat("ermite page ", 400) + "</p>"
	srv := newBareServer(t)
	do := func(ae, inm string) *http.Response {
		rec := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
		if ae != "" {
			r.Header.Set("Accept-Encoding", ae)
		}
		if inm != "" {
			r.Header.Set("If-None-Match", inm)
		}
		srv.write(r.Context(), rec, r, 200, templ.Raw(big))
		res := rec.Result()
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}
	plain, gz := do("", "").Header.Get("ETag"), do("gzip", "").Header.Get("ETag")
	require.NotEmpty(t, plain)
	require.NotEmpty(t, gz)
	assert.NotEqual(t, plain, gz, "identity and gzip are different representations")

	for _, tc := range []struct {
		name, ae, inm string
		want          int
	}{
		{"identity match", "", plain, 304},
		{"gzip match", "gzip", gz, 304},
		{"list", "gzip", `"nope", ` + gz, 304},
		{"weak prefix", "", "W/" + plain, 304},
		{"star", "", "*", 304},
		{"identity tag for gzip request", "gzip", plain, 200},
		{"gzip tag for identity request", "", gz, 200},
		{"stale", "", `"stale"`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := do(tc.ae, tc.inm)
			assert.Equal(t, tc.want, res.StatusCode)
			b, _ := io.ReadAll(res.Body)
			if tc.want == 304 {
				assert.Empty(t, b)
				assert.NotEmpty(t, res.Header.Get("ETag"))
				assert.Equal(t, "no-cache", res.Header.Get("Cache-Control"))
				assert.Contains(t, res.Header.Values("Vary"), "Accept-Encoding")
				assert.Empty(t, res.Header.Get("Content-Encoding"))
			} else {
				assert.NotEmpty(t, b)
			}
		})
	}
}

// The owner's page is private and never revalidated, whatever the client sends.
func TestWritePageOwnerHasNoValidator(t *testing.T) {
	t.Parallel()
	srv := newBareServer(t)
	ctx := context.WithValue(context.Background(), signedInKey{}, struct{}{})
	rec := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(ctx, "GET", "/", nil)
	r.Header.Set("If-None-Match", "*")
	srv.write(ctx, rec, r, 200, templ.Raw("<p>mine</p>"))
	assert.Equal(t, 200, rec.Code)
	assert.Equal(t, sessionCache, rec.Header().Get("Cache-Control"))
	assert.Empty(t, rec.Header().Get("ETag"))
	assert.Contains(t, rec.Header().Values("Vary"), "Cookie")
	assert.Contains(t, rec.Header().Values("Vary"), "Accept-Encoding")
}

// The assets: gzip once at load, chosen by Accept-Encoding, not for fonts.
func TestAssetsGzip(t *testing.T) {
	t.Parallel()
	srv := newBareServer(t)
	get := func(url, ae string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(context.Background(), "GET", url, nil)
		if ae != "" {
			r.Header.Set("Accept-Encoding", ae)
		}
		srv.ServeHTTP(rec, r)
		return rec
	}
	u := srv.assets.urls
	for _, tc := range []struct {
		name, url, ae string
		wantGzip      bool
		vary          bool
	}{
		{"css gzip", u.CSS, "gzip, br", true, true},
		{"css identity", u.CSS, "", false, true},
		{"css refused", u.CSS, "gzip;q=0", false, true},
		{"js gzip", u.HTMX, "gzip", true, true},
		{"rules gzip", u.Rules, "gzip", true, true},
		{"svg gzip", u.Icon, "gzip", true, true},
		{"font never", u.Font, "gzip", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := get(tc.url, tc.ae)
			require.Equal(t, 200, rec.Code)
			assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
			assert.Equal(t, tc.vary, strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Accept-Encoding"))
			assert.Equal(t, itoa(rec.Body.Len()), rec.Header().Get("Content-Length"))
			plain := srv.assets.byName[strings.TrimPrefix(tc.url, StaticPrefix)].body
			if tc.wantGzip {
				assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
				assert.Less(t, rec.Body.Len(), len(plain))
				assert.Equal(t, string(plain), gunzip(t, rec.Body.Bytes()))
			} else {
				assert.Empty(t, rec.Header().Get("Content-Encoding"))
				assert.Equal(t, string(plain), rec.Body.String())
			}
		})
	}

	t.Run("HEAD has the length of the encoded body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(context.Background(), "HEAD", u.CSS, nil)
		r.Header.Set("Accept-Encoding", "gzip")
		srv.ServeHTTP(rec, r)
		assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
		assert.Equal(t, itoa(len(srv.assets.byName[strings.TrimPrefix(u.CSS, StaticPrefix)].gz)), rec.Header().Get("Content-Length"))
		assert.Zero(t, rec.Body.Len())
	})

	t.Run("compressible types", func(t *testing.T) {
		for typ, want := range map[string]bool{
			"text/css; charset=utf-8": true, "text/javascript; charset=utf-8": true,
			"application/speculationrules+json": true, "image/svg+xml": true,
			"font/woff2": false, "image/png": false,
		} {
			assert.Equal(t, want, compressible(typ), typ)
		}
	})
}

// The event stream is never compressed, even for a client that accepts gzip.
func TestEventsNotCompressed(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{})}
	ts := eventsServer(t, f, 20*time.Millisecond, 0)
	resp, _ := open(t, ts, "topic=home", map[string]string{"Accept-Encoding": "gzip"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Empty(t, resp.Header.Get("Content-Encoding"))
	assert.Empty(t, resp.Header.Get("ETag"))
	assert.Contains(t, resp.Header.Get("Cache-Control"), "no-transform")
}
