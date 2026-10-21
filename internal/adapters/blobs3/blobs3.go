// SPDX-License-Identifier: AGPL-3.0-or-later

// Package blobs3 implements the blob port with the object, multipart and
// listing S3 APIs. Checksums are WhenRequired, credentials are explicit, and
// no bucket creation, policy, ACL, tagging or lifecycle API is ever called.
package blobs3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bnema/zerowrap"
)

// Store is a provider-independent S3 blob store.
type Store struct {
	client *s3.Client
	cfg    Config
}

var _ ports.BlobStore = (*Store)(nil)

// New builds a client without consulting ambient configuration or probing.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if cfg.Bucket == "" || cfg.Region == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("bucket, region and explicit credentials are required: %w", domain.ErrInvalid)
	}
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	if cfg.Prefix != "" {
		cfg.Prefix += "/"
	}
	client := cfg.HTTPClient
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.IdleConnTimeout = 60 * time.Second
		transport.ResponseHeaderTimeout = 60 * time.Second
		client = &http.Client{Transport: transport}
	}
	ac := aws.Config{Region: cfg.Region, Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""), HTTPClient: client, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired, RetryMaxAttempts: 3}
	c := s3.NewFromConfig(ac, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
	})
	return &Store{client: c, cfg: cfg}, nil
}
func valid(key string) error {
	if !ports.ValidBlobKey(key) {
		return domain.ErrInvalid
	}
	return nil
}
func (s *Store) key(key string) *string { return aws.String(s.cfg.Prefix + key) }

type statusError interface{ HTTPStatusCode() int }
type codeError interface{ ErrorCode() string }

func status(err error) int {
	var e statusError
	if errors.As(err, &e) {
		return e.HTTPStatusCode()
	}
	return 0
}
func translate(err error) error {
	if err == nil {
		return nil
	}
	var c codeError
	if errors.As(err, &c) {
		switch c.ErrorCode() {
		case "InvalidRange":
			return domain.ErrOutOfRange
		case "NoSuchKey", "NotFound":
			return domain.ErrNotFound
		case "EntityTooSmall":
			return domain.ErrInvalid
		}
	}
	switch status(err) {
	case 404:
		return domain.ErrNotFound
	case 416:
		return domain.ErrOutOfRange
	}
	return err
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}

// One pooled buffer per in-flight upload bounds staging to BlobPartSize.
var buffers = sync.Pool{New: func() any { b := make([]byte, ports.BlobPartSize); return &b }}

func exactEnd(ctx context.Context, r io.Reader) error {
	var b [1]byte
	n, e := io.ReadFull(ctxReader{ctx, r}, b[:])
	if n != 0 {
		return domain.ErrInvalid
	}
	if e == io.EOF {
		return nil
	}
	return e
}
func readPart(ctx context.Context, r io.Reader, b []byte) error {
	_, e := io.ReadFull(ctxReader{ctx, r}, b)
	if e == io.EOF || e == io.ErrUnexpectedEOF {
		return domain.ErrInvalid
	}
	return e
}

// Put uses signed, seekable bodies, multiparting objects larger than one buffer.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) (ports.BlobInfo, error) {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobs3: Put")
	if e := valid(key); e != nil {
		return ports.BlobInfo{}, e
	}
	if size < 0 || size > int64(ports.BlobPartSize)*ports.BlobMaxParts {
		return ports.BlobInfo{}, domain.ErrInvalid
	}
	bp := buffers.Get().(*[]byte)
	defer buffers.Put(bp)
	h := sha256.New()
	source := io.TeeReader(r, h)
	if size <= ports.BlobPartSize {
		b := (*bp)[:int(size)]
		if e := readPart(ctx, source, b); e != nil {
			return ports.BlobInfo{}, e
		}
		if e := exactEnd(ctx, r); e != nil {
			return ports.BlobInfo{}, e
		}
		_, e := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: s.key(key), Body: bytes.NewReader(b), ContentLength: aws.Int64(size)})
		if e != nil {
			return ports.BlobInfo{}, translate(e)
		}
	} else {
		u, e := s.NewMultipart(ctx, key)
		if e != nil {
			return ports.BlobInfo{}, e
		}
		completed := false
		defer func() {
			if !completed {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
				defer cancel()
				_ = s.AbortMultipart(cleanup, key, u.ID())
			}
		}()
		remaining := size
		for n := 1; remaining > 0; n++ {
			count := min(remaining, int64(ports.BlobPartSize))
			b := (*bp)[:int(count)]
			if e = readPart(ctx, source, b); e != nil {
				return ports.BlobInfo{}, e
			}
			if e = u.UploadPart(ctx, n, bytes.NewReader(b), count); e != nil {
				return ports.BlobInfo{}, e
			}
			remaining -= count
		}
		if e = exactEnd(ctx, r); e != nil {
			return ports.BlobInfo{}, e
		}
		if _, e = u.Complete(ctx); e != nil {
			return ports.BlobInfo{}, e
		}
		completed = true
	}
	log := zerowrap.FromCtx(ctx)
	log.Debug().Str("key", key).Int64("size", size).Msg("blob stored")
	return ports.BlobInfo{Key: key, Size: size, Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), ModTime: time.Now().UTC()}, nil
}

