// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/domain"
)

func TestNextDepsWork(t *testing.T) {
	a, _ := domain.ParseRepoName("a")
	b, _ := domain.ParseRepoName("b")
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	batch := func(key string, st domain.DepsState, manual bool, at int) domain.DepBatch {
		return domain.DepBatch{Key: domain.DepBatchKey(key), State: st, Manual: manual, QueuedAt: t0.Add(time.Duration(at) * time.Minute)}
	}
	recs := []domain.RepoDeps{
		{Repo: a, Batches: []domain.DepBatch{batch("", domain.DepsDrafted, false, 1), batch("x", domain.DepsApplying, false, 0)}},
		{Repo: b, Batches: []domain.DepBatch{batch("", domain.DepsDrafted, true, 5), batch("y", domain.DepsIdle, false, 0)}},
	}
	assert.Equal(t, 1, domain.DepsRunning(recs))
	order := domain.DepsQueueOrder(recs)
	assert.Len(t, order, 2)
	assert.Equal(t, b, order[0].Repo, "manual first")
	assert.Equal(t, 1, domain.DepsQueuePosition(order, b, ""))
	assert.Equal(t, 2, domain.DepsQueuePosition(order, a, ""))
	assert.Zero(t, domain.DepsQueuePosition(order, a, "x"))

	for _, c := range []struct {
		name           string
		running, limit int
		waiting        bool
		wantRepo       domain.RepoName
		wantOK         bool
	}{
		{"manual takes the slot", 1, 2, false, b, true},
		{"manual ignores waiting workflows", 1, 2, true, b, true},
		{"limit reached", 2, 2, false, domain.RepoName{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo, _, ok := domain.NextDepsWork(recs, c.running, c.limit, c.waiting)
			assert.Equal(t, c.wantOK, ok)
			assert.Equal(t, c.wantRepo, repo)
		})
	}

	auto := recs[:1]
	_, _, ok := domain.NextDepsWork(auto, 0, 1, true)
	assert.False(t, ok, "an automatic run waits for the workflow jobs")
	repo, _, ok := domain.NextDepsWork(auto, 0, 1, false)
	assert.True(t, ok)
	assert.Equal(t, a, repo)

	// A Queued batch takes its server step even with no slot free, behind
	// a manual batch that waits for one.
	queued := append(recs, domain.RepoDeps{Repo: a, Batches: []domain.DepBatch{batch("q", domain.DepsQueued, false, 9)}})
	_, key, ok := domain.NextDepsWork(queued, 2, 2, true)
	assert.True(t, ok)
	assert.Equal(t, domain.DepBatchKey("q"), key)
}
