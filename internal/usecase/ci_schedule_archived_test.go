// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/require"
)

func TestScheduleSkipsArchivedAndPrunesCache(t *testing.T) {
	ctx := t.Context()
	n, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	meta := mocks.NewRepoMetaStore(t)
	store := mocks.NewCIStore(t)
	repo := domain.Repo{Name: n, Archived: true}
	meta.EXPECT().List(ctx).Return([]domain.Repo{repo}, nil).Once()
	store.EXPECT().ListRunners(ctx).Return([]domain.CIRunner{{ID: "runner"}}, nil).Once()
	c := &CI{cfg: CIConfig{Meta: meta, Store: store}, sched: newScheduler(time.Time{})}
	c.sched.put(n, &scheduleEntry{})
	c.sched.markDirty(n)
	pass := c.startPass(ctx, time.Now())
	require.Empty(t, pass.repos)
	require.Equal(t, -1, c.CachedSchedules(n))
	require.Empty(t, c.sched.takeDirty(1))
	// The final firing path also refuses an archived repository without touching storage.
	require.NoError(t, c.fireSchedule(ctx, repo, "main", nil, domain.Commit{}, scheduledWorkflow{}, "* * * * *"))
}

func TestScheduleCachedBeforeArchiveDoesNotFire(t *testing.T) {
	ctx := t.Context()
	n, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	tip, err := domain.ParseObjectID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	cron, err := domain.ParseCron("* * * * *")
	require.NoError(t, err)
	meta := mocks.NewRepoMetaStore(t)
	repos := mocks.NewGitRepos(t)
	git := mocks.NewMergeGit(t)
	repos.EXPECT().DefaultBranch(ctx, n).Return("main", true, nil).Once()
	git.EXPECT().BranchHead(ctx, n, "main").Return(tip, nil).Once()
	meta.EXPECT().Get(ctx, n).Return(domain.Repo{Name: n, Archived: true}, nil).Once()
	c := &CI{cfg: CIConfig{Meta: meta, Repos: repos, Git: git, Store: mocks.NewCIStore(t)}, sched: newScheduler(time.Time{})}
	c.sched.put(n, &scheduleEntry{tip: tip, files: []scheduledWorkflow{{path: ".github/workflows/cron.yml", crons: []scheduledCron{{spec: "* * * * *", sched: domain.CronSchedule{Cron: cron, Loc: time.UTC}}}}}})
	require.NoError(t, c.scheduleRepo(ctx, n, time.Now().UTC().Truncate(time.Minute)))
	require.Equal(t, -1, c.CachedSchedules(n))
}
