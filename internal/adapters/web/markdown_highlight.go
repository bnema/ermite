// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	goldhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

const (
	markdownBlockBytes    = 64 << 10
	markdownDocumentBytes = 256 << 10
	markdownLexTime       = 200 * time.Millisecond
	markdownBudgetKey     = "markdown-lex-budget"
)

// Budget belongs to the parsed document, never to the shared renderer.
// Charge attempts, including failures, so repeated bad blocks cannot bypass it.
type markdownLexBudget struct {
	bytes, blocks int
	deadline      time.Time
}

type fencedRenderer struct{}

// Capture goldmark's default renderer rather than duplicate its escaping rules.
type fencedFallback struct{ render renderer.NodeRendererFunc }

func (f *fencedFallback) Register(k ast.NodeKind, fn renderer.NodeRendererFunc) {
	if k == ast.KindFencedCodeBlock {
		f.render = fn
	}
}

func (fencedRenderer) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	var fallback fencedFallback
	goldhtml.NewRenderer().RegisterFuncs(&fallback)
	r.Register(ast.KindFencedCodeBlock, func(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
		n := node.(*ast.FencedCodeBlock)
		if !entering {
			return ast.WalkContinue, nil
		}
		toks, lang, ok := markdownTokens(n, source)
		if !ok {
			// Default rendering is plain escaped text. Omit unsafe language classes.
			if !safeMarkdownLanguage(string(n.Language(source))) {
				plain := ast.NewFencedCodeBlock(nil)
				plain.SetLines(n.Lines())
				n = plain
			}
			_, err := fallback.render(w, source, n, true)
			if err != nil {
				return ast.WalkStop, err
			}
			return fallback.render(w, source, n, false)
		}
		_, _ = w.WriteString(`<pre class="chroma"><code class="language-` + lang + `">`)
		// EnsureNL lexers may append a newline: never emit bytes not in the block.
		remaining := 0
		for i := 0; i < n.Lines().Len(); i++ {
			line := n.Lines().At(i)
			remaining += len(line.Value(source))
		}
		for _, tk := range toks {
			v := tk.Value
			if len(v) > remaining {
				v = v[:remaining]
			}
			remaining -= len(v)
			if v == "" {
				continue
			}
			c := tokenClass(tk.Type)
			if c != "" {
				_, _ = w.WriteString(`<span class="` + c + `">`)
			}
			writeMarked(w, v)
			if c != "" {
				_, _ = w.WriteString(`</span>`)
			}
		}
		_, _ = w.WriteString("</code></pre>\n")
		return ast.WalkSkipChildren, nil
	})
}

func safeMarkdownLanguage(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("_+.#-", c):
		default:
			return false
		}
	}
	return true
}

func markdownTokens(n *ast.FencedCodeBlock, source []byte) ([]chroma.Token, string, bool) {
	var lang string
	if n.Info != nil {
		if words := strings.Fields(string(n.Info.Segment.Value(source))); len(words) > 0 {
			lang = words[0]
		}
	}
	if !safeMarkdownLanguage(lang) {
		return nil, "", false
	}
	value, _ := n.OwnerDocument().AttributeString(markdownBudgetKey)
	budget, ok := value.(*markdownLexBudget)
	if !ok || budget.blocks >= 128 || time.Now().After(budget.deadline) {
		return nil, "", false
	}
	size := 0
	for i := 0; i < n.Lines().Len(); i++ {
		line := n.Lines().At(i)
		size += len(line.Value(source))
		if size > markdownBlockBytes {
			return nil, "", false
		}
	}
	if size > markdownDocumentBytes-budget.bytes {
		return nil, "", false
	}
	budget.blocks++
	budget.bytes += size
	lx := lexers.Get(lang)
	if lx == nil {
		return nil, "", false
	}
	select {
	case lexSlots <- struct{}{}:
		defer func() { <-lexSlots }()
	default:
		return nil, "", false
	}
	var code strings.Builder
	code.Grow(size)
	for i := 0; i < n.Lines().Len(); i++ {
		line := n.Lines().At(i)
		code.Write(line.Value(source))
	}
	ctx, cancel := context.WithDeadline(context.Background(), budget.deadline)
	defer cancel()
	toks, ok := tokenise(ctx, chroma.Coalesce(lx), code.String())
	return toks, lang, ok
}
