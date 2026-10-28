// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestLanguageOf(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
		ok   bool
	}{
		{"main.go", "Go", true},
		{"a/b/lib.rs", "Rust", true},
		{"x.h", "C", true},
		{"x.hpp", "C++", true},
		{"x.m", "Objective-C", true},
		{"x.pl", "Perl", true},
		{"x.v", "Verilog", true},
		{"x.tsx", "TypeScript", true},
		{"run.sh", "Shell", true},
		{"run.fish", "Fish", true},
		{"Makefile", "Makefile", true},
		{"sub/GNUmakefile", "Makefile", true},
		{"rules.mk", "Makefile", true},
		{"Dockerfile", "Dockerfile", true},
		{"ci.dockerfile", "Dockerfile", true},
		{"MAIN.GO", "Go", true},
		{"App.JS", "JavaScript", true},
		{"view.templ", "Templ", true},
		{"a.proto", "Protocol Buffer", true},
		{"data.json", "", false},
		{"conf.yaml", "", false},
		{"conf.yml", "", false},
		{"conf.toml", "", false},
		{"README.md", "", false},
		{"notes.txt", "", false},
		{"logo.png", "", false},
		{"noext", "", false},
		{"go.sum", "", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := domain.LanguageOf(tc.path)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLanguageCounted(t *testing.T) {
	for _, tc := range []struct {
		path string
		size int64
		want bool
	}{
		{"main.go", 10, true},
		{"main.go", domain.MaxLanguageBlob, true},
		{"main.go", domain.MaxLanguageBlob + 1, false},
		{"main.go", 0, false},
		{"vendor/x/y.go", 10, false},
		{"web/node_modules/x/y.js", 10, false},
		{"api.pb.go", 10, false},
		{"view_templ.go", 10, false},
		{"app.min.js", 10, false},
		{"third_party/x.c", 10, false},
		{"a/dist/x.js", 10, false},
		{"docs/gen.py", 10, false},
		{"doc/gen.py", 10, false},
		{"Documentation/x.py", 10, false},
		{"src/docs/gen.py", 10, true},
		{".hidden/x.py", 10, false},
		{"a/.cache/x.py", 10, false},
		{".github/scripts/x.py", 10, true},
		{".gitignore", 10, true}, // a file, not a directory: the language table decides
	} {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, domain.LanguageCounted(tc.path, tc.size))
		})
	}
}

func TestCountLanguages(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got := domain.CountLanguages(domain.FileSizes{})
		assert.Nil(t, got.Languages)
		assert.False(t, got.Truncated)
		assert.Nil(t, got.Shares())
	})
	t.Run("nothing counts", func(t *testing.T) {
		got := domain.CountLanguages(domain.FileSizes{Files: []domain.FileSize{{Path: "README.md", Size: 5}, {Path: "a.go", Size: 0}}, Truncated: true})
		assert.Nil(t, got.Languages)
		assert.True(t, got.Truncated)
	})
	t.Run("order and shares", func(t *testing.T) {
		got := domain.CountLanguages(domain.FileSizes{Files: []domain.FileSize{
			{Path: "main.go", Size: 3000},
			{Path: "run.sh", Size: 1000},
			{Path: "b.py", Size: 500},
			{Path: "a.rs", Size: 500},
			{Path: "x/y.go", Size: 7},
			{Path: "vendor/z.go", Size: 99999},
			{Path: "big.go", Size: domain.MaxLanguageBlob + 1},
			{Path: "data.json", Size: 4000},
		}, Truncated: true})
		assert.True(t, got.Truncated)
		assert.Equal(t, []domain.LanguageBytes{
			{Name: "Go", Bytes: 3007}, {Name: "Shell", Bytes: 1000}, {Name: "Python", Bytes: 500}, {Name: "Rust", Bytes: 500},
		}, got.Languages)
		shares := got.Shares()
		require.Len(t, shares, 4)
		var sum float64
		for i, sh := range shares {
			assert.Equal(t, got.Languages[i].Name, sh.Name)
			assert.Equal(t, got.Languages[i].Bytes, sh.Bytes)
			sum += sh.Percent
		}
		assert.InDelta(t, 100, sum, 1e-9)
		assert.InDelta(t, 100*3007.0/5007.0, shares[0].Percent, 1e-9)
		assert.False(t, math.IsNaN(sum))
	})
}
