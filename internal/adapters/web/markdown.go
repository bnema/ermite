// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"html/template"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// headingIDPrefix keeps the ids of README headings apart from the ids of the
// page (main, keys, cmd-list…): a heading must not be able to take one.
const headingIDPrefix = "md-"

// markdown renders the Markdown of a repository. Its output is safe to put in
// a page: raw HTML is dropped, and links and images follow the policy below.
//
// Link policy:
//   - http and https links are kept, with rel="nofollow noopener noreferrer";
//   - relative links and links to the root of the repository are rewritten to
//     the blob route of the ref being viewed, never above the repository root;
//   - #fragments point at the (prefixed) heading ids;
//   - everything else (javascript:, data:, vbscript:, file:, mailto:,
//     protocol-relative //host, empty targets…) loses its link: the text stays;
//   - images are never loaded (the CSP forbids other origins, and the design
//     shows an image as its alt text): each becomes <span class="alt">[alt]</span>.
type markdown struct {
	md goldmark.Markdown
}

func newMarkdown() *markdown {
	return &markdown{md: goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(
			renderer.WithNodeRenderers(util.Prioritized(altRenderer{}, 100)),
			// No html.WithUnsafe(): raw HTML blocks and inline tags are dropped.
			// Table alignment as an align attribute: the CSP forbids style="".
			extension.WithTableCellAlignMethod(extension.TableCellAlignAttribute),
		),
	)}
}

// render turns source into HTML and the outline of its headings. repo and ref
// are where relative links point; dir is the directory of the Markdown file
// in the repository, "" for the root.
func (m *markdown) render(source []byte, repo, ref, dir string) (template.HTML, []Heading, error) {
	return m.renderWithIDs(source, repo, ref, dir, headingIDPrefix)
}

// renderWithIDs is render with the prefix of the heading ids and of the
// #fragment links chosen, so that several documents on one page (the summaries
// of a run) do not share ids.
func (m *markdown) renderWithIDs(source []byte, repo, ref, dir, idPrefix string) (template.HTML, []Heading, error) {
	return m.renderPolicy(source, linkPolicy{repo: repo, ref: ref, dir: dir, idPrefix: idPrefix}, refResolver{})
}

// renderNote uses the repository Markdown policy, then adds local note links
// and links to issues and merge requests.
func (m *markdown) renderNote(source []byte, repo, ref, idPrefix string, refs refResolver) (template.HTML, error) {
	h, _, err := m.renderPolicy(source, linkPolicy{repo: repo, ref: ref, idPrefix: idPrefix}, refs)
	return h, err
}

// renderIssue renders the trusted body of an issue: the policy of render, plus
// links to issues and merge requests.
func (m *markdown) renderIssue(source []byte, repo, ref, idPrefix string, numbers func(int64) (string, domain.NumberKind, bool)) (template.HTML, error) {
	h, _, err := m.renderPolicy(source, linkPolicy{repo: repo, ref: ref, idPrefix: idPrefix}, refResolver{numbers: numbers})
	return h, err
}

// refResolver tells which references of a text are links.
type refResolver struct {
	// notes tells whether ^N names a note of the page (nil: no ^N links).
	notes func(int64) bool
	// numbers gives the page of the issue or merge request #N, when it exists
	// (nil: no #N links). The href is built by the server, never from the text.
	numbers func(int64) (href string, kind domain.NumberKind, ok bool)
}

// renderUntrusted renders Markdown written by an anonymous visitor (an issue
// approved from the public form). On top of the policy of render, which already
// drops raw HTML and never loads an image:
//   - only http, https and mailto links are kept, every one with
//     rel="nofollow noopener noreferrer ugc"; relative links, #fragments and
//     every other scheme lose their link and keep the text;
//   - nothing points into the repository.
func (m *markdown) renderUntrusted(source []byte, idPrefix string, numbers func(int64) (string, domain.NumberKind, bool)) (template.HTML, error) {
	h, _, err := m.renderPolicy(source, linkPolicy{idPrefix: idPrefix, untrusted: true}, refResolver{numbers: numbers})
	return h, err
}

func (m *markdown) renderPolicy(source []byte, pol linkPolicy, res refResolver) (template.HTML, []Heading, error) {
	pc := parser.NewContext(parser.WithIDs(&prefixedIDs{prefix: pol.idPrefix, inner: parser.NewContext().IDs()}))
	doc := m.md.Parser().Parse(text.NewReader(source), parser.WithContext(pc))
	pol.source = source
	// Collect eligible text before the policy unwraps rejected links: their
	// labels must not accidentally become references either.
	var refs []*ast.Text
	if res.notes != nil || res.numbers != nil {
		_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
			if !entering {
				return ast.WalkContinue, nil
			}
			switch n := n.(type) {
			case *ast.CodeSpan, *ast.Link, *ast.AutoLink, *ast.Image, *ast.CodeBlock, *ast.FencedCodeBlock:
				return ast.WalkSkipChildren, nil
			case *ast.Text:
				refs = append(refs, n)
			}
			return ast.WalkContinue, nil
		})
	}
	pol.apply(doc)
	for _, t := range refs {
		references(t, source, res)
	}

	var buf bytes.Buffer
	if err := m.md.Renderer().Render(&buf, source, doc); err != nil {
		return "", nil, err
	}
	// #nosec G203 -- goldmark without WithUnsafe escapes text and drops raw
	// HTML; linkPolicy has removed every URL it does not allow.
	return template.HTML(buf.String()), outline(doc, source), nil
}

