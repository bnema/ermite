// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// manyFiles commits n one-line files m/f000... in one commit and
// returns the commit id.
func (r *mrRig) manyFiles(n int) domain.ObjectID {
	r.t.Helper()
	for i := range n {
		full := filepath.Join(r.work, "m", fmt.Sprintf("f%03d", i))
		require.NoError(r.t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(r.t, os.WriteFile(full, []byte(fmt.Sprintf("line %d\n", i)), 0o644))
	}
	r.sh("add", "m")
	r.sh("commit", "-q", "-m", "many files")
	return r.head()
}

const manyFilesCount = 320

// bigMR opens a request that changes manyFilesCount files.
func bigMR(t *testing.T) (*mrRig, domain.MergeRequest) {
	t.Helper()
	r := newMRRig(t)
	r.seed()
	r.sh("checkout", "-q", "-B", "feat", "main")
	r.manyFiles(manyFilesCount)
	r.push("feat")
	return r, r.open("feat")
}

func TestGetMergeRequestFilesWindow(t *testing.T) {
	r, mr := bigMR(t)
	n := mr.Number

	t.Run("the default diff keeps the contract limit", func(t *testing.T) {
		d, err := r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{})
		require.NoError(t, err)
		assert.Len(t, d.Diff.Files, 300)
		assert.True(t, d.Diff.Truncated)
		assert.Equal(t, manyFilesCount, d.Stats.FilesChanged)
	})

	t.Run("every file is listed, the hunks are those of the window", func(t *testing.T) {
		d, err := r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{Files: &ports.FileWindow{From: 305, To: 310}})
		require.NoError(t, err)
		require.Len(t, d.Diff.Files, manyFilesCount)
		assert.Equal(t, manyFilesCount, d.Stats.FilesChanged)
		assert.False(t, d.Diff.Truncated)
		for i, f := range d.Diff.Files {
			assert.Equal(t, fmt.Sprintf("m/f%03d", i), f.NewPath)
			assert.Equal(t, 1, f.Additions)
			assert.Equal(t, domain.OmitNone, f.Omitted)
			if i >= 305 && i < 310 {
				require.Len(t, f.Hunks, 1, f.NewPath)
				assert.Equal(t, fmt.Sprintf("line %d", i), f.Hunks[0].Lines[0].Text)
			} else {
				assert.Empty(t, f.Hunks, f.NewPath)
			}
		}
	})

	t.Run("an empty window past the end lists the files only", func(t *testing.T) {
		d, err := r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{Files: &ports.FileWindow{From: 1000}})
		require.NoError(t, err)
		require.Len(t, d.Diff.Files, manyFilesCount)
		for _, f := range d.Diff.Files {
			assert.Empty(t, f.Hunks)
		}
	})

	t.Run("an open window reads through the end", func(t *testing.T) {
		d, err := r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{Files: &ports.FileWindow{From: 315}})
		require.NoError(t, err)
		require.Len(t, d.Diff.Files, manyFilesCount)
		assert.Empty(t, d.Diff.Files[314].Hunks)
		for _, f := range d.Diff.Files[315:] {
			assert.Len(t, f.Hunks, 1)
		}
	})

	t.Run("an invalid window is refused", func(t *testing.T) {
		for _, w := range []ports.FileWindow{{From: -1}, {To: -1}, {From: 5, To: 3}} {
			_, err := r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{Files: &w})
			require.ErrorIs(t, err, domain.ErrInvalid, "%+v", w)
		}
	})

	t.Run("a comment on a line of a file past the 300th is accepted", func(t *testing.T) {
		c, err := r.mrs.AddComment(r.ctx, mrW, r.repo, n, ports.AddCommentParams{Path: "m/f315", Line: 1, Side: domain.SideNew, Text: "late file"})
		require.NoError(t, err)
		assert.Equal(t, "m/f315", c.Path)
		_, err = r.mrs.AddComment(r.ctx, mrW, r.repo, n, ports.AddCommentParams{Path: "m/f319", Line: 1, Side: domain.SideNew, Text: "last file"})
		require.NoError(t, err)

		_, err = r.mrs.AddComment(r.ctx, mrW, r.repo, n, ports.AddCommentParams{Path: "m/f315", Line: 2, Side: domain.SideNew, Text: "no such line"})
		require.ErrorIs(t, err, domain.ErrInvalid)
		_, err = r.mrs.AddComment(r.ctx, mrW, r.repo, n, ports.AddCommentParams{Path: "m/f999", Line: 1, Side: domain.SideNew, Text: "no such file"})
		require.ErrorIs(t, err, domain.ErrInvalid)
	})
}

