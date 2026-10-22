// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIssueFieldAccessibility(t *testing.T) {
	for _, field := range []string{"title", "body", "author"} {
		t.Run(field, func(t *testing.T) {
			html := renderToString(t, IssueForm(Layout{}, IssueFormPage{ErrorField: field, Error: "Too long"}))
			assert.Contains(t, html, `id="issue-`+field+`" aria-describedby="issue-`+field+`-count issue-`+field+`-error" aria-invalid="true"`)
			assert.Contains(t, html, `<p id="issue-`+field+`-error" class="f-error" role="alert">Too long</p>`)
			for _, f := range []string{"title", "body", "author"} {
				assert.Contains(t, html, `<output id="issue-`+f+`-count" aria-live="off"`)
			}
		})
	}
}

func TestHintAccessibility(t *testing.T) {
	html := renderToString(t, Hint(Layout{Live: true, LiveURL: "/events"}))
	assert.Contains(t, html, `<div class="hint"><span aria-hidden="true" class="hint-nav"`)
	assert.Contains(t, html, `<span role="status" aria-live="polite"><span class="hint-sel"`)
	assert.Contains(t, html, `</span></span><span class="hint-live" role="status"`)
}

func TestPaletteAccessibilityWithoutNavigationItems(t *testing.T) {
	html := renderToString(t, OverlayCmd(nil))
	// The shortcut preference action is always present, even with no targets.
	assert.Contains(t, html, `aria-expanded="true"`)
	assert.Contains(t, html, `aria-activedescendant="cmd-o0"`)
	assert.Contains(t, html, `id="cmd-o0" role="option" aria-selected="true" data-cursor`)
	assert.Contains(t, html, `<a role="presentation" data-fire="keys">`)
	assert.Contains(t, html, `aria-label="Close (esc)"`)
	assert.NotContains(t, html, ` href=`)
	assert.NotContains(t, html, `tabindex="-1"`)
}

func TestFinderStatusOutsideHiddenResults(t *testing.T) {
	html := renderToString(t, Tree(Layout{}, TreePage{}))
	assert.Contains(t, html, `<p id="finder-status" class="dim" role="status"></p><div id="finder" class="tree-card" hidden>`)
}

func TestPaletteNavigationOptionsAccessibility(t *testing.T) {
	html := renderToString(t, OverlayCmd([]CmdItem{{Kind: "section", Label: "Files", Href: "/files"}}))
	assert.Contains(t, html, `<a role="presentation" data-href="/files">`)
	assert.NotContains(t, html, ` href=`)
	assert.NotContains(t, html, `tabindex="-1"`)
}
