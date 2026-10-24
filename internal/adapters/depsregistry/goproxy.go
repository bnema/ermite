// SPDX-License-Identifier: AGPL-3.0-or-later

package depsregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// GoVersions fetches at most the 50 newest valid versions above current.
func (r *Registry) GoVersions(ctx context.Context, name, current string) ([]domain.DepVersion, error) {
	path, err := module.EscapePath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: module path: %v", domain.ErrInvalid, err)
	}
	get := func(path string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.proxy+"/"+path, nil)
		if err != nil {
			return nil, err
		}
		return r.request(req, true)
	}
	data, err := get(path + "/@v/list")
	if err != nil {
		return nil, err
	}
	var versions []string
	seen := map[string]bool{}
	for _, v := range strings.Fields(string(data)) {
		if semver.IsValid(v) && semver.Compare(v, current) > 0 && !seen[v] {
			versions = append(versions, v)
			seen[v] = true
		}
	}
	sort.Slice(versions, func(i, j int) bool { return semver.Compare(versions[i], versions[j]) > 0 })
	if len(versions) > 50 {
		versions = versions[:50]
	}
	out := make([]domain.DepVersion, 0, len(versions))
	for _, v := range versions {
		escaped, err := module.EscapeVersion(v)
		if err != nil {
			return nil, err
		}
		data, err := get(path + "/@v/" + escaped + ".info")
		if err != nil {
			return nil, err
		}
		var info struct{ Time time.Time }
		if err := json.Unmarshal(data, &info); err != nil {
			return nil, fmt.Errorf("go proxy info: %w", err)
		}
		out = append(out, domain.DepVersion{Version: v, Published: info.Time})
	}
	return out, nil
}