// prefixedIDs prefixes the heading ids that goldmark generates.
type prefixedIDs struct {
	prefix string
	inner  parser.IDs
}

func (p *prefixedIDs) Generate(value []byte, kind ast.NodeKind) []byte {
	return append([]byte(p.prefix), p.inner.Generate(value, kind)...)
}

func (p *prefixedIDs) Put(value []byte) { p.inner.Put(value) }

// outline lists the headings of a document in order.
func outline(doc ast.Node, source []byte) []Heading {
	var out []Heading
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		id, _ := h.AttributeString("id")
		idb, _ := id.([]byte)
		out = append(out, Heading{Level: h.Level, ID: string(idb), Text: plainText(h, source)})
		return ast.WalkSkipChildren, nil
	})
	return out
}

// plainText is the text of a node without markup.
func plainText(n ast.Node, source []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Value(source))
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(t.Value)
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

// linkPolicy applies the link and image policy to a parsed document.
type linkPolicy struct {
	repo, ref, dir string
	source         []byte
	// idPrefix is the prefix of the heading ids that #fragment links point at.
	idPrefix string
	// untrusted is the policy of text from an anonymous visitor.
	untrusted bool
}

const (
	relExternal  = "nofollow noopener noreferrer"
	relUntrusted = "nofollow noopener noreferrer ugc"
)

func (p linkPolicy) rel() []byte {
	if p.untrusted {
		return []byte(relUntrusted)
	}
	return []byte(relExternal)
}

func (p linkPolicy) apply(doc ast.Node) {
	// Collect first, change after: the walk must not see the tree move.
	var links, images, autos []ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.(type) {
		case *ast.Link:
			links = append(links, n)
		case *ast.Image:
			images = append(images, n)
		case *ast.AutoLink:
			autos = append(autos, n)
		}
		return ast.WalkContinue, nil
	})
	for _, n := range images {
		p.image(n.(*ast.Image))
	}
	for _, n := range links {
		l := n.(*ast.Link)
		dest, kind := p.target(string(l.Destination))
		if kind == dropped {
			unwrap(l)
			continue
		}
		l.Destination = []byte(dest)
		l.Title = nil
		if kind == external {
			l.SetAttributeString("rel", p.rel())
		}
	}
	for _, n := range autos {
		a := n.(*ast.AutoLink)
		if a.AutoLinkType == ast.AutoLinkURL && isWebURL(string(a.URL(p.source))) {
			a.SetAttributeString("rel", p.rel())
			continue
		}
		if p.untrusted && a.AutoLinkType == ast.AutoLinkEmail && validMailbox(string(a.Label(p.source))) {
			a.SetAttributeString("rel", p.rel())
			continue
		}
		// An email address or another scheme: the label as plain text.
		a.Parent().ReplaceChild(a.Parent(), a, ast.NewString(append([]byte(nil), a.Label(p.source)...)))
	}
}

// unwrap replaces a node by its children.
func unwrap(n ast.Node) {
	parent := n.Parent()
	for c := n.FirstChild(); c != nil; c = n.FirstChild() {
		parent.InsertBefore(parent, n, c)
	}
	parent.RemoveChild(parent, n)
}

// image replaces an image by its alt text.
func (p linkPolicy) image(img *ast.Image) {
	alt := plainText(img, p.source)
	if alt == "" {
		alt = "image"
	}
	img.Parent().ReplaceChild(img.Parent(), img, &altSpan{alt: alt})
}

type linkKind int

const (
	dropped linkKind = iota
	external
	internal
)

// target applies the link policy to a destination.
func (p linkPolicy) target(dest string) (string, linkKind) {
	dest = strings.TrimSpace(dest)
	if dest == "" || strings.ContainsAny(dest, "\x00\r\n\t\\") {
		return "", dropped
	}
	if p.untrusted {
		return untrustedTarget(dest)
	}
	if strings.HasPrefix(dest, "#") {
		frag := strings.ToLower(strings.TrimPrefix(dest, "#"))
		if frag == "" || strings.ContainsAny(frag, " \"'<>") {
			return "", dropped
		}
		return "#" + p.idPrefix + frag, internal
	}
	if strings.HasPrefix(dest, "//") {
		return "", dropped
	}
	u, err := url.Parse(dest)
	if err != nil {
		return "", dropped
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return "", dropped
		}
		return u.String(), external
	case "":
	default:
		return "", dropped
	}
	if u.Host != "" || u.Opaque != "" || u.Path == "" {
		return "", dropped
	}
	// A path in the repository: from the directory of the file, or from the
	// root when it starts with a slash. path.Clean on a rooted path never
	// leaves the root, so ../../ cannot climb out of the repository.
	var rel string
	if strings.HasPrefix(u.Path, "/") {
		rel = path.Clean(u.Path)
	} else {
		rel = path.Clean("/" + path.Join(p.dir, u.Path))
	}
	rel = strings.TrimPrefix(rel, "/")
	href := repoHref(p.repo)
	if rel != "" {
		href = repoHref(p.repo, "blob", p.ref, rel)
	}
	if u.Fragment != "" {
		href += "#" + url.PathEscape(u.Fragment)
	}
	return href, internal
}