// Stat retrieves metadata without downloading content.
func (s *Store) Stat(ctx context.Context, key string) (ports.BlobInfo, error) {
	if e := valid(key); e != nil {
		return ports.BlobInfo{}, e
	}
	o, e := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: s.key(key)})
	if e != nil {
		return ports.BlobInfo{}, translate(e)
	}
	return ports.BlobInfo{Key: key, Size: aws.ToInt64(o.ContentLength), ModTime: aws.ToTime(o.LastModified)}, nil
}

// Open streams a single range of an object.
//
//hexcheck:ignore suspicious-business-logic-in-adapter translates the port offset/length contract into an HTTP Range header; range rules belong to the port
func (s *Store) Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ports.BlobInfo, error) {
	if e := valid(key); e != nil {
		return nil, ports.BlobInfo{}, e
	}
	if offset < 0 {
		return nil, ports.BlobInfo{}, domain.ErrInvalid
	}
	if length == 0 {
		info, e := s.Stat(ctx, key)
		if e != nil {
			return nil, info, e
		}
		if offset >= info.Size {
			return nil, info, domain.ErrOutOfRange
		}
		return io.NopCloser(strings.NewReader("")), info, nil
	}
	rg := fmt.Sprintf("bytes=%d-", offset)
	if length > 0 && length-1 <= int64(^uint64(0)>>1)-offset {
		rg += strconv.FormatInt(offset+length-1, 10)
	}
	o, e := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: s.key(key), Range: aws.String(rg)})
	if e != nil {
		return nil, ports.BlobInfo{}, translate(e)
	}
	size := aws.ToInt64(o.ContentLength)
	if _, total, ok := strings.Cut(aws.ToString(o.ContentRange), "/"); ok {
		if n, e := strconv.ParseInt(total, 10, 64); e == nil {
			size = n
		}
	}
	return o.Body, ports.BlobInfo{Key: key, Size: size, ModTime: aws.ToTime(o.LastModified)}, nil
}

// Delete removes a key; absence is success.
func (s *Store) Delete(ctx context.Context, key string) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobs3: Delete")
	if e := valid(key); e != nil {
		return e
	}
	_, e := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: s.key(key)})
	if status(e) == 404 {
		return nil
	}
	return translate(e)
}

// List visits matching objects lexicographically.
func (s *Store) List(ctx context.Context, prefix string, fn func(ports.BlobInfo) error) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobs3: List")
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.cfg.Bucket), Prefix: s.key(prefix)})
	for p.HasMorePages() {
		o, e := p.NextPage(ctx)
		if e != nil {
			return translate(e)
		}
		for _, v := range o.Contents {
			if e = fn(ports.BlobInfo{Key: strings.TrimPrefix(aws.ToString(v.Key), s.cfg.Prefix), Size: aws.ToInt64(v.Size), ModTime: aws.ToTime(v.LastModified)}); e != nil {
				return e
			}
		}
	}
	return nil
}

// Check probes the existing bucket without modifying it.
func (s *Store) Check(ctx context.Context) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobs3: Check")
	_, e := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.cfg.Bucket)})
	if status(e) == 403 {
		_, e = s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.cfg.Bucket), Prefix: aws.String(s.cfg.Prefix), MaxKeys: aws.Int32(1)})
	}
	if e == nil {
		return nil
	}
	where := s.cfg.Endpoint
	if where == "" {
		where = s.cfg.Region
	}
	return fmt.Errorf("bucket %q is not reachable at %s: %w", s.cfg.Bucket, where, e)
}

type part struct {
	etag string
	size int64
}
type upload struct {
	s       *Store
	key, id string
	parts   map[int]part
	done    bool
}

