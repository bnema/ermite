// SPDX-License-Identifier: AGPL-3.0-or-later

// Package blobtest provides the shared BlobStore contract suite.
package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/require"
)

// Option adapts Run to a provider quirk.
type Option func(*options)

type options struct{ exactKeyUploadList bool }

// ExactKeyUploadList is for providers (MinIO) that list unfinished multipart
// uploads only when the prefix is a whole object key: the suite then lists by
// key instead of by directory prefix.
func ExactKeyUploadList() Option { return func(o *options) { o.exactKeyUploadList = true } }

// Run checks the blob contract against a fresh store for each subtest.
func Run(t *testing.T, newStore func(*testing.T) ports.BlobStore, opts ...Option) {
	ctx := context.Background()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	// uploadPrefix is the ListMultipart prefix for key: dir, or key itself
	// on a provider that lists by whole key only.
	uploadPrefix := func(dir, key string) string {
		if o.exactKeyUploadList {
			return key
		}
		return dir
	}
	t.Run("roundtrip", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Check(ctx))
		data := []byte("abcdefghij")
		info, e := s.Put(ctx, "ci/roundtrip", bytes.NewReader(data), int64(len(data)))
		require.NoError(t, e)
		h := sha256.Sum256(data)
		require.Equal(t, "sha256:"+hex.EncodeToString(h[:]), info.Digest)
		info, e = s.Stat(ctx, "ci/roundtrip")
		require.NoError(t, e)
		require.EqualValues(t, len(data), info.Size)
		require.False(t, info.ModTime.IsZero())
		for _, r := range []struct {
			off, len int64
			want     string
		}{{0, -1, "abcdefghij"}, {0, 3, "abc"}, {3, 4, "defg"}, {9, -1, "j"}, {3, 0, ""}} {
			rc, meta, e := s.Open(ctx, "ci/roundtrip", r.off, r.len)
			require.NoError(t, e)
			b, e := io.ReadAll(rc)
			require.NoError(t, e)
			require.NoError(t, rc.Close())
			require.Equal(t, r.want, string(b))
			require.EqualValues(t, 10, meta.Size)
		}
		for _, off := range []int64{10, 11} {
			_, _, e := s.Open(ctx, "ci/roundtrip", off, -1)
			require.ErrorIs(t, e, domain.ErrOutOfRange)
		}
		_, e = s.Put(ctx, "ci/roundtrip", strings.NewReader("new"), 3)
		require.NoError(t, e)
		rc, _, e := s.Open(ctx, "ci/roundtrip", 0, -1)
		require.NoError(t, e)
		b, e := io.ReadAll(rc)
		require.NoError(t, e)
		rc.Close()
		require.Equal(t, "new", string(b))
		require.NoError(t, s.Delete(ctx, "ci/roundtrip"))
		require.NoError(t, s.Delete(ctx, "ci/roundtrip"))
		_, e = s.Stat(ctx, "ci/roundtrip")
		require.ErrorIs(t, e, domain.ErrNotFound)
		_, _, e = s.Open(ctx, "ci/absent", 0, -1)
		require.ErrorIs(t, e, domain.ErrNotFound)
	})
	t.Run("list_and_reserved_names", func(t *testing.T) {
		s := newStore(t)
		keys := []string{"a.parts/b", "a/.tmp-object", "ci/z", "ci/a"}
		for _, k := range keys {
			_, e := s.Put(ctx, k, strings.NewReader(k), int64(len(k)))
			require.NoError(t, e)
			r, _, e := s.Open(ctx, k, 0, -1)
			require.NoError(t, e)
			b, e := io.ReadAll(r)
			r.Close()
			require.NoError(t, e)
			require.Equal(t, k, string(b))
		}
		var got []string
		require.NoError(t, s.List(ctx, "", func(i ports.BlobInfo) error { got = append(got, i.Key); return nil }))
		require.Equal(t, []string{"a.parts/b", "a/.tmp-object", "ci/a", "ci/z"}, got)
		got = nil
		require.NoError(t, s.List(ctx, "ci/", func(i ports.BlobInfo) error { got = append(got, i.Key); return nil }))
		require.Equal(t, []string{"ci/a", "ci/z"}, got)
		stop := errors.New("stop")
		calls := 0
		require.ErrorIs(t, s.List(ctx, "ci/", func(ports.BlobInfo) error { calls++; return stop }), stop)
		require.Equal(t, 1, calls)
	})
	t.Run("invalid", func(t *testing.T) {
		s := newStore(t)
		for _, key := range []string{"", "/a", "a//b", "a/../b", "a/./b", "é", strings.Repeat("a", 513)} {
			_, e := s.Put(ctx, key, strings.NewReader("x"), 1)
			require.ErrorIs(t, e, domain.ErrInvalid)
			_, e = s.Stat(ctx, key)
			require.ErrorIs(t, e, domain.ErrInvalid)
			_, _, e = s.Open(ctx, key, 0, -1)
			require.ErrorIs(t, e, domain.ErrInvalid)
			require.ErrorIs(t, s.Delete(ctx, key), domain.ErrInvalid)
			_, e = s.NewMultipart(ctx, key)
			require.ErrorIs(t, e, domain.ErrInvalid)
			require.ErrorIs(t, s.AbortMultipart(ctx, key, "id"), domain.ErrInvalid)
		}
		_, e := s.Put(ctx, "ci/negative", strings.NewReader(""), -1)
		require.ErrorIs(t, e, domain.ErrInvalid)
	})
	t.Run("multipart", func(t *testing.T) {
		s := newStore(t)
		u, e := s.NewMultipart(ctx, "ci/multi")
		require.NoError(t, e)
		var all []byte
		for n := 1; n <= 4; n++ {
			size := ports.BlobMinPartSize
			if n == 4 {
				size = 1024
			}
			data := bytes.Repeat([]byte{byte(n)}, size)
			all = append(all, data...)
			require.NoError(t, u.UploadPart(ctx, n, bytes.NewReader(data), int64(size)))
		}
		info, e := u.Complete(ctx)
		require.NoError(t, e)
		require.Empty(t, info.Digest)
		r, _, e := s.Open(ctx, "ci/multi", 0, -1)
		require.NoError(t, e)
		b, e := io.ReadAll(r)
		r.Close()
		require.NoError(t, e)
		require.Equal(t, all, b)
	})
	t.Run("abort", func(t *testing.T) {
		s := newStore(t)
		for _, byID := range []bool{false, true} {
			u, e := s.NewMultipart(ctx, "ci/abort")
			require.NoError(t, e)
			require.NoError(t, u.UploadPart(ctx, 1, strings.NewReader("x"), 1))
			var uploads []ports.MultipartInfo
			require.NoError(t, s.ListMultipart(ctx, uploadPrefix("ci/", "ci/abort"), func(i ports.MultipartInfo) error { uploads = append(uploads, i); return nil }))
			require.Len(t, uploads, 1)
			require.Equal(t, u.ID(), uploads[0].ID)
			require.Equal(t, "ci/abort", uploads[0].Key)
			require.False(t, uploads[0].Started.IsZero())
			if byID {
				require.NoError(t, s.AbortMultipart(ctx, "ci/abort", u.ID()))
			} else {
				require.NoError(t, u.Abort(ctx))
			}
			_, e = s.Stat(ctx, "ci/abort")
			require.ErrorIs(t, e, domain.ErrNotFound)
			uploads = nil
			require.NoError(t, s.ListMultipart(ctx, uploadPrefix("", "ci/abort"), func(i ports.MultipartInfo) error { uploads = append(uploads, i); return nil }))
			require.Empty(t, uploads)
		}
	})
	t.Run("undersized_part", func(t *testing.T) {
		s := newStore(t)
		u, e := s.NewMultipart(ctx, "ci/small")
		require.NoError(t, e)
		require.NoError(t, u.UploadPart(ctx, 1, strings.NewReader("x"), 1))
		require.NoError(t, u.UploadPart(ctx, 2, strings.NewReader("y"), 1))
		_, e = u.Complete(ctx)
		require.ErrorIs(t, e, domain.ErrInvalid)
		_, e = s.Stat(ctx, "ci/small")
		require.ErrorIs(t, e, domain.ErrNotFound)
	})
	t.Run("failed_put", func(t *testing.T) {
		s := newStore(t)
		_, e := s.Put(ctx, "ci/fail", io.MultiReader(strings.NewReader("abc"), failReader{}), 10)
		require.Error(t, e)
		_, e = s.Stat(ctx, "ci/fail")
		require.ErrorIs(t, e, domain.ErrNotFound)
		for _, v := range []struct {
			data string
			size int64
		}{{"a", 2}, {"abc", 2}} {
			_, e = s.Put(ctx, "ci/size", strings.NewReader(v.data), v.size)
			require.ErrorIs(t, e, domain.ErrInvalid)
			_, e = s.Stat(ctx, "ci/size")
			require.ErrorIs(t, e, domain.ErrNotFound)
		}
	})
	t.Run("large_put", func(t *testing.T) {
		s := newStore(t)
		data := bytes.Repeat([]byte("large"), ports.BlobPartSize/5+100)
		info, e := s.Put(ctx, "ci/large", bytes.NewBuffer(data), int64(len(data)))
		require.NoError(t, e)
		h := sha256.Sum256(data)
		require.Equal(t, "sha256:"+hex.EncodeToString(h[:]), info.Digest)
		r, _, e := s.Open(ctx, "ci/large", 0, -1)
		require.NoError(t, e)
		got, e := io.ReadAll(r)
		require.NoError(t, e)
		require.NoError(t, r.Close())
		require.Equal(t, data, got)
		for _, size := range []int64{int64(len(data) - 1), int64(len(data) + 1)} {
			_, e = s.Put(ctx, "ci/badlarge", bytes.NewBuffer(data), size)
			require.ErrorIs(t, e, domain.ErrInvalid)
			_, e = s.Stat(ctx, "ci/badlarge")
			require.ErrorIs(t, e, domain.ErrNotFound)
		}
		require.NoError(t, s.ListMultipart(ctx, uploadPrefix("", "ci/badlarge"), func(ports.MultipartInfo) error { t.Error("unfinished upload after Put"); return nil }))
	})

	t.Run("cancel_put", func(t *testing.T) {
		s := newStore(t)
		c, cancel := context.WithCancel(ctx)
		defer cancel()
		r := &cancelReader{cancel: cancel}
		_, e := s.Put(c, "ci/cancel", r, 1024)
		require.ErrorIs(t, e, context.Canceled)
		_, e = s.Stat(ctx, "ci/cancel")
		require.ErrorIs(t, e, domain.ErrNotFound)
	})
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("reader failed") }

type cancelReader struct {
	cancel context.CancelFunc
	done   bool
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, context.Canceled
	}
	r.done = true
	r.cancel()
	p[0] = 'x'
	return 1, nil
}