// untrustedTarget keeps absolute http and https links with a host, and mailto
// links to one plain address.
func untrustedTarget(dest string) (string, linkKind) {
	if strings.HasPrefix(dest, "//") {
		return "", dropped
	}
	u, err := url.Parse(dest)
	if err != nil {
		return "", dropped
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" || u.User != nil {
			return "", dropped
		}
		return u.String(), external
	case "mailto":
		if u.Opaque == "" || u.RawQuery != "" || !validMailbox(u.Opaque) {
			return "", dropped
		}
		return "mailto:" + u.Opaque, external
	}
	return "", dropped
}

// validMailbox tells whether s is a bare address: no space, quote, angle
// bracket, percent sign, comma or second at sign.
func validMailbox(s string) bool {
	local, host, ok := strings.Cut(s, "@")
	return ok && local != "" && host != "" && len(s) <= 254 &&
		!strings.ContainsAny(local+host, " \t\r\n\"'<>%,;?#\\@")
}

func isWebURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	sc := strings.ToLower(u.Scheme)
	return sc == "http" || sc == "https"
}

// kindAlt is the kind of altSpan.
var kindAlt = ast.NewNodeKind("AltSpan")

// altSpan stands for an image: its alt text, in a span the stylesheet styles.
type altSpan struct {
	ast.BaseInline
	alt string
}

func (n *altSpan) Kind() ast.NodeKind { return kindAlt }

func (n *altSpan) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// altRenderer renders altSpan nodes.
type altRenderer struct{}

func (altRenderer) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	r.Register(kindAlt, func(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			_, _ = w.WriteString(`<span class="alt">[`)
			_, _ = w.Write(util.EscapeHTML([]byte(n.(*altSpan).alt)))
			_, _ = w.WriteString(`]</span>`)
		}
		return ast.WalkContinue, nil
	})
}

// referencePattern matches ^N (groups 1-2) and #N (groups 3-4). The preceding
// character is not part of a word; #N also rejects & and # so that numeric
// entities such as &#39; stay text.
var referencePattern = regexp.MustCompile(`(^|[^0-9A-Za-z_^])\^([1-9][0-9]{0,5})\b|(^|[^0-9A-Za-z_&#])#([1-9][0-9]{0,9})\b`)

// references changes only text segments; code and link labels never enter
// this function. New links deliberately bypass heading-prefix rewriting.
func references(t *ast.Text, source []byte, res refResolver) {
	parent := t.Parent()
	if parent == nil {
		return
	}
	value := t.Value(source)
	offset := 0
	for _, match := range referencePattern.FindAllSubmatchIndex(value, -1) {
		isNote := match[4] >= 0
		var from, to int
		if isNote {
			from, to = match[4], match[5]
		} else {
			from, to = match[8], match[9]
		}
		if (isNote && res.notes == nil) || (!isNote && res.numbers == nil) {
			continue
		}
		id := string(value[from:to])
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			continue
		}
		start := from - 1
		// Markdown punctuation may split adjacent text into separate AST nodes.
		// Check the original preceding byte as well as the segment-local regex.
		if at := t.Segment.Start + start; at > 0 {
			prev := source[at-1]
			if prev >= '0' && prev <= '9' || prev >= 'A' && prev <= 'Z' || prev >= 'a' && prev <= 'z' || prev == '_' {
				continue
			}
			if isNote && prev == '^' || !isNote && (prev == '&' || prev == '#') {
				continue
			}
		}
		link := ast.NewLink()
		if isNote {
			if !res.notes(n) {
				continue
			}
			link.Destination = []byte("#c" + id)
			link.Title = []byte("note " + id)
		} else {
			href, kind, ok := res.numbers(n)
			if !ok {
				continue
			}
			link.Destination = []byte(href)
			title := "issue "
			if kind == domain.NumberMergeRequest {
				title = "merge request "
			}
			link.Title = []byte(title + id)
		}
		if start > offset {
			parent.InsertBefore(parent, t, ast.NewTextSegment(text.NewSegment(t.Segment.Start+offset, t.Segment.Start+start)))
		}
		link.SetAttributeString("class", []byte("ref"))
		link.AppendChild(link, ast.NewString(value[start:to]))
		parent.InsertBefore(parent, t, link)
		offset = to
	}
	if offset == 0 {
		return
	}
	tail := ast.NewTextSegment(text.NewSegment(t.Segment.Start+offset, t.Segment.Stop))
	tail.SetSoftLineBreak(t.SoftLineBreak())
	tail.SetHardLineBreak(t.HardLineBreak())
	parent.InsertBefore(parent, t, tail)
	parent.RemoveChild(parent, t)
}
