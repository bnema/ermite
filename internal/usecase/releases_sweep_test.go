// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func (r *releaseRig) setClock(t *testing.T, at time.Time, blobs ports.BlobStore) {
	t.Helper()
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(at).Maybe()
	bus := mocks.NewEventBus(t)
	bus.EXPECT().Publish(mock.Anything, mock.Anything).Return().Maybe()
	r.clock = clock
	r.svc = usecase.NewReleases(r.store.Releases(), blobs, r.store, r.reader, r.tags, r.pushes, bus, clock, ports.ReleaseLimits{})
}
func TestReleaseSweepPendingAndOrphans(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	rel := r.create(t, "v1", false)
	pending := r.upload(t, rel, "pending", []byte("abc"))
	good := r.upload(t, rel, "good", []byte("xyz"))
	fresh := r.upload(t, rel, "fresh", nil)
	old, err := r.store.Releases().GetAsset(r.ctx, pending.ID)
	require.NoError(t, err)
	old.State = domain.ReleaseAssetUploading
	mp, err := r.blobs.NewMultipart(r.ctx, old.BlobKey())
	require.NoError(t, err)
	old.MultipartID = mp.ID()
	fresh.State = domain.ReleaseAssetUploading
	fresh.UpdatedAt = time.Now().Add(25 * time.Hour)
	used, v, err := r.store.Releases().ReleaseUsage(r.ctx, r.repo)
	require.NoError(t, err)
	require.NoError(t, r.store.Releases().ApplyReleases(r.ctx, ports.ReleaseWrite{Repo: r.repo, Used: used, UsageVersion: v, PutAssets: []domain.ReleaseAsset{old, fresh}}))
	_, err = r.blobs.Put(r.ctx, "release/99999999999999999999", bytes.NewReader([]byte("orphan")), 6)
	require.NoError(t, err)
	// The clock is advanced beyond the disk adapter's real modification times.
	at := time.Now().Add(26 * time.Hour)
	r.setClock(t, at, r.blobs)
	r.svc.Sweep(r.ctx)
	_, err = r.store.Releases().GetAsset(r.ctx, pending.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = r.blobs.Stat(r.ctx, pending.BlobKey())
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = r.blobs.Stat(r.ctx, "release/99999999999999999999")
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = r.store.Releases().GetAsset(r.ctx, good.ID)
	require.NoError(t, err)
	// The pending record inside the grace period is retained.
	_, err = r.store.Releases().GetAsset(r.ctx, fresh.ID)
	require.NoError(t, err)
	require.NoError(t, r.blobs.ListMultipart(r.ctx, "release/", func(ports.MultipartInfo) error { t.Error("multipart survived"); return nil }))
}
func TestReleaseSweepMissingBlobTwoObservations(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	rel := r.create(t, "v1", false)
	a := r.upload(t, rel, "binary", []byte("abc"))
	require.NoError(t, r.blobs.Delete(r.ctx, a.BlobKey()))
	r.svc.Sweep(r.ctx)
	_, err := r.store.Releases().GetAsset(r.ctx, a.ID)
	require.NoError(t, err)
	r.svc.Sweep(r.ctx)
	_, err = r.store.Releases().GetAsset(r.ctx, a.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
	usage, err := r.svc.Usage(r.ctx, domain.Root(), r.repo)
	require.NoError(t, err)
	require.Zero(t, usage.Used)
}

type releaseStatOverride struct {
	ports.BlobStore
	err error
}

func (b *releaseStatOverride) Stat(ctx context.Context, key string) (ports.BlobInfo, error) {
	if b.err != nil {
		return ports.BlobInfo{}, b.err
	}
	return b.BlobStore.Stat(ctx, key)
}
func TestReleaseSweepTransientStatAndRecovery(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient preserves miss", true: "success resets miss"}[recover], func(t *testing.T) {
			r := newReleaseRig(t, ports.ReleaseLimits{})
			rel := r.create(t, "v1", false)
			a := r.upload(t, rel, "binary", []byte("abc"))
			b := &releaseStatOverride{BlobStore: r.blobs, err: domain.ErrNotFound}
			r.setClock(t, now, b)
			r.svc.Sweep(r.ctx)
			b.err = errors.New("outage")
			r.svc.Sweep(r.ctx)
			_, err := r.store.Releases().GetAsset(r.ctx, a.ID)
			require.NoError(t, err)
			if recover {
				b.err = nil
				r.svc.Sweep(r.ctx)
			}
			b.err = domain.ErrNotFound
			r.svc.Sweep(r.ctx)
			_, err = r.store.Releases().GetAsset(r.ctx, a.ID)
			if recover {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, domain.ErrNotFound)
			}
		})
	}
}
func TestReleaseSweepGraceAndFailureIsolation(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	rel := r.create(t, "v1", false)
	a := r.upload(t, rel, "pending", nil)
	a.State = domain.ReleaseAssetUploading
	used, v, err := r.store.Releases().ReleaseUsage(r.ctx, r.repo)
	require.NoError(t, err)
	require.NoError(t, r.store.Releases().ApplyReleases(r.ctx, ports.ReleaseWrite{Repo: r.repo, Used: used, UsageVersion: v, PutAssets: []domain.ReleaseAsset{a}}))
	b := mocks.NewBlobStore(t)
	b.EXPECT().List(r.ctx, "release/", mock.Anything).RunAndReturn(func(_ context.Context, _ string, fn func(ports.BlobInfo) error) error {
		require.NoError(t, fn(ports.BlobInfo{Key: "release/old", ModTime: now.Add(-25 * time.Hour)}))
		require.NoError(t, fn(ports.BlobInfo{Key: "release/new", ModTime: now.Add(23 * time.Hour)}))
		return nil
	}).Once()
	b.EXPECT().Delete(r.ctx, "release/old").Return(errors.New("failed deletion")).Once()
	b.EXPECT().ListMultipart(r.ctx, "release/", mock.Anything).RunAndReturn(func(_ context.Context, _ string, fn func(ports.MultipartInfo) error) error {
		require.NoError(t, fn(ports.MultipartInfo{Key: "release/old", ID: "old", Started: now.Add(-25 * time.Hour)}))
		require.NoError(t, fn(ports.MultipartInfo{Key: "release/new", ID: "new", Started: now}))
		return nil
	}).Once()
	b.EXPECT().AbortMultipart(r.ctx, "release/old", "old").Return(nil).Once()
	r.setClock(t, now.Add(24*time.Hour), b)
	// Use times relative to the sweep, keeping both new objects within grace.
	// pending exactly 24h old survives; failures in orphan deletion do not stop multipart GC.
	r.svc.Sweep(r.ctx)
	_, err = r.store.Releases().GetAsset(r.ctx, a.ID)
	require.NoError(t, err)
}
