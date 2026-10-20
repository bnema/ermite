// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"path"
	"strings"
)

var generatedSuffixes = []string{
	"_templ.go", ".pb.go", "_gen.go", ".gen.go",
	".min.js", ".min.css", ".js.map", ".css.map",
}

var generatedNames = map[string]struct{}{
	"go.sum": {}, "package-lock.json": {}, "pnpm-lock.yaml": {}, "yarn.lock": {},
	"Cargo.lock": {}, "bun.lockb": {}, "bun.lock": {}, "composer.lock": {},
	"Gemfile.lock": {}, "poetry.lock": {}, "uv.lock": {}, "flake.lock": {},
}

// IsGeneratedPath reports whether a changed file is generated or vendored by
// its path alone: its diff is collapsed by default in the web view.
func IsGeneratedPath(p string) bool {
	if p == "" {
		return false
	}
	base := path.Base(p)
	if _, ok := generatedNames[base]; ok {
		return true
	}
	for _, s := range generatedSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	// Directory segments only: the last segment is the file name.
	dir := p[:len(p)-len(base)]
	for dir != "" {
		seg, rest, _ := strings.Cut(dir, "/")
		if seg == "vendor" || seg == "node_modules" {
			return true
		}
		dir = rest
	}
	return false
}
