// SPDX-License-Identifier: AGPL-3.0-or-later

package ciartifacts_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/ciartifacts"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var ref = ports.CIArtifactRef{RunID: 7, ID: 42}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestLegacyReadAndRemoval(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := ciartifacts.New(root)
	run := filepath.Join(root, "00000000000000000007")
	require.NoError(t, os.MkdirAll(run, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(run, "00000000000000000042"), []byte("hello world"), 0o600))
	r, err := s.Open(ctx, ref, 6)
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Equal(t, "world", string(data))
	ids, err := s.ArtifactIDs(ctx, 7)
	require.NoError(t, err)
	assert.Equal(t, []int64{42}, ids)
	require.NoError(t, s.Remove(ctx, ref))
	require.NoError(t, s.Remove(ctx, ref))
	_, err = s.Open(ctx, ref, 0)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	require.NoError(t, s.RemoveRuns(ctx, []int64{7}))
	assert.Empty(t, entries(t, root))
	for _, bad := range []ports.CIArtifactRef{{RunID: 0, ID: 1}, {RunID: 1, ID: -1}} {
		_, err := s.Open(ctx, bad, 0)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}
}

func TestOrphanListingSeesEveryKindOfFile(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := ciartifacts.New(root)
	run := filepath.Join(root, "00000000000000000009")
	require.NoError(t, os.MkdirAll(filepath.Join(run, "00000000000000000003.blocks"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(run, "00000000000000000001"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(run, ".tmp-00000000000000000002-123"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(run, "junk"), nil, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "not-a-run"), 0o700))

	ids, err := s.ArtifactIDs(ctx, 9)
	require.NoError(t, err)
	assert.Equal(t, []int64{1, 2, 3}, ids)
	runs, err := s.RunIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int64{9}, runs)

	// Remove takes the assembly in progress too.
	require.NoError(t, s.Remove(ctx, ports.CIArtifactRef{RunID: 9, ID: 2}))
	ids, _ = s.ArtifactIDs(ctx, 9)
	assert.Equal(t, []int64{1, 3}, ids)
}
