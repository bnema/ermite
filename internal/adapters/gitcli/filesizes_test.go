// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/readertest"
)

func TestListFileSizes(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = g.Close(ctx) })

	f := readertest.NewFixture(t, data, "proj")
	f.Write("main.go", "package main\n")
	f.Write("dir/sub/run.sh", "#!/bin/sh\necho\n")
	f.Chmod("dir/sub/run.sh", 0o755)
	f.Write("dir/empty.txt", "")
	f.Symlink("main.go", "link")
	f.Git("update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,vendor/mod")
	id := oidOf(t, f.Commit("files"))

	got, err := g.ListFileSizes(ctx, f.Name, id, 100)
	require.NoError(t, err)
	assert.False(t, got.Truncated)
	assert.Equal(t, []domain.FileSize{
		{Path: "dir/empty.txt", Size: 0},
		{Path: "dir/sub/run.sh", Size: 15},
		{Path: "main.go", Size: 13},
	}, got.Files, "blobs only: no symlink, no gitlink")

	got, err = g.ListFileSizes(ctx, f.Name, id, 2)
	require.NoError(t, err)
	assert.True(t, got.Truncated)
	assert.Len(t, got.Files, 2)

	got, err = g.ListFileSizes(ctx, f.Name, id, 3)
	require.NoError(t, err)
	assert.False(t, got.Truncated, "a list of exactly the bound is complete")

	_, err = g.ListFileSizes(ctx, f.Name, id, 0)
	assert.ErrorIs(t, err, domain.ErrInvalid)
	_, err = g.ListFileSizes(ctx, name(t, "missing"), id, 5)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestListFileSizesBounds(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = g.Close(ctx) })

	f := readertest.NewFixture(t, data, "bounded")
	f.Write("aaaa", "1")
	f.Write("bbbb", "2")
	f.Write("cccc", "3")
	id := oidOf(t, f.Commit("files"))

	got, err := g.ListFileSizesBounded(ctx, f.Name, id, 100, 8)
	require.NoError(t, err)
	assert.True(t, got.Truncated, "path bytes over the budget")
	assert.Len(t, got.Files, 2)

	got, err = g.ListFileSizesBounded(ctx, f.Name, id, 100, 12)
	require.NoError(t, err)
	assert.False(t, got.Truncated, "paths exactly at the budget")
	assert.Len(t, got.Files, 3)

	long := strings.Repeat("d", 5000)
	f.Git("update-index", "--add", "--cacheinfo", "100644,"+f.Git("rev-parse", "HEAD:aaaa")+","+long)
	f.Git("commit", "--quiet", "-m", "long")
	id = oidOf(t, f.Git("rev-parse", "HEAD"))
	got, err = g.ListFileSizes(ctx, f.Name, id, 100)
	require.NoError(t, err)
	assert.True(t, got.Truncated, "a record over the bound stops the listing")
	assert.Len(t, got.Files, 3)
}
