// SPDX-License-Identifier: AGPL-3.0-or-later

// Package blobdisk stores objects under root/objects, temporary writes under
// root/tmp, and unfinished multipart uploads under root/multipart. Files are
// 0600 in 0700 directories; completed objects appear by synced atomic rename.
package blobdisk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/bnema/zerowrap"
)

// Store is the disk implementation of ports.BlobStore.
type Store struct{ root string }

var _ ports.BlobStore = (*Store)(nil)

// Dir returns the blob root under dataDir.
func Dir(dataDir string) string { return filepath.Join(dataDir, "blobs") }

// New returns a disk store without performing I/O.
func New(root string) *Store { return &Store{root: root} }
func (s *Store) object(key string) string {
	return filepath.Join(s.root, "objects", filepath.FromSlash(key))
}
func valid(key string) error {
	if !ports.ValidBlobKey(key) {
		return domain.ErrInvalid
	}
	return nil
}
func translate(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return domain.ErrNotFound
	}
	return err
}

// tmpGrace is how long leftovers in root/tmp (and malformed multipart dirs)
// are kept before reclamation.
const tmpGrace = time.Hour

// now is the clock used for reclamation; tests may replace it.
var now = time.Now

// syncDir fsyncs a directory; tests may replace it to record calls.
var syncDir = func(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	return errors.Join(f.Sync(), f.Close())
}

// mkdirAllSync creates dir (under root) with 0700, fsyncing the parent of
// every directory it creates so the new entries are durable.
func mkdirAllSync(root, dir string) error {
	var missing []string
	for d := filepath.Clean(dir); d != filepath.Clean(root) && d != filepath.Dir(d); d = filepath.Dir(d) {
		if fi, e := os.Stat(d); e == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s: not a directory", d)
			}
			break
		} else if !errors.Is(e, fs.ErrNotExist) {
			return e
		}
		missing = append(missing, d)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if e := os.Mkdir(missing[i], 0700); e != nil && !errors.Is(e, fs.ErrExist) {
			return e
		}
		if e := syncDir(filepath.Dir(missing[i])); e != nil {
			return e
		}
	}
	return nil
}

// reclaimTmp removes entries (files or directories) in root/tmp whose mtime
// is older than olderThan.
func (s *Store) reclaimTmp(ctx context.Context, olderThan time.Duration) error {
	dir := filepath.Join(s.root, "tmp")
	entries, e := os.ReadDir(dir)
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	cutoff := now().Add(-olderThan)
	var errs []error
	for _, d := range entries {
		if e = ctx.Err(); e != nil {
			return e
		}
		fi, e := d.Info()
		if e != nil || !fi.ModTime().Before(cutoff) {
			continue
		}
		if e = os.RemoveAll(filepath.Join(dir, d.Name())); e != nil {
			errs = append(errs, e)
			continue
		}
		logEntry := zerowrap.FromCtx(ctx)
		logEntry.Info().Str("entry", d.Name()).Msg("blobdisk: reclaimed stale tmp entry")
	}
	return errors.Join(errs...)
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
func (s *Store) init(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	for _, d := range []string{"objects", "tmp", "multipart"} {
		if e := os.MkdirAll(filepath.Join(s.root, d), 0700); e != nil {
			return e
		}
	}
	return nil
}

// Check verifies that the root is writable.
func (s *Store) Check(ctx context.Context) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobdisk: Check")
	if e := s.init(ctx); e != nil {
		return e
	}
	if e := s.reclaimTmp(ctx, tmpGrace); e != nil {
		logEntry.Warn().Err(e).Msg("blobdisk: tmp reclamation failed")
	}
	f, e := os.CreateTemp(filepath.Join(s.root, "tmp"), ".probe-*")
	if e != nil {
		return e
	}
	return errors.Join(f.Close(), os.Remove(f.Name()))
}

// Put atomically replaces key with exactly size bytes.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) (ports.BlobInfo, error) {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobdisk: Put")
	if e := valid(key); e != nil {
		return ports.BlobInfo{}, e
	}
	if size < 0 {
		return ports.BlobInfo{}, domain.ErrInvalid
	}
	if e := s.init(ctx); e != nil {
		return ports.BlobInfo{}, e
	}
	f, e := os.CreateTemp(filepath.Join(s.root, "tmp"), ".tmp-*")
	if e != nil {
		return ports.BlobInfo{}, e
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), ctxReader{ctx, r})
	if e == nil && n != size {
		e = domain.ErrInvalid
	}
	if e == nil {
		e = ctx.Err()
	}
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return ports.BlobInfo{}, e
	}
	path := s.object(key)
	if e = mkdirAllSync(filepath.Join(s.root, "objects"), filepath.Dir(path)); e == nil {
		e = os.Rename(f.Name(), path)
	}
	if e == nil {
		e = syncDir(filepath.Dir(path))
	}
	if e != nil {
		return ports.BlobInfo{}, e
	}
	info, e := s.Stat(ctx, key)
	info.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return info, e
}

