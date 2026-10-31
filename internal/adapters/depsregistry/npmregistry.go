// SPDX-License-Identifier: AGPL-3.0-or-later

package depsregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"golang.org/x/mod/semver"
)

// NPMVersions reads the full package document, including publication times.
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
	data, err := r.request(req, true, 32<<20, 60*time.Second)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Versions map[string]struct {
			Deprecated string `json:"deprecated"`
		} `json:"versions"`
		Time map[string]string `json:"time"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("npm response: %w", err)
	}
	var out []domain.DepVersion
	for v, meta := range doc.Versions {
		version := "v" + v
		if meta.Deprecated != "" || !semver.IsValid(version) || len(strings.Split(strings.SplitN(strings.SplitN(v, "-", 2)[0], "+", 2)[0], ".")) != 3 || semver.Compare(version, current) <= 0 {
			continue
		}
		// Without a timestamp cooldown cannot be enforced: skip this version.
		published, err := time.Parse(time.RFC3339Nano, doc.Time[v])
		if err != nil {
			continue
		}
		out = append(out, domain.DepVersion{Version: version, Published: published})
	}
	sort.Slice(out, func(i, j int) bool { return semver.Compare(out[i].Version, out[j].Version) > 0 })
	return out, nil
}
