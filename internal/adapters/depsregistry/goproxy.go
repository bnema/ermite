// SPDX-License-Identifier: AGPL-3.0-or-later

package depsregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// GoDirective reads the go directive of the module's go.mod at version. A
// go.mod whose module line is not name is domain.ErrModulePathMismatch.
func (r *Registry) GoDirective(ctx context.Context, name, version string) (string, error) {
	path, err := module.EscapePath(name)
	if err != nil {
		return "", fmt.Errorf("%w: module path: %v", domain.ErrInvalid, err)
	}
	escaped, err := module.EscapeVersion(version)
	if err != nil {
		return "", fmt.Errorf("%w: module version: %v", domain.ErrInvalid, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.proxy+"/"+path+"/@v/"+escaped+".mod", nil)
	if err != nil {
		return "", err
	}
	data, err := r.request(req, true, 1<<20, 15*time.Second)
	if err != nil {
		return "", err
	}
	f, err := modfile.ParseLax("go.mod", data, nil)
	if err != nil {
		return "", fmt.Errorf("go proxy go.mod: %w", err)
	}
	if f.Module == nil || f.Module.Mod.Path != name {
		declared := ""
		if f.Module != nil {
			declared = f.Module.Mod.Path
		}
		return "", fmt.Errorf("%w: module declares its path as %q", domain.ErrModulePathMismatch, declared)
	}
	if f.Go == nil {
		return "", nil
	}
	return f.Go.Version, nil
}

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
		return r.request(req, true, 1<<20, 15*time.Second)
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
	// A listed version the proxy cannot describe (no .info) is skipped, not
	// the module; when none can be described, the module is not found.
	out := make([]domain.DepVersion, 0, len(versions))
	missing := 0
	for _, v := range versions {
		escaped, err := module.EscapeVersion(v)
		if err != nil {
			return nil, err
		}
		data, err := get(path + "/@v/" + escaped + ".info")
		if errors.Is(err, domain.ErrNotFound) {
			missing++
			continue
		}
		if err != nil {
			return nil, err
		}
		var info struct{ Time time.Time }
		if err := json.Unmarshal(data, &info); err != nil {
			return nil, fmt.Errorf("go proxy info: %w", err)
		}
		out = append(out, domain.DepVersion{Version: v, Published: info.Time})
	}
	if missing > 0 && len(out) == 0 {
		return nil, fmt.Errorf("go proxy info of %s: %w", name, domain.ErrNotFound)
	}
	return out, nil
}

// GoMajorPaths probes consecutive path majors, stopping at the first missing path.
func (r *Registry) GoMajorPaths(ctx context.Context, name string) ([]string, error) {
	if err := module.CheckPath(name); err != nil {
		return nil, fmt.Errorf("%w: module path: %v", domain.ErrInvalid, err)
	}
	base, suffix, ok := module.SplitPathVersion(name)
	if !ok {
		return nil, domain.ErrInvalid
	}
	separator := "/v"
	if strings.HasPrefix(name, "gopkg.in/") {
		separator = ".v"
	}
	major := 1
	if suffix != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(suffix, separator))
		if err != nil {
			return nil, domain.ErrInvalid
		}
		major = n
	}
	var paths []string
	for n := major + 1; n <= major+5; n++ {
		candidate := base + separator + strconv.Itoa(n)
		escaped, err := module.EscapePath(candidate)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.proxy+"/"+escaped+"/@v/list", nil)
		if err != nil {
			return nil, err
		}
		data, err := r.request(req, true, 1<<20, 15*time.Second)
		if errors.Is(err, domain.ErrNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(strings.Fields(string(data))) == 0 {
			break
		}
		paths = append(paths, candidate)
	}
	return paths, nil
}
