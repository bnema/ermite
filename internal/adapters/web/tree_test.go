// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTreeRowStablePath(t *testing.T) {
	var b bytes.Buffer
	require.NoError(t, TreeRowView(TreeRow{Path: "docs/a\"&.md", Name: "a", Kind: kindFile}).Render(context.Background(), &b))
	assert.Contains(t, b.String(), `data-path="docs/a&#34;&amp;.md"`)
	assert.NotContains(t, b.String(), ` style=`)
}