// NewMultipart initiates an object upload.
func (s *Store) NewMultipart(ctx context.Context, key string) (ports.MultipartUpload, error) {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobs3: NewMultipart")
	if e := valid(key); e != nil {
		return nil, e
	}
	o, e := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(s.cfg.Bucket), Key: s.key(key)})
	if e != nil {
		return nil, translate(e)
	}
	return &upload{s: s, key: key, id: aws.ToString(o.UploadId), parts: map[int]part{}}, nil
}
func (u *upload) ID() string { return u.id }
func (u *upload) UploadPart(ctx context.Context, n int, r io.Reader, size int64) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobs3: UploadPart")
	if u.done || n < 1 || n > ports.BlobMaxParts || size < 0 || size > ports.BlobPartSize {
		return domain.ErrInvalid
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	var body io.ReadSeeker
	if seeker, ok := r.(io.ReadSeeker); ok {
		start, e := seeker.Seek(0, io.SeekCurrent)
		if e != nil {
			return e
		}
		end, e := seeker.Seek(0, io.SeekEnd)
		if e != nil {
			return e
		}
		if _, e = seeker.Seek(start, io.SeekStart); e != nil {
			return e
		}
		if end-start != size {
			return domain.ErrInvalid
		}
		body = seeker
	} else {
		bp := buffers.Get().(*[]byte)
		defer buffers.Put(bp)
		b := (*bp)[:int(size)]
		if e := readPart(ctx, r, b); e != nil {
			return e
		}
		if e := exactEnd(ctx, r); e != nil {
			return e
		}
		body = bytes.NewReader(b)
	}
	o, e := u.s.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(u.s.cfg.Bucket), Key: u.s.key(u.key), UploadId: aws.String(u.id), PartNumber: aws.Int32(int32(n)), Body: body, ContentLength: aws.Int64(size)})
	if e != nil {
		return translate(e)
	}
	u.parts[n] = part{aws.ToString(o.ETag), size}
	log := zerowrap.FromCtx(ctx)
	log.Debug().Int("part", n).Msg("blob part uploaded")
	return nil
}
func (u *upload) Complete(ctx context.Context) (ports.BlobInfo, error) {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobs3: Complete")
	if u.done {
		return ports.BlobInfo{}, domain.ErrInvalid
	}
	u.done = true
	var nums []int
	for n := range u.parts {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var parts []types.CompletedPart
	var size int64
	invalid := len(nums) == 0
	for i, n := range nums {
		p := u.parts[n]
		if i < len(nums)-1 && p.size < ports.BlobMinPartSize {
			invalid = true
		}
		size += p.size
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(n)), ETag: aws.String(p.etag)})
	}
	if invalid {
		_ = u.s.AbortMultipart(ctx, u.key, u.id)
		return ports.BlobInfo{}, domain.ErrInvalid
	}
	_, e := u.s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(u.s.cfg.Bucket), Key: u.s.key(u.key), UploadId: aws.String(u.id), MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	if e != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		_ = u.s.AbortMultipart(cleanup, u.key, u.id)
		return ports.BlobInfo{}, translate(e)
	}
	return ports.BlobInfo{Key: u.key, Size: size, ModTime: time.Now().UTC()}, nil
}
func (u *upload) Abort(ctx context.Context) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobs3: Abort")
	if u.done {
		return domain.ErrInvalid
	}
	u.done = true
	return u.s.AbortMultipart(ctx, u.key, u.id)
}

// AbortMultipart discards a listed upload.
func (s *Store) AbortMultipart(ctx context.Context, key, id string) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobs3: AbortMultipart")
	if e := valid(key); e != nil {
		return e
	}
	_, e := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(s.cfg.Bucket), Key: s.key(key), UploadId: aws.String(id)})
	if status(e) == 404 {
		return nil
	}
	return translate(e)
}

// ListMultipart visits unfinished uploads matching prefix.
func (s *Store) ListMultipart(ctx context.Context, prefix string, fn func(ports.MultipartInfo) error) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobs3: ListMultipart")
	input := &s3.ListMultipartUploadsInput{Bucket: aws.String(s.cfg.Bucket), Prefix: s.key(prefix)}
	for {
		o, e := s.client.ListMultipartUploads(ctx, input)
		if e != nil {
			return translate(e)
		}
		for _, v := range o.Uploads {
			if e = fn(ports.MultipartInfo{Key: strings.TrimPrefix(aws.ToString(v.Key), s.cfg.Prefix), ID: aws.ToString(v.UploadId), Started: aws.ToTime(v.Initiated)}); e != nil {
				return e
			}
		}
		if !aws.ToBool(o.IsTruncated) {
			return nil
		}
		input.KeyMarker = o.NextKeyMarker
		input.UploadIdMarker = o.NextUploadIdMarker
	}
}
