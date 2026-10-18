// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const wfRelease = ".github/workflows/release.yml"

func TestCIWorkflowDisabledState(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	ci := s.CI()
	a, b := name(t, "a"), name(t, "b")
	for _, n := range []domain.RepoName{a, b} {
		require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created, Settings: domain.DefaultRepoSettings()}))
	}

	got, err := ci.DisabledWorkflows(ctx, a)
	require.NoError(t, err)
	assert.Empty(t, got)

	require.NoError(t, ci.DisableWorkflow(ctx, a, wfRelease, created))
	require.NoError(t, ci.DisableWorkflow(ctx, a, wfRelease, created), "idempotent")
	require.NoError(t, ci.DisableWorkflow(ctx, a, ".github/workflows/aur.yml", created))
	got, err = ci.DisabledWorkflows(ctx, a)
	require.NoError(t, err)
	assert.Equal(t, []string{".github/workflows/aur.yml", wfRelease}, got)
	got, _ = ci.DisabledWorkflows(ctx, b)
	assert.Empty(t, got, "per repository")

	require.NoError(t, ci.EnableWorkflow(ctx, a, wfRelease))
	require.NoError(t, ci.EnableWorkflow(ctx, a, wfRelease), "idempotent")
	got, _ = ci.DisabledWorkflows(ctx, a)
	assert.Equal(t, []string{".github/workflows/aur.yml"}, got)

	assert.ErrorIs(t, ci.DisableWorkflow(ctx, name(t, "gone"), wfRelease, created), domain.ErrNotFound)

	// The state goes with the repository: a new one under the same name starts enabled.
	require.NoError(t, s.Delete(ctx, a))
	require.NoError(t, s.Create(ctx, domain.Repo{Name: a, Visibility: domain.VisibilityPrivate, CreatedAt: created, Settings: domain.DefaultRepoSettings()}))
	got, _ = ci.DisabledWorkflows(ctx, a)
	assert.Empty(t, got)
}

// A run is refused for a disabled workflow in the very transaction that stores
// it: a disable that lands between the read of the state and the write of the
// run (here, from inside build) cannot be overtaken, and no run is left.
func TestCreateRunRefusesAWorkflowDisabledMeanwhile(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	ci := s.CI()
	n := name(t, "a")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created, Settings: domain.DefaultRepoSettings()}))
	draft := func(id, number int64) ports.CIRunDraft {
		return ports.CIRunDraft{Run: domain.CIRun{Repo: n, ID: id, Number: number, Attempt: 1, WorkflowPath: wfRelease, Status: domain.CIQueued, CreatedAt: created}}
	}

	// Enabled: a run is created.
	_, err := ci.CreateRun(ctx, n, wfRelease, draft)
	require.NoError(t, err)

	// Disabled from inside build, after the state was read.
	calls := 0
	_, err = ci.CreateRun(ctx, n, wfRelease, func(id, number int64) ports.CIRunDraft {
		calls++
		if calls == 1 {
			require.NoError(t, ci.DisableWorkflow(ctx, n, wfRelease, created))
		}
		return draft(id, number)
	})
	assert.ErrorIs(t, err, domain.ErrWorkflowDisabled)
	assert.Equal(t, 1, calls, "refused on the retry, before build")
	runs, err := ci.ListRuns(ctx, n, 0, 10)
	require.NoError(t, err)
	assert.Len(t, runs, 1, "no run was stored by the refused call")

	// Other workflows are not affected, and enabling restores it.
	_, err = ci.CreateRun(ctx, n, ".github/workflows/other.yml", draft)
	require.NoError(t, err)
	require.NoError(t, ci.EnableWorkflow(ctx, n, wfRelease))
	_, err = ci.CreateRun(ctx, n, wfRelease, draft)
	require.NoError(t, err)
}