// Stat returns object metadata.
func (s *Store) Stat(ctx context.Context, key string) (ports.BlobInfo, error) {
	if e := valid(key); e != nil {
		return ports.BlobInfo{}, e
	}
	if e := ctx.Err(); e != nil {
		return ports.BlobInfo{}, e
	}
	f, e := os.Stat(s.object(key))
	if e != nil {
		return ports.BlobInfo{}, translate(e)
	}
	if !f.Mode().IsRegular() {
		return ports.BlobInfo{}, domain.ErrNotFound
	}
	return ports.BlobInfo{Key: key, Size: f.Size(), ModTime: f.ModTime()}, nil
}

type limitedCloser struct {
	io.Reader
	io.Closer
}

// Open reads an object range.
//
//hexcheck:ignore suspicious-business-logic-in-adapter translates the port offset/length contract into a file seek; range rules belong to the port
func (s *Store) Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ports.BlobInfo, error) {
	info, e := s.Stat(ctx, key)
	if e != nil {
		return nil, info, e
	}
	if offset < 0 {
		return nil, info, domain.ErrInvalid
	}
	if offset >= info.Size {
		return nil, info, domain.ErrOutOfRange
	}
	f, e := os.Open(s.object(key))
	if e != nil {
		return nil, info, translate(e)
	}
	if _, e = f.Seek(offset, io.SeekStart); e != nil {
		f.Close()
		return nil, info, e
	}
	var r io.Reader = ctxReader{ctx, f}
	if length >= 0 {
		r = io.LimitReader(r, length)
	}
	return limitedCloser{r, f}, info, nil
}

// Delete removes an object, treating absence as success.
func (s *Store) Delete(ctx context.Context, key string) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobdisk: Delete")
	if e := valid(key); e != nil {
		return e
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	e := os.Remove(s.object(key))
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	return e
}

// List visits matching objects in lexical order.
func (s *Store) List(ctx context.Context, prefix string, fn func(ports.BlobInfo) error) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobdisk: List")
	root := filepath.Join(s.root, "objects")
	var infos []ports.BlobInfo
	e := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		if !d.Type().IsRegular() {
			return nil
		}
		key, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		key = filepath.ToSlash(key)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		i, e := d.Info()
		if e != nil {
			return e
		}
		infos = append(infos, ports.BlobInfo{Key: key, Size: i.Size(), ModTime: i.ModTime()})
		return nil
	})
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	for _, i := range infos {
		if e = ctx.Err(); e != nil {
			return e
		}
		if e = fn(i); e != nil {
			return e
		}
	}
	return nil
}

type upload struct {
	s       *Store
	key, id string
	done    bool
}
type metadata struct {
	Key     string
	Started time.Time
}

func (u *upload) dir() string { return filepath.Join(u.s.root, "multipart", u.id) }

// NewMultipart starts an isolated staging upload.
func (s *Store) NewMultipart(ctx context.Context, key string) (ports.MultipartUpload, error) {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobdisk: NewMultipart")
	if e := valid(key); e != nil {
		return nil, e
	}
	if e := s.init(ctx); e != nil {
		return nil, e
	}
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return nil, e
	}
	u := &upload{s: s, key: key, id: hex.EncodeToString(b)}
	stage := filepath.Join(s.root, "tmp", "mp-"+u.id)
	if e := os.Mkdir(stage, 0700); e != nil {
		return nil, e
	}
	if e := writeMetadata(stage, metadata{key, time.Now().UTC()}); e != nil {
		os.RemoveAll(stage)
		return nil, e
	}
	if e := os.Rename(stage, u.dir()); e != nil {
		os.RemoveAll(stage)
		return nil, e
	}
	if e := syncDir(filepath.Join(s.root, "multipart")); e != nil {
		os.RemoveAll(u.dir())
		return nil, e
	}
	return u, nil
}

