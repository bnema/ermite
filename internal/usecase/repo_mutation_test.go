// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

func expectRepoMutation(t *testing.T, meta *mocks.RepoMetaStore, repo domain.Repo, want any) *mocks.RepoMetaStore_Mutate_Call {
	t.Helper()
	return meta.EXPECT().Mutate(mock.Anything, repo.Name, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.RepoName, fn func(*domain.Repo) (bool, error)) (domain.Repo, error) {
		result := repo
		_, err := fn(&result)
		require.NoError(t, err)
		_, differences := (mock.Arguments{want}).Diff([]any{result})
		require.Zero(t, differences, "unexpected mutated repository: %+v", result)
		return result, err
	})
}

func expectPushMutation(t *testing.T, meta *mocks.RepoMetaStore, repo domain.Repo, err error) *mocks.RepoMetaStore_Mutate_Call {
	t.Helper()
	if err != nil {
		return meta.EXPECT().Mutate(mock.Anything, mock.Anything, mock.Anything).Return(repo, err)
	}
	return expectRepoMutation(t, meta, repo, mock.Anything)
}
