// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"context"
	"io"
	"strings"
	"time"
)

// BlobStore keeps the contents of release assets and CI artifacts, never
// their metadata. Keys are built by the use cases from ids only ("a/b/c",
// ASCII, no empty segment, no ".", no ".." segment, no leading "/", ≤ 512
// bytes); an adapter rejects any other key with domain.ErrInvalid.
// *blobdisk.Store and *blobs3.Store implement it.
type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) (BlobInfo, error)              // size ≥ 0 required; replaces; a failure leaves no object
	Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, BlobInfo, error) // length < 0: to the end; offset ≥ size → domain.ErrOutOfRange
	Stat(ctx context.Context, key string) (BlobInfo, error)
	Delete(ctx context.Context, key string) error                           // absent: nil
	List(ctx context.Context, prefix string, fn func(BlobInfo) error) error // lexicographic; fn error stops and is returned
	NewMultipart(ctx context.Context, key string) (MultipartUpload, error)
	ListMultipart(ctx context.Context, prefix string, fn func(MultipartInfo) error) error
	AbortMultipart(ctx context.Context, key, id string) error // for GC of uploads listed by ListMultipart
	Check(ctx context.Context) error                          // startup probe; never creates anything
}
type BlobInfo struct {
	Key     string
	Size    int64
	Digest  string
	ModTime time.Time
} // Digest "sha256:<hex>" from Put, "" from Stat/List/Open on S3
type MultipartInfo struct {
	Key, ID string
	Started time.Time
}

// MultipartUpload uploads one object in parts of BlobMinPartSize or more
// (the last may be smaller), at most BlobMaxParts, numbered from 1 in the
// order given to Complete. Abort discards everything; Complete or Abort
// must be called exactly once.
type MultipartUpload interface {
	ID() string
	UploadPart(ctx context.Context, n int, r io.Reader, size int64) error
	Complete(ctx context.Context) (BlobInfo, error) // parts in number order; Digest "" (the caller hashes)
	Abort(ctx context.Context) error
}

const (
	BlobMinPartSize = 5 << 20
	BlobPartSize    = 8 << 20
	BlobMaxParts    = 10_000
)

// ValidBlobKey reports whether key follows the rule above.
func ValidBlobKey(key string) bool {
	if len(key) == 0 || len(key) > 512 {
		return false
	}
	for _, c := range []byte(key) {
		if c > 127 {
			return false
		}
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
