// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

func TestDisabledBernard(t *testing.T) {
	t.Parallel()
	ctx, d, repo := context.Background(), usecase.DisabledBernard{}, name(t, "app")
	list, err := d.List(ctx, domain.Owner(), domain.RepoName{})
	require.NoError(t, err)
	assert.Empty(t, list)
	_, err = d.List(ctx, domain.Anonymous(), domain.RepoName{})
	assert.ErrorIs(t, err, domain.ErrNotFound)
	for _, call := range []func(p domain.Principal) error{
		func(p domain.Principal) error { _, err := d.Check(ctx, p, repo); return err },
		func(p domain.Principal) error { _, err := d.Open(ctx, p, repo, nil); return err },
		func(p domain.Principal) error { _, err := d.Ignore(ctx, p, repo, nil); return err },
		func(p domain.Principal) error { _, err := d.Snooze(ctx, p, repo); return err },
	} {
		err := call(domain.Owner())
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.ErrorContains(t, err, "Bernard is disabled")
		assert.ErrorIs(t, call(domain.Anonymous()), domain.ErrNotFound)
	}
}
