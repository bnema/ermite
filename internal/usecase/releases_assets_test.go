// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func (r *releaseRig) upload(t *testing.T, rel domain.Release, n string, data []byte) domain.ReleaseAsset {
	t.Helper()
	a, err := r.svc.UploadAsset(r.ctx, domain.Root(), r.repo, ports.UploadAssetParams{ReleaseID: rel.ID, Name: n, Size: int64(len(data))}, bytes.NewReader(data))
	require.NoError(t, err)
	return a
}
func TestReleaseAssetRoundTrip(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{AssetMax: 10, Quota: 10})
	rel := r.create(t, "v1", false)
	a := r.upload(t, rel, "binary", []byte("0123456789"))
	require.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("0123456789"))), a.Digest)
	usage, err := r.svc.Usage(r.ctx, domain.Anonymous(), r.repo)
	require.NoError(t, err)
	require.EqualValues(t, 10, usage.Used)
	for _, tc := range []struct {
		offset, length int64
		want           string
		err            error
	}{{0, -1, "0123456789", nil}, {3, 4, "3456", nil}, {9, 1, "9", nil}, {10, -1, "", domain.ErrOutOfRange}, {11, -1, "", domain.ErrOutOfRange}} {
		var got bytes.Buffer
		metadata := false
		err := r.svc.DownloadAsset(r.ctx, domain.Anonymous(), r.repo, ports.AssetRef{Latest: true, Name: a.Name}, tc.offset, tc.length, func(c ports.AssetChunk) error {
			if c.Asset != nil {
				require.False(t, metadata)
				metadata = true
				require.Equal(t, a.ID, c.Asset.ID)
			} else {
				require.True(t, metadata)
				got.Write(c.Data)
			}
			return nil
		})
		require.ErrorIs(t, err, tc.err)
		require.Equal(t, tc.want, got.String())
	}
	_, err = r.svc.UploadAsset(r.ctx, domain.Root(), r.repo, ports.UploadAssetParams{ReleaseID: rel.ID, Name: "large", Size: 11}, bytes.NewReader(nil))
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	_, err = r.svc.UploadAsset(r.ctx, domain.Root(), r.repo, ports.UploadAssetParams{ReleaseID: rel.ID, Name: "quota", Size: 1}, bytes.NewReader([]byte("x")))
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	_, err = r.svc.UploadAsset(r.ctx, domain.Root(), r.repo, ports.UploadAssetParams{ReleaseID: rel.ID, Name: a.Name, Size: 0}, bytes.NewReader(nil))
	require.ErrorIs(t, err, domain.ErrAlreadyExists)
	replacement, err := r.svc.UploadAsset(r.ctx, domain.Root(), r.repo, ports.UploadAssetParams{ReleaseID: rel.ID, Name: a.Name, Size: 2, Replace: true}, bytes.NewReader([]byte("hi")))
	require.NoError(t, err)
	require.NotEqual(t, a.ID, replacement.ID)
	_, err = r.blobs.Stat(r.ctx, a.BlobKey())
	require.ErrorIs(t, err, domain.ErrNotFound)
	renamed := "new"
	label := "label"
	replacement, err = r.svc.UpdateAsset(r.ctx, domain.Root(), r.repo, replacement.ID, ports.UpdateAssetParams{Name: &renamed, Label: &label})
	require.NoError(t, err)
	require.Equal(t, label, replacement.Label)
	list, err := r.svc.ListAssets(r.ctx, domain.Anonymous(), r.repo, rel.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, renamed, list[0].Name)
	require.NoError(t, r.svc.DeleteAsset(r.ctx, domain.Root(), r.repo, replacement.ID))
	usage, err = r.svc.Usage(r.ctx, domain.Root(), r.repo)
	require.NoError(t, err)
	require.Zero(t, usage.Used)
}
func TestReleaseAssetCountBoundary(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	rel := r.create(t, "v1", false)
	// Seed 999 records in one batch, then exercise the exact public boundary.
	used, v, err := r.store.Releases().ReleaseUsage(r.ctx, r.repo)
	require.NoError(t, err)
	w := ports.ReleaseWrite{Repo: r.repo, Used: used, UsageVersion: v}
	for range 999 {
		id, err := r.store.Releases().NextReleaseID(r.ctx)
		require.NoError(t, err)
		w.PutAssets = append(w.PutAssets, domain.ReleaseAsset{ID: id, ReleaseID: rel.ID, Repo: r.repo, Name: fmt.Sprint(id), State: domain.ReleaseAssetUploaded})
	}
	require.NoError(t, r.store.Releases().ApplyReleases(r.ctx, w))
	r.upload(t, rel, "thousandth", nil)
	_, err = r.svc.UploadAsset(r.ctx, domain.Root(), r.repo, ports.UploadAssetParams{ReleaseID: rel.ID, Name: "1001"}, bytes.NewReader(nil))
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
}
func TestReleaseAssetBadBodies(t *testing.T) {
	bodyErr := errors.New("reader failed")
	for _, size := range []int64{4, 9 << 20} {
		for _, kind := range []string{"short", "long", "error", "cancel"} {
			t.Run(fmt.Sprint(size, "/", kind), func(t *testing.T) {
				r := newReleaseRig(t, ports.ReleaseLimits{})
				rel := r.create(t, "v1", false)
				var body io.Reader
				switch kind {
				case "short":
					body = bytes.NewReader(make([]byte, size-1))
				case "long":
					body = bytes.NewReader(make([]byte, size+1))
				case "error":
					body = errReader{bodyErr}
				case "cancel":
					body = errReader{context.Canceled}
				}
				_, err := r.svc.UploadAsset(r.ctx, domain.Root(), r.repo, ports.UploadAssetParams{ReleaseID: rel.ID, Name: "bad", Size: size}, body)
				switch kind {
				case "error":
					require.ErrorIs(t, err, bodyErr)
				case "cancel":
					require.ErrorIs(t, err, context.Canceled)
				default:
					require.ErrorIs(t, err, domain.ErrInvalid)
				}
				all, err := r.store.Releases().AllAssets(r.ctx)
				require.NoError(t, err)
				require.Empty(t, all)
				u, err := r.svc.Usage(r.ctx, domain.Root(), r.repo)
				require.NoError(t, err)
				require.Zero(t, u.Used)
				require.NoError(t, r.blobs.List(r.ctx, "release/", func(ports.BlobInfo) error { t.Error("blob left behind"); return nil }))
				require.NoError(t, r.blobs.ListMultipart(r.ctx, "release/", func(ports.MultipartInfo) error { t.Error("multipart left behind"); return nil }))
			})
		}
	}
}

