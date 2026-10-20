// SPDX-License-Identifier: AGPL-3.0-or-later

package readertest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// DiffChangedLineLimits checks that context is retained without consuming the
// changed-line limits, but still consumes the byte budget.
func DiffChangedLineLimits(t *testing.T, mk Maker) {
	g, data := mk(t)
	f := NewFixture(t, data, "diff-context-budget")
	var before, after strings.Builder
	const hunks = 96
	for i := range hunks * 10 {
		line := fmt.Sprintf("context %04d", i)
		fmt.Fprintln(&before, line)
		if i%10 == 4 {
			line = fmt.Sprintf("changed %04d", i)
		}
		fmt.Fprintln(&after, line)
	}
	f.Write("many", before.String()).Commit("before")
	f.Write("many", after.String()).Commit("after")
	limits := domain.DiffLimits{MaxFileLines: hunks * 2, MaxTotalLines: hunks * 2, MaxTotalBytes: 1 << 20}
	d, err := g.GetCommit(context.Background(), f.Name, domain.Revision{}, limits)
	require.NoError(t, err)
	require.Len(t, d.Diff.Files, 1)
	file := d.Diff.Files[0]
	require.False(t, file.Truncated)
	assert.False(t, d.Diff.Truncated)
	require.Len(t, file.Hunks, hunks)
	var changed, patchLines, textBytes, changedBytes int
	for _, h := range file.Hunks {
		for _, l := range h.Lines {
			patchLines++
			textBytes += len(l.Text)
			if l.Kind != domain.LineContext {
				changed++
				changedBytes += len(l.Text)
			}
		}
	}
	assert.Equal(t, hunks*2, changed)
	assert.Greater(t, patchLines, limits.MaxFileLines)
	assert.Equal(t, hunks, file.Additions)
	assert.Equal(t, hunks, file.Deletions)
	// The exact byte boundary fits; one fewer byte must discard the file even
	// though every changed line's text alone easily fits.
	limits.MaxTotalBytes = textBytes
	d, err = g.GetCommit(context.Background(), f.Name, domain.Revision{}, limits)
	require.NoError(t, err)
	assert.False(t, d.Diff.Files[0].Truncated)
	limits.MaxTotalBytes--
	require.Greater(t, limits.MaxTotalBytes, changedBytes)
	d, err = g.GetCommit(context.Background(), f.Name, domain.Revision{}, limits)
	require.NoError(t, err)
	assert.True(t, d.Diff.Truncated)
	assert.True(t, d.Diff.Files[0].Truncated)
	assert.Empty(t, d.Diff.Files[0].Hunks)
}
