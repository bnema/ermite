// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

//go:embed static
var staticFS embed.FS

// StaticPrefix is where the embedded assets are served.
const StaticPrefix = "/-/static/"

// Assets are the URLs of the assets a page loads. Their names carry a hash of
// the content, so they can be cached for ever.
type Assets struct {
	CSS, HTMX, Live, SSE, App string
}

// asset is one embedded file under its content-hashed name.
type asset struct {
	body []byte
	typ  string
}

// assetSet is the embedded files by hashed URL path (under StaticPrefix).
type assetSet struct {
	byName map[string]asset
	urls   Assets
}

// scripts are the JavaScript files of the page, in embedded static/js: the
// file to serve, as it is named in Assets.
var scripts = []struct {
	file string
	set  func(*Assets, string)
}{
	{"htmx.min.js", func(a *Assets, u string) { a.HTMX = u }},
	{"hx-live.min.js", func(a *Assets, u string) { a.Live = u }},
	{"hx-sse.min.js", func(a *Assets, u string) { a.SSE = u }},
	{"ermite.js", func(a *Assets, u string) { a.App = u }},
}

// loadAssets hashes the embedded files. The stylesheet is the concatenation
// of static/css/*.css in name order, as in the mockup.
func loadAssets() (*assetSet, error) {
	s := &assetSet{byName: map[string]asset{}}
	put := func(dir, file string, body []byte, typ string) string {
		sum := sha256.Sum256(body)
		ext := path.Ext(file)
		name := dir + "/" + strings.TrimSuffix(file, ext) + "." + hex.EncodeToString(sum[:6]) + ext
		s.byName[name] = asset{body: body, typ: typ}
		return StaticPrefix + name
	}

	files, err := fs.Glob(staticFS, "static/css/*.css")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var css strings.Builder
	for _, f := range files {
		b, err := staticFS.ReadFile(f)
		if err != nil {
			return nil, err
		}
		css.WriteString("/* " + path.Base(f) + " */\n")
		css.Write(b)
	}
	s.urls.CSS = put("css", "ermite.css", []byte(css.String()), "text/css; charset=utf-8")

	for _, sc := range scripts {
		b, err := staticFS.ReadFile("static/js/" + sc.file)
		if err != nil {
			return nil, err
		}
		sc.set(&s.urls, put("js", sc.file, b, "text/javascript; charset=utf-8"))
	}
	return s, nil
}

// serve answers GET /-/static/<hashed name>. A name that does not match a
// current asset is a 404, so a stale hash never serves the wrong content.
func (s *assetSet) serve(w http.ResponseWriter, r *http.Request) {
	a, ok := s.byName[strings.TrimPrefix(r.URL.Path, StaticPrefix)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.typ)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Content-Length", itoa(len(a.body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(a.body)
	}
}