type releaseBlobSpy struct {
	ports.BlobStore
	put, multipart int
	parts          []int64
}

func (s *releaseBlobSpy) Put(ctx context.Context, key string, b io.Reader, size int64) (ports.BlobInfo, error) {
	s.put++
	return s.BlobStore.Put(ctx, key, b, size)
}
func (s *releaseBlobSpy) NewMultipart(ctx context.Context, key string) (ports.MultipartUpload, error) {
	s.multipart++
	u, e := s.BlobStore.NewMultipart(ctx, key)
	return &releasePartSpy{MultipartUpload: u, spy: s}, e
}

type releasePartSpy struct {
	ports.MultipartUpload
	spy *releaseBlobSpy
}

func (s *releasePartSpy) UploadPart(ctx context.Context, n int, b io.Reader, size int64) error {
	s.spy.parts = append(s.spy.parts, size)
	return s.MultipartUpload.UploadPart(ctx, n, b, size)
}
func TestReleaseAssetUploadPaths(t *testing.T) {
	for _, size := range []int{1 << 20, 9 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			r := newReleaseRig(t, ports.ReleaseLimits{})
			rel := r.create(t, "v1", false)
			spy := &releaseBlobSpy{BlobStore: r.blobs}
			bus := mocks.NewEventBus(t)
			bus.EXPECT().Publish(mock.Anything, mock.Anything).Return().Maybe()
			r.svc = usecase.NewReleases(r.store.Releases(), spy, r.store, r.reader, r.tags, r.pushes, bus, r.clock, ports.ReleaseLimits{})
			data := bytes.Repeat([]byte("z"), size)
			a := r.upload(t, rel, "blob", data)
			require.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(data)), a.Digest)
			if size == 1<<20 {
				require.Equal(t, 1, spy.put)
				require.Zero(t, spy.multipart)
			} else {
				require.Zero(t, spy.put)
				require.Equal(t, 1, spy.multipart)
				require.Equal(t, []int64{8 << 20, 1 << 20}, spy.parts)
			}
		})
	}
}
func TestReleaseAssetStaleReplacementAndReleaseDelete(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	rel := r.create(t, "v1", false)
	a := r.upload(t, rel, "pending", []byte("old"))
	current, err := r.store.Releases().GetAsset(r.ctx, a.ID)
	require.NoError(t, err)
	current.State = domain.ReleaseAssetUploading
	current.UpdatedAt = now.Add(-61 * time.Second)
	used, v, err := r.store.Releases().ReleaseUsage(r.ctx, r.repo)
	require.NoError(t, err)
	require.NoError(t, r.store.Releases().ApplyReleases(r.ctx, ports.ReleaseWrite{Repo: r.repo, Used: used, UsageVersion: v, PutAssets: []domain.ReleaseAsset{current}}))
	b := r.upload(t, rel, "pending", []byte("new"))
	require.NotEqual(t, a.ID, b.ID)
	require.NoError(t, r.svc.DeleteRelease(r.ctx, domain.Root(), r.repo, rel.ID))
	_, err = r.blobs.Stat(r.ctx, b.BlobKey())
	require.ErrorIs(t, err, domain.ErrNotFound)
	u, err := r.svc.Usage(r.ctx, domain.Root(), r.repo)
	require.NoError(t, err)
	require.Zero(t, u.Used)
}

