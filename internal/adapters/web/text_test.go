// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Invisible characters in names and subjects show as markers.
func TestVisibleText(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "a<U+202E>b.go", visibleText("a\u202eb.go"))
	assert.Equal(t, "plain π\t“é”", visibleText("plain π\t“é”"))
	assert.Equal(t, "a<U+0000>b<U+200B>", visibleText("a\x00b\u200b"))
}
