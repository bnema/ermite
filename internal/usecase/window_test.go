// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

func TestWindowLimits(t *testing.T) {
	def := domain.DefaultDiffLimits()
	win := def
	win.MaxFiles, win.From, win.To, win.SkipGenerated = 0, 2, 5, true
	open := win
	open.From, open.To = 7, 0

	tests := []struct {
		name string
		w    *ports.FileWindow
		want domain.DiffLimits
		bad  bool
	}{
		{name: "nil is the default", want: def},
		{name: "a window", w: &ports.FileWindow{From: 2, To: 5}, want: win},
		{name: "an open window", w: &ports.FileWindow{From: 7}, want: open},
		{name: "full", w: &ports.FileWindow{From: 3, To: 4, Full: true}, want: domain.OneFileDiffLimits(3)},
		{name: "negative from", w: &ports.FileWindow{From: -1}, bad: true},
		{name: "negative to", w: &ports.FileWindow{To: -1}, bad: true},
		{name: "to before from", w: &ports.FileWindow{From: 5, To: 3}, bad: true},
		{name: "full without to", w: &ports.FileWindow{From: 3, Full: true}, bad: true},
		{name: "full of two files", w: &ports.FileWindow{From: 3, To: 5, Full: true}, bad: true},
		{name: "full of no file", w: &ports.FileWindow{From: 3, To: 3, Full: true}, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := usecase.WindowLimits(tt.w)
			if tt.bad {
				require.ErrorIs(t, err, domain.ErrInvalid)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGetMergeRequestFullAndGenerated(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.sh("checkout", "-q", "-B", "feat", "main")
	r.commit(author, "a.txt", "one\n", "a")
	r.commit(author, "go.sum", "x y z\n", "sum")
	r.push("feat")
	n := r.open("feat").Number

	t.Run("full with a wider window is invalid", func(t *testing.T) {
		_, err := r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{Files: &ports.FileWindow{From: 0, To: 2, Full: true}})
		require.ErrorIs(t, err, domain.ErrInvalid)
	})

	t.Run("a windowed read collapses a generated file, a nil read does not", func(t *testing.T) {
		d, err := r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{Files: &ports.FileWindow{From: 0, To: 10}})
		require.NoError(t, err)
		byPath := map[string]domain.FileDiff{}
		for _, f := range d.Diff.Files {
			byPath[f.NewPath] = f
		}
		assert.Equal(t, domain.OmitGenerated, byPath["go.sum"].Omitted)
		assert.Empty(t, byPath["go.sum"].Hunks)
		assert.Equal(t, domain.OmitNone, byPath["a.txt"].Omitted)
		assert.NotEmpty(t, byPath["a.txt"].Hunks)

		d, err = r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{})
		require.NoError(t, err)
		for _, f := range d.Diff.Files {
			assert.Equal(t, domain.OmitNone, f.Omitted, f.NewPath)
			assert.NotEmpty(t, f.Hunks, f.NewPath)
		}
	})
}

func bigContent(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func TestCheckLineBeyondFileLimit(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.sh("checkout", "-q", "-B", "feat", "main")
	r.commit(author, "big.txt", bigContent(3000), "big")
	r.commit(author, "go.sum", "h1:x\nh1:y\n", "sum")
	r.push("feat")
	n := r.open("feat").Number

	add := func(path string, line int) error {
		_, err := r.mrs.AddComment(r.ctx, mrW, r.repo, n, ports.AddCommentParams{Path: path, Line: line, Side: domain.SideNew, Text: "c"})
		return err
	}
	require.NoError(t, add("big.txt", 1))
	require.NoError(t, add("big.txt", 2500))
	require.NoError(t, add("big.txt", 3000))
	require.ErrorIs(t, add("big.txt", 3001), domain.ErrInvalid)
	// A generated file is collapsed in the web windows, but loadable: its
	// lines can be commented on.
	require.NoError(t, add("go.sum", 2))
}

func TestBrowseGetCommitFullWindow(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.commit(author, "a-big.txt", bigContent(3000), "big")
	id := r.head()
	r.push("main")
	b := usecase.NewBrowse(r.store, r.git)
	rev := domain.RevisionOf(id)

	d, err := b.GetCommit(r.ctx, root, r.repo, rev, ports.GetCommitOptions{})
	require.NoError(t, err)
	require.Len(t, d.Diff.Files, 1)
	assert.Equal(t, domain.OmitLarge, d.Diff.Files[0].Omitted)
	assert.Empty(t, d.Diff.Files[0].Hunks)

	d, err = b.GetCommit(r.ctx, root, r.repo, rev, ports.GetCommitOptions{Files: &ports.FileWindow{From: 0, To: 1, Full: true}})
	require.NoError(t, err)
	require.Len(t, d.Diff.Files, 1)
	f := d.Diff.Files[0]
	assert.Equal(t, domain.OmitNone, f.Omitted)
	require.NotEmpty(t, f.Hunks)
	assert.Equal(t, 3000, f.Additions)

	_, err = b.GetCommit(r.ctx, root, r.repo, rev, ports.GetCommitOptions{Files: &ports.FileWindow{From: 0, To: 2, Full: true}})
	require.ErrorIs(t, err, domain.ErrInvalid)
}

func TestBrowseGetCommitWindowCollapsesGenerated(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.commit(author, "go.sum", "h1:x\n", "sum")
	id := r.head()
	r.push("main")
	b := usecase.NewBrowse(r.store, r.git)
	rev := domain.RevisionOf(id)

	d, err := b.GetCommit(r.ctx, root, r.repo, rev, ports.GetCommitOptions{})
	require.NoError(t, err)
	require.Len(t, d.Diff.Files, 1)
	assert.Equal(t, domain.OmitNone, d.Diff.Files[0].Omitted, "API reads keep generated files")

	d, err = b.GetCommit(r.ctx, root, r.repo, rev, ports.GetCommitOptions{Files: &ports.FileWindow{From: 0, To: 25}})
	require.NoError(t, err)
	require.Len(t, d.Diff.Files, 1)
	assert.Equal(t, domain.OmitGenerated, d.Diff.Files[0].Omitted, "web windows collapse them")
	assert.Empty(t, d.Diff.Files[0].Hunks)
}