type releaseBlockingReader struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (b releaseBlockingReader) Read([]byte) (int, error) {
	b.entered <- struct{}{}
	<-b.release
	return 0, io.EOF
}
func TestReleaseAssetConcurrencyCap(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	rel := r.create(t, "v1", false)
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			_, err := r.svc.UploadAsset(r.ctx, domain.Root(), r.repo, ports.UploadAssetParams{ReleaseID: rel.ID, Name: fmt.Sprint(i), Size: 1}, releaseBlockingReader{entered, release})
			errs <- err
		})
	}
	// Every reader has a reservation and holds its slot before the 17th call.
	for range 16 {
		<-entered
	}
	_, err := r.svc.UploadAsset(r.ctx, domain.Root(), r.repo, ports.UploadAssetParams{ReleaseID: rel.ID, Name: "excess", Size: 0}, bytes.NewReader(nil))
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.ErrorIs(t, err, domain.ErrInvalid)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestReleaseAssetDraftAccessAndRenameCollision(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	rel := r.create(t, "draft", true)
	a := r.upload(t, rel, "a", []byte("abc"))
	b := r.upload(t, rel, "b", nil)
	_, err := r.svc.GetAsset(r.ctx, domain.Anonymous(), r.repo, ports.AssetRef{ID: a.ID})
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = r.svc.ListAssets(r.ctx, domain.Anonymous(), r.repo, rel.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = r.svc.UpdateAsset(r.ctx, domain.Root(), r.repo, b.ID, ports.UpdateAssetParams{Name: &a.Name})
	require.ErrorIs(t, err, domain.ErrAlreadyExists)
	got, err := r.svc.GetAsset(r.ctx, domain.Root(), r.repo, ports.AssetRef{Tag: rel.Tag, Name: a.Name})
	require.NoError(t, err)
	require.Equal(t, a.ID, got.ID)
	assets, err := r.svc.AssetsOf(r.ctx, r.repo)
	require.NoError(t, err)
	require.Len(t, assets, 2)
	require.NoError(t, r.store.Delete(r.ctx, r.repo))
	r.svc.OnRepoDeleted(r.ctx, r.repo, assets)
	_, err = r.blobs.Stat(r.ctx, a.BlobKey())
	require.ErrorIs(t, err, domain.ErrNotFound)
	all, err := r.store.Releases().AllAssets(r.ctx)
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestReleaseAssetQuotaAcrossReleasesAndWrongRepository(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{AssetMax: 3, Quota: 5})
	a := r.create(t, "a", false)
	b := r.create(t, "b", false)
	r.upload(t, a, "a", []byte("abc"))
	r.upload(t, b, "b", []byte("de"))
	_, err := r.svc.UploadAsset(r.ctx, domain.Root(), r.repo, ports.UploadAssetParams{ReleaseID: b.ID, Name: "extra", Size: 1}, bytes.NewReader([]byte("x")))
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	other := name(t, "other")
	require.NoError(t, r.store.Create(r.ctx, domain.Repo{Name: other, Visibility: domain.VisibilityPublic, CreatedAt: now}))
	_, err = r.svc.UploadAsset(r.ctx, domain.Root(), other, ports.UploadAssetParams{ReleaseID: b.ID, Name: "wrong"}, bytes.NewReader(nil))
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
}
