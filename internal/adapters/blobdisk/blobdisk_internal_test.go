// SPDX-License-Identifier: AGPL-3.0-or-later
package blobdisk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/require"
)

func age(t *testing.T, p string) {
	t.Helper()
	old := time.Now().Add(-2 * tmpGrace)
	require.NoError(t, os.Chtimes(p, old, old))
}

func TestCheckReclaimsOldTmp(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	ctx := context.Background()
	require.NoError(t, s.Check(ctx))
	oldF, freshF := filepath.Join(root, "tmp", ".tmp-old"), filepath.Join(root, "tmp", ".tmp-fresh")
	oldD := filepath.Join(root, "tmp", "mp-old")
	require.NoError(t, os.WriteFile(oldF, []byte("x"), 0600))
	require.NoError(t, os.WriteFile(freshF, []byte("x"), 0600))
	require.NoError(t, os.MkdirAll(oldD, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(oldD, "metadata"), []byte("x"), 0600))
	age(t, oldF)
	age(t, oldD)
	require.NoError(t, s.Check(ctx))
	require.NoFileExists(t, oldF)
	require.NoDirExists(t, oldD)
	require.FileExists(t, freshF)
}

func TestListMultipartSkipsAndReclaimsMalformed(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	ctx := context.Background()
	a, err := s.NewMultipart(ctx, "a")
	require.NoError(t, err)
	c, err := s.NewMultipart(ctx, "c")
	require.NoError(t, err)
	mk := func(id string, meta *string) string {
		d := filepath.Join(root, "multipart", id)
		require.NoError(t, os.Mkdir(d, 0700))
		if meta != nil {
			require.NoError(t, os.WriteFile(filepath.Join(d, "metadata"), []byte(*meta), 0600))
		}
		return d
	}
	trunc := `{"Key":`
	freshBad := mk(strings.Repeat("1", 32), &trunc)
	oldBad := mk(strings.Repeat("2", 32), nil)
	oldTrunc := mk(strings.Repeat("3", 32), &trunc)
	age(t, oldBad)
	age(t, oldTrunc)
	var got []string
	require.NoError(t, s.ListMultipart(ctx, "", func(i ports.MultipartInfo) error { got = append(got, i.ID); return nil }))
	require.ElementsMatch(t, []string{a.ID(), c.ID()}, got)
	require.DirExists(t, freshBad)
	require.NoDirExists(t, oldBad)
	require.NoDirExists(t, oldTrunc)
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestMkdirAllSyncSyncsParents(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	var synced []string
	orig := syncDir
	syncDir = func(p string) error { synced = append(synced, p); return orig(p) }
	t.Cleanup(func() { syncDir = orig })
	_, err := s.Put(context.Background(), "a/b/c/obj", strings.NewReader("hi"), 2)
	require.NoError(t, err)
	obj := filepath.Join(root, "objects")
	for _, want := range []string{obj, filepath.Join(obj, "a"), filepath.Join(obj, "a/b"), filepath.Join(obj, "a/b/c")} {
		require.Contains(t, synced, want)
	}
}
