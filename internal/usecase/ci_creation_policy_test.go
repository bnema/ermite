// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestCreateRunWorkflowPolicy(t *testing.T) {
	repo, err := domain.ParseRepoName("project")
	require.NoError(t, err)
	failure := errors.New("state unavailable")
	for _, tc := range []struct {
		name           string
		off            []string
		stateErr, want error
	}{
		{name: "enabled"},
		{name: "other disabled", off: []string{"other.yml"}},
		{name: "disabled", off: []string{"ci.yml"}, want: domain.ErrWorkflowDisabled},
		{name: "state failure", stateErr: failure, want: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := mocks.NewCIWorkflowStore(t)
			store := mocks.NewCIStore(t)
			state.EXPECT().DisabledWorkflows(context.Background(), repo).Return(tc.off, tc.stateErr).Once()
			c := &CI{cfg: CIConfig{Store: store, WorkflowState: state}}
			build := func(id, number int64) ports.CIRunDraft { return ports.CIRunDraft{} }
			if tc.want == nil {
				store.EXPECT().CreateRun(context.Background(), repo, "ci.yml", mock.Anything).Return(ports.CIRunDraft{}, nil).Once()
			}
			_, err := c.createRun(context.Background(), pushFacts{}, repo, "ci.yml", build)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}
