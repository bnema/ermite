// SPDX-License-Identifier: AGPL-3.0-or-later

package depsregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"golang.org/x/mod/semver"
)

// npmDocBytes bounds a package document; the largest public ones (renovate,
// prisma, vite) are tens of MiB.
const npmDocBytes = 128 << 20

// npmVersionMeta keeps only the deprecation of a version manifest. Deprecated
// is raw: the registry holds strings, but older documents have booleans.
type npmVersionMeta struct {
	Deprecated json.RawMessage `json:"deprecated"`
}

// deprecated treats any value except absent, null, false and "" as set.
func (m npmVersionMeta) deprecated() bool {
	switch strings.TrimSpace(string(m.Deprecated)) {
	case "", "null", "false", `""`:
		return false
	}
	return true
}

// NPMVersions reads the full package document, including publication times.
// The document is decoded as a stream that keeps only versions.*.deprecated
// and time, so memory stays small even for documents near the byte cap.
func (r *Registry) NPMVersions(ctx context.Context, name, current string) ([]domain.DepVersion, error) {
	if name == "" {
		return nil, domain.ErrInvalid
	}
	// Keep the scope marker readable while escaping the slash as one path segment.
	escaped := strings.ReplaceAll(url.PathEscape(name), "%40", "@")
	escaped = strings.ReplaceAll(escaped, "%2F", "%2f")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.npm+"/"+escaped, nil)
	if err != nil {
		return nil, err
	}
	var versions map[string]npmVersionMeta
	var times map[string]json.RawMessage
	err = r.stream(req, true, npmDocBytes, 60*time.Second, func(body io.Reader) error {
		var err error
		versions, times, err = decodeNPMDoc(body)
		return err
	})
	if err != nil {
		return nil, err
	}
	var out []domain.DepVersion
	for v, meta := range versions {
		version := "v" + v
		if meta.deprecated() || !semver.IsValid(version) || len(strings.Split(strings.SplitN(strings.SplitN(v, "-", 2)[0], "+", 2)[0], ".")) != 3 || semver.Compare(version, current) <= 0 {
			continue
		}
		// Without a timestamp cooldown cannot be enforced: skip this version.
		var stamp string
		if json.Unmarshal(times[v], &stamp) != nil {
			continue
		}
		published, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		out = append(out, domain.DepVersion{Version: version, Published: published})
	}
	sort.Slice(out, func(i, j int) bool { return semver.Compare(out[i].Version, out[j].Version) > 0 })
	return out, nil
}

// decodeNPMDoc reads the top-level object, decoding "versions" entry by entry
// and "time" as raw values (an unpublished package has an object there), and
// skipping every other member.
func decodeNPMDoc(body io.Reader) (map[string]npmVersionMeta, map[string]json.RawMessage, error) {
	d := json.NewDecoder(body)
	bad := func(err error) error { return fmt.Errorf("npm response: %w", err) }
	if err := expectDelim(d, '{'); err != nil {
		return nil, nil, bad(err)
	}
	versions := map[string]npmVersionMeta{}
	var times map[string]json.RawMessage
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, nil, bad(err)
		}
		switch key {
		case "versions":
			if err := decodeNPMVersions(d, versions); err != nil {
				return nil, nil, bad(err)
			}
		case "time":
			var raw json.RawMessage
			if err := d.Decode(&raw); err != nil {
				return nil, nil, bad(err)
			}
			// Not an object (absent in odd mirrors): no publication times.
			_ = json.Unmarshal(raw, &times)
		default:
			if err := skipValue(d); err != nil {
				return nil, nil, bad(err)
			}
		}
	}
	return versions, times, nil
}

func decodeNPMVersions(d *json.Decoder, out map[string]npmVersionMeta) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		if delim, ok := tok.(json.Delim); ok {
			return skipRest(d, delim)
		}
		return nil // null or a scalar: no versions
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		var meta npmVersionMeta
		if err := d.Decode(&meta); err != nil {
			// A version that is not an object carries no deprecation.
			meta = npmVersionMeta{}
		}
		out[key.(string)] = meta
	}
	_, err = d.Token()
	return err
}

func expectDelim(d *json.Decoder, want json.Delim) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	if tok != want {
		return fmt.Errorf("expected %q", want)
	}
	return nil
}

// skipValue discards the next value without building it.
func skipValue(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); ok {
		return skipRest(d, delim)
	}
	return nil
}

// skipRest discards the remainder of a value opened by delim.
func skipRest(d *json.Decoder, delim json.Delim) error {
	if delim != '{' && delim != '[' {
		return nil
	}
	for depth := 1; depth > 0; {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}
