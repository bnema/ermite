// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestArchiveWritesAGzipTarUnderThePrefix(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	t.Cleanup(func() { _ = g.Close(ctx) })

	var buf bytes.Buffer
	require.NoError(t, g.Archive(ctx, m.f.Name, m.d, "owner-proj-abcdef0", &buf))
	zr, err := gzip.NewReader(&buf)
	require.NoError(t, err)
	tr := tar.NewReader(zr)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if h.Typeflag == tar.TypeXGlobalHeader {
			// git archive records the commit id here, as GitHub's tarballs do.
			assert.Equal(t, m.d.String(), h.PAXRecords["comment"])
			continue
		}
		require.True(t, strings.HasPrefix(h.Name, "owner-proj-abcdef0/"), h.Name)
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			files[h.Name] = string(b)
		}
	}
	assert.Equal(t, "one\n2\nthree\n", files["owner-proj-abcdef0/a.txt"], "the tree of the commit asked, not of HEAD")
	assert.Equal(t, "see\n", files["owner-proj-abcdef0/c.txt"])
	assert.NotContains(t, files, "owner-proj-abcdef0/.git")
}

func TestArchiveOfAMissingCommitWritesNothing(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	t.Cleanup(func() { _ = g.Close(ctx) })

	var buf bytes.Buffer
	missing, err := domain.ParseObjectID(strings.Repeat("1", 40))
	require.NoError(t, err)
	err = g.Archive(ctx, m.f.Name, missing, "p", &buf)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.Zero(t, buf.Len(), "nothing was written, so the caller can still answer 404")

	err = g.Archive(ctx, name(t, "nope"), m.d, "p", &buf)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestArchiveStopsWithItsContext(t *testing.T) {
	g, data := newGit(t)
	m := newMergeFixture(t, data)
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := g.Archive(ctx, m.f.Name, m.d, "p", io.Discard)
	assert.ErrorIs(t, err, context.Canceled)
}