// writeMetadata durably writes dir/metadata via temp file and rename.
func writeMetadata(dir string, m metadata) error {
	data, e := json.Marshal(m)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".meta-*")
	if e != nil {
		return e
	}
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	if e = errors.Join(e, f.Close()); e == nil {
		e = os.Rename(f.Name(), filepath.Join(dir, "metadata"))
	}
	if e != nil {
		os.Remove(f.Name())
		return e
	}
	return syncDir(dir)
}
func (u *upload) ID() string { return u.id }
func (u *upload) UploadPart(ctx context.Context, n int, r io.Reader, size int64) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobdisk: UploadPart")
	if u.done || n < 1 || n > ports.BlobMaxParts || size < 0 {
		return domain.ErrInvalid
	}
	f, e := os.CreateTemp(u.dir(), ".tmp-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	count, e := io.Copy(f, ctxReader{ctx, r})
	if e == nil && count != size {
		e = domain.ErrInvalid
	}
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), filepath.Join(u.dir(), fmt.Sprintf("%05d", n)))
}
func (u *upload) Complete(ctx context.Context) (ports.BlobInfo, error) {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobdisk: Complete")
	if u.done {
		return ports.BlobInfo{}, domain.ErrInvalid
	}
	u.done = true
	defer os.RemoveAll(u.dir())
	entries, e := os.ReadDir(u.dir())
	if e != nil {
		return ports.BlobInfo{}, e
	}
	var names []string
	for _, d := range entries {
		if n, e := strconv.Atoi(d.Name()); e == nil && n > 0 {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ports.BlobInfo{}, domain.ErrInvalid
	}
	var size int64
	var readers []io.Reader
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for i, n := range names {
		f, e := os.Open(filepath.Join(u.dir(), n))
		if e != nil {
			return ports.BlobInfo{}, e
		}
		files = append(files, f)
		info, e := f.Stat()
		if e != nil {
			return ports.BlobInfo{}, e
		}
		if i < len(names)-1 && info.Size() < ports.BlobMinPartSize {
			return ports.BlobInfo{}, domain.ErrInvalid
		}
		size += info.Size()
		readers = append(readers, f)
	}
	info, e := u.s.Put(ctx, u.key, io.MultiReader(readers...), size)
	info.Digest = ""
	return info, e
}
func (u *upload) Abort(ctx context.Context) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobdisk: Abort")
	if u.done {
		return domain.ErrInvalid
	}
	u.done = true
	return u.s.AbortMultipart(ctx, u.key, u.id)
}
func validID(id string) bool {
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 16 && id == strings.ToLower(id)
}

// AbortMultipart removes a listed upload by identity.
func (s *Store) AbortMultipart(ctx context.Context, key, id string) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobdisk: AbortMultipart")
	if e := valid(key); e != nil {
		return e
	}
	if !validID(id) {
		return domain.ErrInvalid
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	dir := filepath.Join(s.root, "multipart", id)
	b, e := os.ReadFile(filepath.Join(dir, "metadata"))
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var m metadata
	if e = json.Unmarshal(b, &m); e != nil {
		return e
	}
	if m.Key != key {
		return domain.ErrInvalid
	}
	return os.RemoveAll(dir)
}

// ListMultipart visits unfinished uploads matching prefix.
func (s *Store) ListMultipart(ctx context.Context, prefix string, fn func(ports.MultipartInfo) error) error {
	logEntry := zerowrap.FromCtx(ctx)
	logEntry.Debug().Msg("blobdisk: ListMultipart")
	entries, e := os.ReadDir(filepath.Join(s.root, "multipart"))
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = s.reclaimTmp(ctx, tmpGrace); e != nil {
		logEntry.Warn().Err(e).Msg("blobdisk: tmp reclamation failed")
	}
	for _, d := range entries {
		if e = ctx.Err(); e != nil {
			return e
		}
		if !d.IsDir() || !validID(d.Name()) {
			continue
		}
		dir := filepath.Join(s.root, "multipart", d.Name())
		var m metadata
		b, e := os.ReadFile(filepath.Join(dir, "metadata"))
		if e == nil {
			e = json.Unmarshal(b, &m)
		}
		if e == nil && !ports.ValidBlobKey(m.Key) {
			e = domain.ErrInvalid
		}
		if e != nil {
			logEntry.Warn().Err(e).Str("id", d.Name()).Msg("blobdisk: skipping malformed multipart upload")
			if fi, ie := d.Info(); ie == nil && fi.ModTime().Before(now().Add(-tmpGrace)) {
				if re := os.RemoveAll(dir); re != nil {
					logEntry.Warn().Err(re).Str("id", d.Name()).Msg("blobdisk: remove malformed multipart failed")
				} else {
					logEntry.Info().Str("id", d.Name()).Msg("blobdisk: removed malformed multipart upload")
				}
			}
			continue
		}
		if strings.HasPrefix(m.Key, prefix) {
			if e = fn(ports.MultipartInfo{Key: m.Key, ID: d.Name(), Started: m.Started}); e != nil {
				return e
			}
		}
	}
	return nil
}
