// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"
)

// ServiceWorkerPath is where the service worker of the installed app is
// served. Its default scope would be "/-/"; Service-Worker-Allowed widens it to
// the whole site. It lives under /-/ (a name no repository can take) rather
// than at /sw.js, which a repository could be named.
const ServiceWorkerPath = "/-/sw.js"

// serviceWorker is static/sw.js with its entity tag.
type serviceWorker struct {
	body []byte
	etag string
}

func loadServiceWorker() (serviceWorker, error) {
	b, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		return serviceWorker{}, err
	}
	sum := sha256.Sum256(b)
	return serviceWorker{body: b, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}, nil
}

// serve answers GET /-/sw.js. The script has no hash in its name (the browser
// needs one address to look for a new version), so it is revalidated on every
// check: no-cache plus an entity tag, which makes the check a 304. An answer
// the session marked no-store (it clears a stale cookie) stays no-store.
func (sw serviceWorker) serve(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	if h.Get("Cache-Control") != noStore {
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("Service-Worker-Allowed", "/")
	h.Set("ETag", sw.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(sw.body))
}
