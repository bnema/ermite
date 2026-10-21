// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// artifactBlobMock uses generated port mocks with byte storage in callbacks.
// Real-disk end-to-end behavior is exercised by internal/app's artifact rig.
func artifactBlobMock(t *testing.T, failPart bool) (*mocks.BlobStore, *int, *int) {
	t.Helper()
	b := mocks.NewBlobStore(t)
	objects := map[string][]byte{}
	active := map[string]ports.MultipartInfo{}
	puts, multiparts := new(int), new(int)
	b.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, key string, r io.Reader, size int64) (ports.BlobInfo, error) {
		*puts++
		data, err := io.ReadAll(r)
		if err != nil {
			return ports.BlobInfo{}, err
		}
		if int64(len(data)) != size {
			return ports.BlobInfo{}, domain.ErrInvalid
		}
		objects[key] = data
		return ports.BlobInfo{Key: key, Size: size}, nil
	}).Maybe()
	b.EXPECT().Open(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, key string, offset, length int64) (io.ReadCloser, ports.BlobInfo, error) {
		data, ok := objects[key]
		if !ok {
			return nil, ports.BlobInfo{}, domain.ErrNotFound
		}
		info := ports.BlobInfo{Key: key, Size: int64(len(data))}
		if offset >= info.Size {
			return nil, info, domain.ErrOutOfRange
		}
		end := info.Size
		if length >= 0 {
			end = min(end, offset+length)
		}
		return io.NopCloser(bytes.NewReader(data[offset:end])), info, nil
	}).Maybe()
	b.EXPECT().Stat(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, key string) (ports.BlobInfo, error) {
		data, ok := objects[key]
		if !ok {
			return ports.BlobInfo{}, domain.ErrNotFound
		}
		return ports.BlobInfo{Key: key, Size: int64(len(data))}, nil
	}).Maybe()
	b.EXPECT().Delete(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, key string) error { delete(objects, key); return nil }).Maybe()
	b.EXPECT().List(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, prefix string, fn func(ports.BlobInfo) error) error {
		for key, data := range objects {
			if strings.HasPrefix(key, prefix) {
				if err := fn(ports.BlobInfo{Key: key, Size: int64(len(data))}); err != nil {
					return err
				}
			}
		}
		return nil
	}).Maybe()
	b.EXPECT().ListMultipart(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, prefix string, fn func(ports.MultipartInfo) error) error {
		for key, m := range active {
			if strings.HasPrefix(key, prefix) {
				if err := fn(m); err != nil {
					return err
				}
			}
		}
		return nil
	}).Maybe()
	b.EXPECT().AbortMultipart(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, key, id string) error { delete(active, key); return nil }).Maybe()
	b.EXPECT().NewMultipart(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, key string) (ports.MultipartUpload, error) {
		*multiparts++
		active[key] = ports.MultipartInfo{Key: key, ID: "upload"}
		u := mocks.NewMultipartUpload(t)
		var data []byte
		u.EXPECT().UploadPart(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, _ int, r io.Reader, size int64) error {
			if failPart {
				return errors.New("part failed")
			}
			part, err := io.ReadAll(r)
			data = append(data, part...)
			return err
		}).Maybe()
		u.EXPECT().Complete(mock.Anything).RunAndReturn(func(context.Context) (ports.BlobInfo, error) {
			objects[key] = data
			delete(active, key)
			return ports.BlobInfo{Key: key, Size: int64(len(data))}, nil
		}).Maybe()
		u.EXPECT().Abort(mock.Anything).RunAndReturn(func(context.Context) error { delete(active, key); return nil }).Maybe()
		return u, nil
	}).Maybe()
	return b, puts, multiparts
}

