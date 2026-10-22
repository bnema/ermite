// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSectionShortcutSequences(t *testing.T) {
	sections := []Section{}
	for _, key := range []string{"s", "f", "c", "b", "m", "i", "r", "a"} {
		sections = append(sections, Section{Key: key, Label: key, Href: "/" + key})
	}
	header := renderToString(t, Header(Layout{Sections: sections}))
	bindings := renderToString(t, Keys())
	for _, section := range sections {
		assert.Contains(t, header, `data-key="g `+section.Key+`"`)
		assert.NotContains(t, header, `aria-keyshortcuts="g `+section.Key+`"`)
		assert.Contains(t, header, `<kbd aria-hidden="true">g `+section.Key+`</kbd>`)
		assert.Contains(t, bindings, "ermite.tab('"+section.Key+"')")
	}
	assert.NotContains(t, bindings, "k-gf")
	assert.NotContains(t, bindings, "k-t from:body")
}
