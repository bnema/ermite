// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestDeleteRefTipIfOnlyRemovesTheMatchingRecord(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	n := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
	ci := s.CI()
	ref := "refs/heads/feat"

	require.NoError(t, ci.PutRefTip(ctx, domain.CIRefTip{Repo: n, Ref: ref, Sha: "aaa", Actor: "ada"}))
	require.NoError(t, ci.DeleteRefTipIf(ctx, n, ref, "bbb"))
	got, err := ci.GetRefTip(ctx, n, ref)
	require.NoError(t, err)
	assert.Equal(t, "aaa", got.Sha, "a record of another object stays")

	require.NoError(t, ci.DeleteRefTipIf(ctx, n, ref, "aaa"))
	_, err = ci.GetRefTip(ctx, n, ref)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	require.NoError(t, ci.DeleteRefTipIf(ctx, n, ref, "aaa"), "a missing record is not an error")
}