func TestArtifactBlobAssembly(t *testing.T) {
	for _, size := range []int{0, 1, ports.BlobPartSize, ports.BlobPartSize + 1, 2*ports.BlobPartSize + 17} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ctx := t.Context()
			b, puts, multiparts := artifactBlobMock(t, false)
			c := &CI{cfg: CIConfig{Blobs: b}}
			a := domain.CIArtifact{RunID: 1, ID: 2, Store: domain.CIArtifactStoreBlob}
			data := bytes.Repeat([]byte("x"), size)
			// Parts straddle block boundaries, with an empty block in the list.
			cut := min(size, 3<<20)
			_, err := b.Put(ctx, a.BlockKey("a"), bytes.NewReader(data[:cut]), int64(cut))
			require.NoError(t, err)
			_, err = b.Put(ctx, a.BlockKey("b"), bytes.NewReader(data[cut:]), int64(size-cut))
			require.NoError(t, err)
			_, err = b.Put(ctx, a.BlockKey("empty"), bytes.NewReader(nil), 0)
			require.NoError(t, err)
			*puts = 0
			n, digest, err := c.assembleBlobs(ctx, a, []string{"a", "empty", "b"}, int64(size))
			require.NoError(t, err)
			assert.EqualValues(t, size, n)
			sum := sha256.Sum256(data)
			assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), digest)
			if size <= ports.BlobPartSize {
				assert.Equal(t, 1, *puts)
				assert.Zero(t, *multiparts)
			} else {
				assert.Zero(t, *puts)
				assert.Equal(t, 1, *multiparts)
			}
			if size > 0 {
				r, _, err := b.Open(ctx, a.BlobKey(), 0, -1)
				require.NoError(t, err)
				got, err := io.ReadAll(r)
				require.NoError(t, err)
				require.NoError(t, r.Close())
				assert.Equal(t, data, got)
			} else {
				info, err := b.Stat(ctx, a.BlobKey())
				require.NoError(t, err)
				assert.Zero(t, info.Size)
			}
			// No legacy reader is needed for a blob download, including EOF resumes.
			a.Size = int64(size)
			var got []byte
			require.NoError(t, c.streamArtifact(ctx, a, 0, func(ch ports.ArtifactChunk) error { got = append(got, ch.Data...); return nil }))
			assert.Equal(t, data, append([]byte{}, got...))
			require.NoError(t, c.streamArtifact(ctx, a, a.Size, func(ports.ArtifactChunk) error { return nil }))
			c.removeArtifactFiles(ctx, []domain.CIArtifact{a})
			_, err = b.Stat(ctx, a.BlobKey())
			assert.ErrorIs(t, err, domain.ErrNotFound)
			require.NoError(t, b.List(ctx, a.BlobKey()+".blocks/", func(ports.BlobInfo) error { t.Error("block survived deletion"); return nil }))
		})
	}
}

func TestArtifactBlobAssemblyFailureAborts(t *testing.T) {
	for _, failPart := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing block", true: "failed part"}[failPart], func(t *testing.T) {
			b, _, multiparts := artifactBlobMock(t, failPart)
			c := &CI{cfg: CIConfig{Blobs: b}}
			a := domain.CIArtifact{RunID: 1, ID: 2}
			data := bytes.Repeat([]byte("a"), ports.BlobPartSize)
			_, err := b.Put(t.Context(), a.BlockKey("a"), bytes.NewReader(data), int64(len(data)))
			require.NoError(t, err)
			_, _, err = c.assembleBlobs(t.Context(), a, []string{"a", "missing"}, ports.BlobPartSize+1)
			require.Error(t, err)
			assert.Equal(t, 1, *multiparts)
			_, err = b.Stat(t.Context(), a.BlobKey())
			assert.ErrorIs(t, err, domain.ErrNotFound)
			require.NoError(t, b.ListMultipart(t.Context(), "ci/", func(ports.MultipartInfo) error { t.Error("multipart survived failure"); return nil }))
		})
	}
}

