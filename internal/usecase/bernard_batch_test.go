// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestBernardFindOpenMRUsesBatchBranch(t *testing.T) {
	for _, key := range []domain.DepBatchKey{"", "github-actions/.github/workflows/ci.yml/actions/checkout@v5"} {
		t.Run(string(key), func(t *testing.T) {
			repo, err := domain.ParseRepoName("proj")
			require.NoError(t, err)
			store := mocks.NewMergeRequestStore(t)
			branch := key.Branch()
			store.EXPECT().OpenBySource(mock.Anything, repo, []string{branch}, 16).Return([]int64{3, 4}, false, nil)
			store.EXPECT().Get(mock.Anything, repo, int64(3)).Return(domain.MergeRequest{Number: 3, Source: "other", Target: "main", State: domain.MRStateOpen}, nil)
			store.EXPECT().Get(mock.Anything, repo, int64(4)).Return(domain.MergeRequest{Number: 4, Source: branch, Target: "main", State: domain.MRStateOpen}, nil)
			b := Bernard{cfg: BernardConfig{MRStore: store}}
			got, err := b.findOpenMR(t.Context(), repo, "main", branch)
			require.NoError(t, err)
			require.Equal(t, int64(4), got)
		})
	}
}
