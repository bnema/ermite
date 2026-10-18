// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/ermitegit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/readertest"
)

// A repository ermite-git cannot read (alternates) is served by the wrapped
// reader, with the same bytes.
func TestReadCommitObjectsUnsupportedRepositoryFallsBack(t *testing.T) {
	ctx := context.Background()
	a, g, data := newAdapter(t, ermitegit.DefaultRoutes(), nil)
	h := readertest.NewHistory(t, data)

	tip := readertest.ObjectID(t, h.Git("rev-parse", "HEAD"))
	ids := []domain.ObjectID{tip, readertest.ObjectID(t, h.Git("rev-parse", "HEAD^{tree}"))}
	require.NoError(t, os.WriteFile(filepath.Join(h.Bare, "objects", "info", "alternates"), []byte(filepath.Join(t.TempDir(), "objects")+"\n"), 0o644))
	want, err := g.ReadCommitObjects(ctx, h.Name, ids)
	require.NoError(t, err)
	require.Len(t, want, 1)
	got, err := a.ReadCommitObjects(ctx, h.Name, ids)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// The library and git read the same bytes for every commit of a history.
func TestReadCommitObjectsMatchesCLI(t *testing.T) {
	ctx := context.Background()
	a, g, data := newAdapter(t, ermitegit.DefaultRoutes(), nil)
	h := readertest.NewHistory(t, data)
	var ids []domain.ObjectID
	for _, line := range strings.Fields(h.Git("rev-list", "--all")) {
		ids = append(ids, readertest.ObjectID(t, line))
	}
	require.NotEmpty(t, ids)
	want, err := g.ReadCommitObjects(ctx, h.Name, ids)
	require.NoError(t, err)
	got, err := a.ReadCommitObjects(ctx, h.Name, ids)
	require.NoError(t, err)
	assert.Len(t, got, len(ids))
	assert.Equal(t, want, got)
}
