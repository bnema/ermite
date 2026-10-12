// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
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
	CSS, HTMX, Live, SSE, App, Files, Log, Icon string
	// Font is the UI font, preloaded by every page.
	Font string
	// Rules is the speculation rules file named by the Speculation-Rules header.
	Rules string
}

// asset is one embedded file under its content-hashed name.
type asset struct {
	body []byte
	typ  string
	// gz is the gzip form of body, compressed once at load; nil when the type
	// is already compressed or gzip does not make the body smaller.
	gz []byte
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
	{"files.js", func(a *Assets, u string) { a.Files = u }},
	{"log.js", func(a *Assets, u string) { a.Log = u }},
}

// sheetAssets are the files the stylesheet references as url(<dir>/<file>):
// fonts and icons (pages hold no inline SVG; icons are CSS masks).
var sheetAssets = []struct{ glob, typ string }{
	{"static/fonts/*.woff2", "font/woff2"},
	{"static/icons/*.svg", "image/svg+xml"},
}

// loadAssets hashes the embedded files. The stylesheet is the concatenation
// of static/css/*.css in name order; its url(<dir>/<file>) references become
// the hashed URLs of the files of sheetAssets.
func loadAssets() (*assetSet, error) {
	s := &assetSet{byName: map[string]asset{}}
	put := func(dir, file string, body []byte, typ string) string {
		sum := sha256.Sum256(body)
		ext := path.Ext(file)
		name := dir + "/" + strings.TrimSuffix(file, ext) + "." + hex.EncodeToString(sum[:6]) + ext
		a := asset{body: body, typ: typ}
		if compressible(typ) {
			if z, err := gzipBest(body); err == nil && len(z) < len(body) {
				a.gz = z
			}
		}
		s.byName[name] = a
		return StaticPrefix + name
	}

	var refs []string
	for _, sa := range sheetAssets {
		files, err := fs.Glob(staticFS, sa.glob)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			b, err := staticFS.ReadFile(f)
			if err != nil {
				return nil, err
			}
			dir := path.Base(path.Dir(f))
			u := put(dir, path.Base(f), b, sa.typ)
			refs = append(refs, "url("+dir+"/"+path.Base(f)+")", "url("+u+")")
			if f == "static/fonts/atkinson-next.woff2" {
				s.urls.Font = u
			}
		}
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
	if s.urls.Font == "" {
		return nil, errors.New("web: the UI font static/fonts/atkinson-next.woff2 is missing")
	}
	sheet := strings.NewReplacer(refs...).Replace(css.String())
	s.urls.CSS = put("css", "ermite.css", []byte(sheet), "text/css; charset=utf-8")

	rules, err := staticFS.ReadFile("static/rules.json")
	if err != nil {
		return nil, err
	}
	s.urls.Rules = put("rules", "rules.json", rules, "application/speculationrules+json")

	for _, sc := range scripts {
		b, err := staticFS.ReadFile("static/js/" + sc.file)
		if err != nil {
			return nil, err
		}
		sc.set(&s.urls, put("js", sc.file, b, "text/javascript; charset=utf-8"))
	}

	// The favicon is the brand picture of the header.
	icon, err := staticFS.ReadFile("static/icons/brand.svg")
	if err != nil {
		return nil, err
	}
	s.urls.Icon = put("img", "icon.svg", icon, "image/svg+xml")
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
	body := a.body
	if a.gz != nil {
		addVary(h, "Accept-Encoding")
		if acceptsGzip(r.Header) {
			body = a.gz
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("Content-Length", itoa(len(body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// compressible tells whether a content type is worth gzip: text and
// structured text. Fonts (woff2) and raster images are compressed already.
func compressible(typ string) bool {
	mt, _, _ := strings.Cut(typ, ";")
	mt = strings.TrimSpace(strings.ToLower(mt))
	return strings.HasPrefix(mt, "text/") || mt == "image/svg+xml" ||
		mt == "application/javascript" || strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "/json")
}
