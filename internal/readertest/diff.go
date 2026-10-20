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
	require.Equal(t, domain.OmitNone, file.Omitted)
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
	// Every hunk is read with no line limit too.
	free, err := g.GetCommit(context.Background(), f.Name, domain.Revision{}, domain.DiffLimits{MaxTotalBytes: 1 << 20})
	require.NoError(t, err)
	require.Len(t, free.Diff.Files[0].Hunks, hunks)
	assert.Greater(t, patchLines, limits.MaxFileLines)
	assert.Equal(t, hunks, file.Additions)
	assert.Equal(t, hunks, file.Deletions)
	// The exact byte boundary fits; one fewer byte must discard the file even
	// though every changed line's text alone easily fits.
	limits.MaxTotalBytes = textBytes
	d, err = g.GetCommit(context.Background(), f.Name, domain.Revision{}, limits)
	require.NoError(t, err)
	assert.Equal(t, domain.OmitNone, d.Diff.Files[0].Omitted)
	limits.MaxTotalBytes--
	require.Greater(t, limits.MaxTotalBytes, changedBytes)
	d, err = g.GetCommit(context.Background(), f.Name, domain.Revision{}, limits)
	require.NoError(t, err)
	assert.False(t, d.Diff.Truncated, "only MaxFiles sets Diff.Truncated")
	assert.Equal(t, domain.OmitUnavailable, d.Diff.Files[0].Omitted)
	assert.Empty(t, d.Diff.Files[0].Hunks)
}

// DiffOmissions checks the omission reasons: generated files skipped without
// reading their hunks, one file read without a line cap, and the MaxFiles
// overflow being the only thing that sets Diff.Truncated.
func DiffOmissions(t *testing.T, mk Maker) {
	g, data := mk(t)
	ctx := context.Background()
	f := NewFixture(t, data, "diff-omissions")
	f.Write("base.txt", "x\n").Commit("base")
	f.Write("a.txt", "small\n")
	// The generated file (515 bytes) would overflow the 300-byte budget below
	// if it were read: OmitGenerated, not OmitUnavailable, proves its hunks
	// are not read.
	f.Write("go.sum", strings.Repeat("h1:"+strings.Repeat("z", 100)+"\n", 5))
	f.Lines("z-big.txt", "line", 2500)
	f.Commit("change")

	limits := domain.DiffLimits{MaxTotalBytes: 300, MaxFileLines: 2000, SkipGenerated: true}
	d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, limits)
	require.NoError(t, err)
	require.Len(t, d.Diff.Files, 3)
	byPath := map[string]domain.FileDiff{}
	for _, file := range d.Diff.Files {
		byPath[file.NewPath] = file
	}
	assert.Equal(t, domain.OmitNone, byPath["a.txt"].Omitted)
	assert.Len(t, byPath["a.txt"].Hunks, 1)
	assert.Equal(t, domain.OmitGenerated, byPath["go.sum"].Omitted)
	assert.Empty(t, byPath["go.sum"].Hunks)
	assert.Equal(t, 5, byPath["go.sum"].Additions)
	assert.Equal(t, domain.OmitLarge, byPath["z-big.txt"].Omitted)
	assert.Empty(t, byPath["z-big.txt"].Hunks)
	assert.False(t, d.Diff.Truncated)

	// Read, go.sum overflows the bytes that a.txt, read before it, already
	// spent: it may fit on its own, so it is OmitBudget, loadable alone.
	limits.SkipGenerated = false
	d, err = g.GetCommit(ctx, f.Name, domain.Revision{}, limits)
	require.NoError(t, err)
	sum := -1
	for i, file := range d.Diff.Files {
		if file.NewPath == "go.sum" {
			sum = i
			assert.Equal(t, domain.OmitBudget, file.Omitted, "earlier files spent bytes")
			assert.Empty(t, file.Hunks)
		}
	}
	require.GreaterOrEqual(t, sum, 0)
	// Alone, it is still over the bytes: OmitUnavailable, a dead end.
	alone := domain.DiffLimits{From: sum, To: sum + 1, MaxTotalBytes: 300}
	d, err = g.GetCommit(ctx, f.Name, domain.Revision{}, alone)
	require.NoError(t, err)
	assert.Equal(t, domain.OmitUnavailable, d.Diff.Files[sum].Omitted, "over the bytes on its own")
	assert.Empty(t, d.Diff.Files[sum].Hunks)

	// OneFileDiffLimits reads a file over 2,000 changed lines, and only it.
	idx := -1
	for i, file := range d.Diff.Files {
		if file.NewPath == "z-big.txt" {
			idx = i
		}
	}
	require.GreaterOrEqual(t, idx, 0)
	d, err = g.GetCommit(ctx, f.Name, domain.Revision{}, domain.OneFileDiffLimits(idx))
	require.NoError(t, err)
	for i, file := range d.Diff.Files {
		assert.Equal(t, domain.OmitNone, file.Omitted)
		if i == idx {
			require.Len(t, file.Hunks, 1)
			assert.Len(t, file.Hunks[0].Lines, 2500)
		} else {
			assert.Empty(t, file.Hunks)
		}
	}
	assert.False(t, d.Diff.Truncated)

	// MaxFiles alone sets Diff.Truncated.
	d, err = g.GetCommit(ctx, f.Name, domain.Revision{}, domain.DiffLimits{MaxFiles: 2, MaxTotalBytes: 1 << 20})
	require.NoError(t, err)
	assert.Len(t, d.Diff.Files, 2)
	assert.True(t, d.Diff.Truncated)
	for _, file := range d.Diff.Files {
		assert.Equal(t, domain.OmitNone, file.Omitted)
	}

	// A one-file read is capped at MaxOneFileLines changed lines: the cap
	// itself loads, one more line is OmitUnavailable (it cannot be loaded).
	f.Lines("cap.txt", "c", domain.MaxOneFileLines).Lines("over.txt", "o", domain.MaxOneFileLines+1).Commit("caps")
	d, err = g.GetCommit(ctx, f.Name, domain.Revision{}, domain.DiffLimits{MaxTotalBytes: 1})
	require.NoError(t, err)
	capIdx, overIdx := -1, -1
	for i, file := range d.Diff.Files {
		switch file.NewPath {
		case "cap.txt":
			capIdx = i
		case "over.txt":
			overIdx = i
		}
	}
	require.GreaterOrEqual(t, capIdx, 0)
	require.GreaterOrEqual(t, overIdx, 0)
	d, err = g.GetCommit(ctx, f.Name, domain.Revision{}, domain.OneFileDiffLimits(capIdx))
	require.NoError(t, err)
	assert.Equal(t, domain.OmitNone, d.Diff.Files[capIdx].Omitted)
	require.Len(t, d.Diff.Files[capIdx].Hunks, 1)
	assert.Len(t, d.Diff.Files[capIdx].Hunks[0].Lines, domain.MaxOneFileLines)
	d, err = g.GetCommit(ctx, f.Name, domain.Revision{}, domain.OneFileDiffLimits(overIdx))
	require.NoError(t, err)
	assert.Equal(t, domain.OmitUnavailable, d.Diff.Files[overIdx].Omitted)
	assert.Empty(t, d.Diff.Files[overIdx].Hunks)
}
