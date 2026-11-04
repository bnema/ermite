// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

// autoOpenRig is a bernardRig with automatic merge requests on and a daily check
// that finds a minor and a major update of a/mod.
func autoOpenRig(t *testing.T) *bernardRig {
	t.Helper()
	r := newBernardRig(t, "0 6 * * *")
	r.b.EnableAutoOpenForTest(mocks.NewMergeGit(t), mocks.NewGitRepos(t), mocks.NewCommitSigner(t), mocks.NewMergeRequestService(t), mocks.NewMergeRequestStore(t))
	c := goCand("a/mod", r.old("v0.21.0"), r.old("v1.0.0"))
	r.cands = []domain.DepCandidate{c}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil).Maybe()
	r.reg.EXPECT().Malicious(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, nil).Maybe()
	r.reg.EXPECT().GoMajorPaths(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Maybe()
	return r
}

// daily runs the scheduled check of the repository.
func (r *bernardRig) daily() {
	r.t.Helper()
	r.repoReads()
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: r.repo}}, nil).Once()
	r.store.EXPECT().List(mock.Anything).Return(nil, nil).Maybe()
	r.at = time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)
	r.b.ScheduleNow(r.t.Context())
	require.Equal(r.t, 1, r.regs, "the check ran")
}

func TestBernardAutoOpensBaseBatch(t *testing.T) {
	r := autoOpenRig(t)
	r.daily()
	base := r.rec.Base()
	assert.Equal(t, domain.DepsQueued, base.State, "the base batch is queued")
	assert.False(t, base.Manual)
	assert.Equal(t, r.at, base.QueuedAt)
	require.Len(t, r.rec.Batches, 2)
	assert.Equal(t, domain.DepsIdle, r.rec.Batches[1].State, "a major is never queued automatically")
	assert.NotEmpty(t, r.rec.Batches[1].Updates)
}

func TestBernardAutoOpenSkips(t *testing.T) {
	refs := []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.21.0"), Current: "v0.20.0", Kind: domain.DepMinor}}
	for _, c := range []struct {
		name  string
		setup func(*domain.RepoDeps, time.Time)
		want  domain.DepsState
	}{
		{"disabled", func(d *domain.RepoDeps, _ time.Time) { d.AutoOpenDisabled = true }, domain.DepsIdle},
		{"snoozed", func(d *domain.RepoDeps, now time.Time) { d.SnoozedUntil = now.Add(time.Hour) }, domain.DepsIdle},
		{"adopted", func(d *domain.RepoDeps, _ time.Time) { d.Batches = []domain.DepBatch{{Adopted: true, MR: 3}} }, domain.DepsIdle},
		{"open current MR", func(d *domain.RepoDeps, _ time.Time) {
			d.Batches = []domain.DepBatch{{MR: 3, MRVersions: []domain.DepRef{refs[0].DepRef}, MRUpdates: refs}}
		}, domain.DepsIdle},
		{"same failure", func(d *domain.RepoDeps, _ time.Time) {
			d.Batches = []domain.DepBatch{{State: domain.DepsFailed, Error: "tidy failed", RunUpdates: refs}}
		}, domain.DepsFailed},
		{"same failure after a run", func(d *domain.RepoDeps, _ time.Time) {
			d.Batches = []domain.DepBatch{{State: domain.DepsFailed, Error: "tidy failed", MR: 3, MRVersions: []domain.DepRef{refs[0].DepRef}, MRUpdates: refs}}
		}, domain.DepsFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := autoOpenRig(t)
			c.setup(&r.rec, time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC))
			r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.21.0"))}
			r.daily()
			assert.Equal(t, c.want, r.rec.Base().State)
		})
	}
	t.Run("failure with other versions is retried", func(t *testing.T) {
		r := autoOpenRig(t)
		r.rec.Batches = []domain.DepBatch{{State: domain.DepsFailed, Error: "tidy failed", RunUpdates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.5")}}}}
		r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.21.0"))}
		r.daily()
		assert.Equal(t, domain.DepsQueued, r.rec.Base().State)
		assert.Empty(t, r.rec.Base().Error)
	})
	t.Run("an outdated MR is queued", func(t *testing.T) {
		r := autoOpenRig(t)
		r.rec.Batches = []domain.DepBatch{{MR: 3, MRVersions: []domain.DepRef{depRef("a/mod", "v0.20.5")}}}
		r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.21.0"))}
		r.daily()
		assert.Equal(t, domain.DepsQueued, r.rec.Base().State)
	})
}

func TestBernardSetAutoOpen(t *testing.T) {
	r := autoOpenRig(t)
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	_, err := r.b.SetAutoOpen(t.Context(), domain.Principal{}, r.repo, false)
	require.ErrorIs(t, err, domain.ErrNotFound, "owner only")

	r.rec.Batches = []domain.DepBatch{{Updates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.21.0"), Current: "v0.20.0"}}}}
	got, err := r.b.SetAutoOpen(t.Context(), domain.Owner(), r.repo, false)
	require.NoError(t, err)
	assert.True(t, got.AutoOpenDisabled)
	assert.Equal(t, domain.DepsIdle, got.Base().State, "off queues nothing")
	assert.Equal(t, 1, r.changedCount())

	got, err = r.b.SetAutoOpen(t.Context(), domain.Owner(), r.repo, true)
	require.NoError(t, err)
	assert.False(t, got.AutoOpenDisabled)
	assert.Equal(t, domain.DepsQueued, got.Base().State, "on queues the due base batch")
	assert.False(t, got.Base().Manual)
	assert.Equal(t, 2, r.changedCount())
}
