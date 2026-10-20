// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "testing"

func TestIsGeneratedPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{"views/a_templ.go", true},
		{"api/x.pb.go", true},
		{"x_gen.go", true},
		{"x.gen.go", true},
		{"static/app.min.js", true},
		{"static/app.min.css", true},
		{"static/app.js.map", true},
		{"static/app.css.map", true},
		{"go.sum", true},
		{"web/package-lock.json", true},
		{"pnpm-lock.yaml", true},
		{"yarn.lock", true},
		{"Cargo.lock", true},
		{"bun.lockb", true},
		{"bun.lock", true},
		{"composer.lock", true},
		{"Gemfile.lock", true},
		{"poetry.lock", true},
		{"uv.lock", true},
		{"flake.lock", true},
		{"vendor/x.go", true},
		{"a/vendor/b/x.go", true},
		{"node_modules/a.js", true},
		{"web/node_modules/a.js", true},
		{"vendor.go", false},
		{"templ.go", false},
		{"main.go", false},
		{"go.mod", false},
		{"minify.js", false},
		{"myvendor/x.go", false},
		{"x.min.jsx", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsGeneratedPath(tt.path); got != tt.want {
			t.Errorf("IsGeneratedPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