func TestGetMergeRequestBaseAndHeadChecks(t *testing.T) {
	r := newMRRig(t)
	base := r.seed()
	head1 := r.feature("feat")
	mr := r.open("feat")
	n := mr.Number
	other := oidFrom(t, "1111111111111111111111111111111111111111")

	t.Run("matching or unset base and head", func(t *testing.T) {
		for _, o := range []ports.GetMergeRequestOptions{
			{}, {Base: base}, {Head: head1}, {Base: base, Head: head1}, {Base: base, Head: head1, Files: &ports.FileWindow{To: 1}},
		} {
			d, err := r.mrs.Get(r.ctx, reader, r.repo, n, o)
			require.NoError(t, err, "%+v", o)
			assert.Equal(t, base, d.DiffBaseID)
		}
	})

	t.Run("another base or head is a conflict", func(t *testing.T) {
		for _, o := range []ports.GetMergeRequestOptions{
			{Base: other}, {Head: other}, {Base: base, Head: other}, {Base: other, Head: head1},
			{Base: head1}, // the head is not the base
			{Base: other, Files: &ports.FileWindow{From: 1, To: 2}},
		} {
			_, err := r.mrs.Get(r.ctx, reader, r.repo, n, o)
			require.ErrorIs(t, err, domain.ErrConflict, "%+v", o)
		}
	})

	t.Run("the head moved", func(t *testing.T) {
		r.commit(author, "d.txt", "dee\n", "add d")
		head2 := r.head()
		r.push("feat")
		_, err := r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{Base: base, Head: head1})
		require.ErrorIs(t, err, domain.ErrConflict)
		d, err := r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{Base: base, Head: head2})
		require.NoError(t, err)
		assert.Equal(t, head2, d.MergeRequest.HeadID)
	})

	t.Run("the base is the one after since-last-read", func(t *testing.T) {
		head2 := r.branchHead("feat")
		_, err := r.mrs.MarkRead(r.ctx, mrW, r.repo, n, head1)
		require.NoError(t, err)
		// The whole diff (base) is no longer what since-last-read shows (head1).
		_, err = r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{SinceLastRead: true, Base: base, Head: head2})
		require.ErrorIs(t, err, domain.ErrConflict)
		d, err := r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{SinceLastRead: true, Base: head1, Head: head2})
		require.NoError(t, err)
		assert.Equal(t, head1, d.DiffBaseID)
		require.Len(t, d.Diff.Files, 1)
		assert.Equal(t, "d.txt", d.Diff.Files[0].NewPath)
		// Without since-last-read the base is still the merge base.
		_, err = r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{Base: head1, Head: head2})
		require.ErrorIs(t, err, domain.ErrConflict)
	})
}

func TestGetMergeRequestDiffOnly(t *testing.T) {
	r := newMRRig(t)
	base := r.seed()
	r.feature("feat")
	mr := r.open("feat")

	full, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	require.Len(t, full.Commits, 2)

	d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{DiffOnly: true})
	require.NoError(t, err)
	assert.Empty(t, d.Commits)
	assert.False(t, d.CommitsTruncated)
	assert.Equal(t, base, d.DiffBaseID)
	assert.Equal(t, full.Diff, d.Diff)
	assert.Equal(t, full.Stats, d.Stats)
	assert.Equal(t, full.Comments, d.Comments)
}
