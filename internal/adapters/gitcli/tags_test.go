// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestCreateReleaseTag(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	t.Cleanup(func() { _ = g.Close(ctx) })
	require.NoError(t, g.CreateTag(ctx, m.f.Name, "v1.0", m.a))
	rev, err := domain.ParseRevision("refs/tags/v1.0")
	require.NoError(t, err)
	got, err := g.ResolveRevision(ctx, m.f.Name, rev)
	require.NoError(t, err)
	require.Equal(t, m.a, got.CommitID)
	require.ErrorIs(t, g.CreateTag(ctx, m.f.Name, "v1.0", m.d), domain.ErrAlreadyExists)
	got, err = g.ResolveRevision(ctx, m.f.Name, rev)
	require.NoError(t, err)
	require.Equal(t, m.a, got.CommitID)
	for _, tag := range []string{"", "../bad", "bad^{commit}", "bad\\name"} {
		require.ErrorIs(t, g.CreateTag(ctx, m.f.Name, tag, m.d), domain.ErrInvalid, tag)
	}
}
