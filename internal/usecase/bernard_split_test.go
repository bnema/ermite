// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestBernardSplit(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	otel := domain.DepUpdate{DepRef: depRef("go.opentelemetry.io/otel", "v2.0.0"), Current: "v1.0.0", New: true}
	sdk := domain.DepUpdate{DepRef: depRef("go.opentelemetry.io/otel/sdk", "v2.0.0"), Current: "v1.0.0"}
	family := domain.FamilyBatchKey(otel, nil)
	name := domain.DepName{Ecosystem: domain.EcosystemGoMod, Name: sdk.Name}
	r.rec.Batches = []domain.DepBatch{{}, {Key: family, Updates: []domain.DepUpdate{otel, sdk}, MR: 3}}
	ctx := t.Context()

	_, err := r.b.Split(ctx, domain.Principal{}, r.repo, name, true)
	require.ErrorIs(t, err, domain.ErrNotFound, "owner only")

	r.rec.Batches[1].State = domain.DepsQueued
	_, err = r.b.Split(ctx, domain.Owner(), r.repo, name, true)
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	r.rec.Batches[1].State = domain.DepsApplying
	_, err = r.b.Split(ctx, domain.Owner(), r.repo, name, true)
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	assert.Zero(t, r.changedCount())
	r.rec.Batches[1].State = domain.DepsIdle

	got, err := r.b.Split(ctx, domain.Owner(), r.repo, name, true)
	require.NoError(t, err)
	assert.Equal(t, []domain.DepName{name}, got.SplitOut)
	require.Len(t, got.Batches, 3)
	assert.Equal(t, family, got.Batches[1].Key)
	assert.Equal(t, int64(3), got.Batches[1].MR, "the family batch keeps its MR")
	assert.Equal(t, []domain.DepUpdate{otel}, got.Batches[1].Updates)
	assert.Equal(t, domain.FamilyBatchKey(sdk, got.SplitOut), got.Batches[2].Key)
	assert.Equal(t, []domain.DepUpdate{sdk}, got.Batches[2].Updates, "not marked new")
	assert.Equal(t, 1, r.changedCount())

	_, err = r.b.Split(ctx, domain.Owner(), r.repo, name, true)
	require.NoError(t, err)
	assert.Equal(t, 1, r.changedCount(), "no change, no event")

	got, err = r.b.Split(ctx, domain.Owner(), r.repo, name, false)
	require.NoError(t, err)
	assert.Empty(t, got.SplitOut)
	require.Len(t, got.Batches, 2)
	assert.Equal(t, []domain.DepUpdate{otel, sdk}, got.Batches[1].Updates)
	assert.Equal(t, 2, r.changedCount())
}

func TestBernardSplitRefusesBusyAndInvalid(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil).Maybe()
	sdk := domain.DepUpdate{DepRef: depRef("go.opentelemetry.io/otel/sdk", "v2.0.0"), Current: "v1.0.0"}
	name := domain.DepName{Ecosystem: domain.EcosystemGoMod, Name: sdk.Name}
	ctx := t.Context()
	for _, bad := range []domain.DepName{{Ecosystem: "cargo", Name: "x"}, {Ecosystem: domain.EcosystemGoMod}, {Ecosystem: domain.EcosystemNPM, Name: " x"}, {Ecosystem: domain.EcosystemNPM, Name: "a\nb"}} {
		_, err := r.b.Split(ctx, domain.Owner(), r.repo, bad, true)
		require.ErrorIs(t, err, domain.ErrInvalid, "%+v", bad)
	}
	// A drafted batch is busy; a legacy key holding the dependency in its
	// run or its merge request is found by what it holds.
	for _, busy := range []domain.DepBatch{
		{Key: domain.FamilyBatchKey(sdk, nil), Updates: []domain.DepUpdate{sdk}, State: domain.DepsDrafted},
		{Key: "gomod/go.opentelemetry.io/otel/sdk@v2", State: domain.DepsApplying, RunUpdates: []domain.DepUpdate{sdk}},
		{Key: "gomod/go.opentelemetry.io/otel/sdk@v2", State: domain.DepsQueued, MRUpdates: []domain.DepUpdate{sdk}, MR: 4},
	} {
		r.rec.Batches = []domain.DepBatch{{}, busy}
		_, err := r.b.Split(ctx, domain.Owner(), r.repo, name, true)
		require.ErrorIs(t, err, domain.ErrPreconditionFailed, "%+v", busy)
	}
	assert.Zero(t, r.changedCount())
}
