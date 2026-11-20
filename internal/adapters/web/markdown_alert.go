// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// alertMarker matches the first line of a GitHub alert: "[!NOTE]", "[!TIP]",
// "[!IMPORTANT]", "[!WARNING]" or "[!CAUTION]", alone on its line, any case.
var alertMarker = regexp.MustCompile(`(?i)^\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\][ \t]*$`)

var kindAlert = ast.NewNodeKind("Alert")

// alertNode is a GitHub alert: a blockquote of the document whose first line is
// a marker. Its children are the blocks of the quote, without the marker.
type alertNode struct {
	ast.BaseBlock
	kind string // note, tip, important, warning or caution
}

func (n *alertNode) Kind() ast.NodeKind { return kindAlert }

func (n *alertNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// alerts replaces the top-level blockquotes that start with an alert marker
// by alert nodes. Like GitHub, a quote nested in a list or another quote stays
// a quote, and so does a quote whose marker line carries other text.
func alerts(doc ast.Node, source []byte) {
	for n := doc.FirstChild(); n != nil; {
		next := n.NextSibling()
		if q, ok := n.(*ast.Blockquote); ok {
			if kind, ok := alertKind(q, source); ok {
				a := &alertNode{kind: kind}
				for c := q.FirstChild(); c != nil; c = q.FirstChild() {
					a.AppendChild(a, c)
				}
				doc.ReplaceChild(doc, q, a)
			}
		}
		n = next
	}
}

// alertKind reads the marker of a quote and removes its line from the first
// paragraph (the whole paragraph when the marker is all it holds).
func alertKind(q *ast.Blockquote, source []byte) (string, bool) {
	p, ok := q.FirstChild().(*ast.Paragraph)
	if !ok || p.Lines().Len() == 0 {
		return "", false
	}
	first := p.Lines().At(0)
	m := alertMarker.FindSubmatch(bytes.TrimRight(first.Value(source), "\r\n"))
	if m == nil {
		return "", false
	}
	kind := strings.ToLower(string(m[1]))
	if _, ok := alertTitles[kind]; !ok {
		return "", false
	}
	if p.Lines().Len() == 1 {
		// Like GitHub, a marker with nothing after it stays a quote.
		if p.NextSibling() == nil {
			return "", false
		}
		q.RemoveChild(q, p)
		return kind, true
	}
	// The inlines of the marker line end at the first soft or hard line break.
	for c := p.FirstChild(); c != nil; c = p.FirstChild() {
		t, isText := c.(*ast.Text)
		p.RemoveChild(p, c)
		if isText && (t.SoftLineBreak() || t.HardLineBreak()) {
			break
		}
	}
	lines := p.Lines()
	lines.SetSliced(1, lines.Len())
	return kind, true
}

// alertTitles are the visible titles of the alerts, by kind. A kind matched by
// alertMarker but missing here is not an alert.
var alertTitles = map[string]string{
	"note":      "Note",
	"tip":       "Tip",
	"important": "Important",
	"warning":   "Warning",
	"caution":   "Caution",
}

type alertRenderer struct{}

func (alertRenderer) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	r.Register(kindAlert, func(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		kind := n.(*alertNode).kind
		if !entering {
			_, _ = w.WriteString("</div>\n")
			return ast.WalkContinue, nil
		}
		// kind comes from the closed list of alertMarker, never from the text.
		_, _ = w.WriteString(`<div class="md-alert md-alert-` + kind + `" role="note">` +
			`<p class="md-alert-title"><span class="ico i-alert-` + kind + `" aria-hidden="true"></span>` +
			alertTitles[kind] + "</p>\n")
		return ast.WalkContinue, nil
	})
}