func TestArtifactPutUnsizedBound(t *testing.T) {
	b, _, _ := artifactBlobMock(t, false)
	c := &CI{cfg: CIConfig{Blobs: b}}
	for _, size := range []int{0, 1, domain.CIArtifactMaxBlockSize, domain.CIArtifactMaxBlockSize + 1} {
		n, err := c.putUnsized(t.Context(), "ci/1/2", bytes.NewReader(make([]byte, size)))
		if size > domain.CIArtifactMaxBlockSize {
			assert.ErrorIs(t, err, domain.ErrInvalid)
			assert.Contains(t, err.Error(), "a block has at most 8 MiB")
		} else {
			require.NoError(t, err)
			assert.EqualValues(t, size, n)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.putUnsized(ctx, "ci/1/cancelled", bytes.NewReader([]byte("x")))
	assert.ErrorIs(t, err, context.Canceled)
	_, err = b.Stat(t.Context(), "ci/1/cancelled")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestArtifactLegacyStream(t *testing.T) {
	a := domain.CIArtifact{RunID: 1, ID: 2, Size: 6}
	legacy := mocks.NewCIArtifactFiles(t)
	legacy.EXPECT().Open(mock.Anything, artifactRef(a), int64(2)).RunAndReturn(func(context.Context, ports.CIArtifactRef, int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("gacy"))), nil
	}).Twice()
	c := &CI{cfg: CIConfig{ArtifactFiles: legacy}}
	for _, store := range []domain.CIArtifactStore{domain.CIArtifactStoreLegacy, "disk"} {
		a.Store = store
		var got []byte
		require.NoError(t, c.streamArtifact(t.Context(), a, 2, func(ch ports.ArtifactChunk) error { got = append(got, ch.Data...); return nil }))
		assert.Equal(t, "gacy", string(got))
	}
	c.cfg.ArtifactFiles = nil
	assert.ErrorIs(t, c.streamArtifact(t.Context(), a, 0, func(ports.ArtifactChunk) error { return nil }), domain.ErrNotFound)
}

func TestArtifactBlobOrphanSweep(t *testing.T) {
	ctx := t.Context()
	b := mocks.NewBlobStore(t)
	s := mocks.NewCIArtifactStore(t)
	c := &CI{cfg: CIConfig{Blobs: b, Artifacts: s, Clock: stillClock{}}}
	now := c.now()
	old := now.Add(-25 * time.Hour)
	pending := domain.CIArtifact{RunID: 1, ID: 2, Store: domain.CIArtifactStoreBlob, State: domain.CIArtifactPending}
	committed := pending
	committed.ID = 3
	committed.State = domain.CIArtifactCommitted
	missing := pending
	missing.ID = 4
	b.EXPECT().List(mock.Anything, "ci/", mock.Anything).RunAndReturn(func(_ context.Context, _ string, fn func(ports.BlobInfo) error) error {
		for _, info := range []ports.BlobInfo{
			{Key: missing.BlobKey(), ModTime: now}, // young: no lookup, no deletion
			{Key: missing.BlobKey(), ModTime: old},
			{Key: pending.BlockKey("a"), ModTime: old},
			{Key: committed.BlockKey("a"), ModTime: old},
			{Key: committed.BlobKey(), ModTime: old},
		} {
			require.NoError(t, fn(info))
		}
		return nil
	})
	s.EXPECT().GetArtifact(mock.Anything, missing.ID).Return(domain.CIArtifact{}, domain.ErrNotFound).Once()
	s.EXPECT().GetArtifact(mock.Anything, pending.ID).Return(pending, nil).Once()
	s.EXPECT().GetArtifact(mock.Anything, committed.ID).Return(committed, nil).Twice()
	b.EXPECT().Delete(mock.Anything, missing.BlobKey()).Return(nil).Once()
	b.EXPECT().Delete(mock.Anything, committed.BlockKey("a")).Return(nil).Once()
	b.EXPECT().ListMultipart(mock.Anything, "ci/", mock.Anything).RunAndReturn(func(_ context.Context, _ string, fn func(ports.MultipartInfo) error) error {
		require.NoError(t, fn(ports.MultipartInfo{Key: missing.BlobKey(), ID: "old", Started: old}))
		require.NoError(t, fn(ports.MultipartInfo{Key: missing.BlobKey(), ID: "young", Started: now}))
		return nil
	})
	b.EXPECT().AbortMultipart(mock.Anything, missing.BlobKey(), "old").Return(nil).Once()
	c.sweepArtifactBlobOrphans(ctx)
}

func TestArtifactRemoveRunsWithoutLegacyReader(t *testing.T) {
	ctx := t.Context()
	b, _, _ := artifactBlobMock(t, false)
	c := &CI{cfg: CIConfig{Blobs: b}}
	a := domain.CIArtifact{RunID: 1, ID: 2}
	other := a
	other.RunID = 2
	for _, key := range []string{a.BlobKey(), a.BlockKey("a"), other.BlobKey()} {
		_, err := b.Put(ctx, key, bytes.NewReader([]byte("x")), 1)
		require.NoError(t, err)
	}
	_, err := b.NewMultipart(ctx, a.BlobKey())
	require.NoError(t, err)
	require.NoError(t, c.removeArtifactRuns(ctx, []int64{1}))
	_, err = b.Stat(ctx, a.BlobKey())
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = b.Stat(ctx, a.BlockKey("a"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = b.Stat(ctx, other.BlobKey())
	require.NoError(t, err)
	require.NoError(t, b.ListMultipart(ctx, "ci/", func(ports.MultipartInfo) error { t.Error("upload survived run deletion"); return nil }))
}

func TestArtifactBlobIDs(t *testing.T) {
	a := domain.CIArtifact{RunID: 7, ID: 42}
	for _, key := range []string{a.BlobKey(), a.BlockKey("abcdef.0123")} {
		run, id, block, ok := artifactBlobIDs(key)
		assert.True(t, ok)
		assert.EqualValues(t, 7, run)
		assert.EqualValues(t, 42, id)
		assert.Equal(t, strings.Contains(key, ".blocks/"), block)
	}
	for _, key := range []string{"release/1", "ci/1/2", a.BlobKey() + ".blocks/", a.BlobKey() + "/wrong"} {
		_, _, _, ok := artifactBlobIDs(key)
		assert.False(t, ok, key)
	}
}
