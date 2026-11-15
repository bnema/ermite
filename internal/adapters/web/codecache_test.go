// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"html/template"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestCodeKey(t *testing.T) {
	t.Parallel()
	id, err := domain.ParseObjectID(strings.Repeat("a", 40))
	require.NoError(t, err)
	assert.Empty(t, codeKey(domain.ObjectID{}, "a.go", "package a\n"))
	assert.Equal(t, "b\x00README\x00"+id.String()+"\x005", codeKey(id, "README", "hello"))
	k := codeKey(id, "a.go", "package a\n")
	assert.True(t, strings.HasPrefix(k, "b\x00a.go\x00"))
	assert.NotEqual(t, k, codeKey(id, "a.go", "package a\nvar x = 1\n"))
	assert.Equal(t, k, codeKey(id, "dir/a.go", "package a\n"))
	assert.NotEqual(t, k, codeKey(id, "b.go", "package a\n"))
}

func TestCodeCacheValidity(t *testing.T) {
	t.Parallel()
	for _, lang := range []string{"", "Go"} {
		t.Run(lang, func(t *testing.T) {
			t.Parallel()
			c := newLRUCache(codeCacheBytes,
				func(v codeView) int { return len(v.html) },
				func(v codeView) bool { return v.lang != "" })
			builds := 0
			build := func() codeView { builds++; return codeView{html: "plain or highlighted", lang: lang} }
			first := c.load(context.Background(), "k", build)
			assert.Equal(t, first, c.load(context.Background(), "k", build))
			if lang == "" {
				assert.Equal(t, 2, builds)
			} else {
				assert.Equal(t, 1, builds)
			}
		})
	}
}

func TestCodeCacheOversized(t *testing.T) {
	t.Parallel()
	c := newLRUCache(1024,
		func(v codeView) int { return len(v.html) },
		func(v codeView) bool { return v.lang != "" })
	builds := 0
	build := func() codeView {
		builds++
		return codeView{html: template.HTML(strings.Repeat("x", 257)), lang: "Go"}
	}
	for range 2 {
		assert.NotEmpty(t, c.load(context.Background(), "big", build).html)
	}
	assert.Equal(t, 2, builds)
	assert.Empty(t, c.byKey)
	assert.Zero(t, c.size)
}

func TestCodeViewOf(t *testing.T) {
	// Do not run in parallel: other highlight tests fill the shared lexing slots.
	id, err := domain.ParseObjectID(strings.Repeat("a", 40))
	require.NoError(t, err)
	s := &Server{code: newLRUCache(codeCacheBytes,
		func(v codeView) int { return len(v.html) },
		func(v codeView) bool { return v.lang != "" })}
	ctx := context.Background()
	first := s.codeViewOf(ctx, id, "a.go", "package a\n")
	require.Equal(t, "Go", first.lang)
	assert.Positive(t, s.code.size)
	// With every lexing slot taken, highlighting again would give plain text:
	// the colours can only come from the cache.
	for range cap(lexSlots) {
		lexSlots <- struct{}{}
	}
	cached := s.codeViewOf(ctx, id, "a.go", "package a\n")
	for range cap(lexSlots) {
		<-lexSlots
	}
	assert.Equal(t, first, cached)
	s.code = nil
	assert.Equal(t, first, s.codeViewOf(ctx, id, "a.go", "package a\n"))
}

func BenchmarkBlobCodeView(b *testing.B) {
	text := "package a\n" + strings.Repeat("func f() int {\n\t// a small function\n\tx := 42\n\treturn x\n}\n", 168)
	ctx := context.Background()
	b.Run("highlightCtx", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			highlightCtx(ctx, "a.go", text)
		}
	})
	b.Run("cachedLoad", func(b *testing.B) {
		c := newLRUCache(codeCacheBytes,
			func(v codeView) int { return len(v.html) },
			func(v codeView) bool { return v.lang != "" })
		build := func() codeView { return highlightCtx(ctx, "a.go", text) }
		c.load(ctx, "blob", build)
		b.ReportAllocs()
		for b.Loop() {
			c.load(ctx, "blob", build)
		}
	})
}
