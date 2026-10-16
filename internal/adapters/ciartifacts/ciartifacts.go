// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ciartifacts implements ports.CIArtifactFiles under
// $ERMITE_DATA_DIR/ci/artifacts, keyed by the global run id like the logs (a
// repository rename never moves them):
//
//	<run:020d>/<id:020d>            the content of a committed artifact
//	<run:020d>/<id:020d>.blocks/<f> the staged blocks of a pending one
//	<run:020d>/.tmp-<id:020d>-*     a content being assembled
//
// Paths are made of ids and of block file names the use case builds from
// hexadecimal characters: an artifact name never reaches the file system.
// Files are 0600 in 0700 directories, synced before they count, and a content
// appears by rename, whole or not at all.
package ciartifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Dir is the directory of the artifacts under the data directory.
func Dir(dataDir string) string { return filepath.Join(dataDir, "ci", "artifacts") }

const (
	idFormat     = "%020d"
	blocksSuffix = ".blocks"
	tmpPrefix    = ".tmp-"
	// maxFileName bounds a block file name (well under the 255 bytes of a
	// name on Linux).
	maxFileName = 200
)

// Store is the file-backed artifact store.
type Store struct{ root string }

var _ ports.CIArtifactFiles = (*Store)(nil)

// New returns a store rooted at dir, which need not exist yet.
func New(dir string) *Store { return &Store{root: dir} }

func validRef(ref ports.CIArtifactRef) bool { return ref.RunID > 0 && ref.ID > 0 }

// validFile accepts the block file names of the use case: lowercase
// hexadecimal characters and dots, not starting with a dot.
func validFile(name string) bool {
	if name == "" || len(name) > maxFileName || name[0] == '.' {
		return false
	}
	return strings.Trim(name, "0123456789abcdef.") == ""
}

func (s *Store) runDir(runID int64) string {
	return filepath.Join(s.root, fmt.Sprintf(idFormat, runID))
}

func (s *Store) contentPath(ref ports.CIArtifactRef) string {
	return filepath.Join(s.runDir(ref.RunID), fmt.Sprintf(idFormat, ref.ID))
}

func (s *Store) blocksDir(ref ports.CIArtifactRef) string {
	return s.contentPath(ref) + blocksSuffix
}

// syncDir makes the entries of a directory durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	return errors.Join(serr, d.Close())
}

// ctxReader stops a copy when its context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// PutBlock writes a new block file, created exclusively.
func (s *Store) PutBlock(ctx context.Context, ref ports.CIArtifactRef, file string, src io.Reader) (int64, error) {
	if !validRef(ref) || !validFile(file) {
		return 0, fmt.Errorf("%w: block file %q", domain.ErrInvalid, file)
	}
	dir := s.blocksDir(ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("create block directory: %w", err)
	}
	path := filepath.Join(dir, file)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create block: %w", err)
	}
	n, werr := io.Copy(f, ctxReader{ctx, src})
	var serr error
	if werr == nil {
		serr = f.Sync()
	}
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(path)
		if werr != nil {
			return 0, werr // the source's error, as it is
		}
		return 0, fmt.Errorf("write block: %w", err)
	}
	if err := syncDir(dir); err != nil {
		_ = os.Remove(path)
		return 0, fmt.Errorf("sync block directory: %w", err)
	}
	return n, nil
}

// RemoveBlock deletes one block file.
func (s *Store) RemoveBlock(_ context.Context, ref ports.CIArtifactRef, file string) error {
	if !validRef(ref) || !validFile(file) {
		return nil
	}
	if err := os.Remove(filepath.Join(s.blocksDir(ref), file)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove block: %w", err)
	}
	return nil
}

// Assemble writes the blocks into a temporary file, syncs it and renames it
// over the content.
func (s *Store) Assemble(ctx context.Context, ref ports.CIArtifactRef, files []string) (int64, string, error) {
	if !validRef(ref) {
		return 0, "", domain.ErrNotFound
	}
	for _, f := range files {
		if !validFile(f) {
			return 0, "", fmt.Errorf("%w: block file %q", domain.ErrInvalid, f)
		}
	}
	dir := s.runDir(ref.RunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, "", fmt.Errorf("create artifact directory: %w", err)
	}
	out, err := os.CreateTemp(dir, tmpPrefix+fmt.Sprintf(idFormat, ref.ID)+"-*")
	if err != nil {
		return 0, "", fmt.Errorf("create artifact: %w", err)
	}
	tmp := out.Name()
	h := sha256.New()
	size, err := s.concat(ctx, ref, files, io.MultiWriter(out, h))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, s.contentPath(ref))
	}
	if err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, domain.ErrNotFound) || ctx.Err() != nil {
			return 0, "", err
		}
		return 0, "", fmt.Errorf("assemble artifact: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return 0, "", fmt.Errorf("sync artifact directory: %w", err)
	}
	return size, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Store) concat(ctx context.Context, ref ports.CIArtifactRef, files []string, w io.Writer) (int64, error) {
	var size int64
	for _, name := range files {
		f, err := os.Open(filepath.Join(s.blocksDir(ref), name))
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("%w: block file %s", domain.ErrNotFound, name)
		}
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(w, ctxReader{ctx, f})
		_ = f.Close()
		if err != nil {
			return 0, err
		}
		size += n
	}
	return size, nil
}

// Open opens the content at offset.
func (s *Store) Open(_ context.Context, ref ports.CIArtifactRef, offset int64) (io.ReadCloser, error) {
	if !validRef(ref) || offset < 0 {
		return nil, domain.ErrNotFound
	}
	f, err := os.Open(s.contentPath(ref))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seek artifact: %w", err)
	}
	return f, nil
}

// Remove deletes the content, the blocks and any assembly in progress.
func (s *Store) Remove(ctx context.Context, ref ports.CIArtifactRef) error {
	if !validRef(ref) {
		return nil
	}
	var errs []error
	if err := os.Remove(s.contentPath(ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	errs = append(errs, s.RemoveBlocks(ctx, ref))
	tmps, _ := filepath.Glob(filepath.Join(s.runDir(ref.RunID), tmpPrefix+fmt.Sprintf(idFormat, ref.ID)+"-*"))
	for _, t := range tmps {
		if err := os.Remove(t); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("remove artifact: %w", err)
	}
	return nil
}

// RemoveBlocks deletes the staging directory of the artifact.
func (s *Store) RemoveBlocks(_ context.Context, ref ports.CIArtifactRef) error {
	if !validRef(ref) {
		return nil
	}
	if err := os.RemoveAll(s.blocksDir(ref)); err != nil {
		return fmt.Errorf("remove blocks: %w", err)
	}
	return nil
}

// RemoveRuns deletes the directories of the runs.
func (s *Store) RemoveRuns(_ context.Context, runIDs []int64) error {
	var errs []error
	for _, id := range runIDs {
		if id < 1 {
			continue
		}
		if err := os.RemoveAll(s.runDir(id)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RunIDs lists the run directories.
func (s *Store) RunIDs(_ context.Context) ([]int64, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list artifact directories: %w", err)
	}
	var ids []int64
	for _, e := range entries {
		if id, ok := parseID(e.Name()); ok && e.IsDir() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ArtifactIDs lists the artifacts that have a content, blocks or an assembly
// in progress in the run directory.
func (s *Store) ArtifactIDs(_ context.Context, runID int64) ([]int64, error) {
	if runID < 1 {
		return nil, nil
	}
	entries, err := os.ReadDir(s.runDir(runID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	var ids []int64
	for _, e := range entries {
		name := e.Name()
		if rest, ok := strings.CutPrefix(name, tmpPrefix); ok {
			name, _, _ = strings.Cut(rest, "-")
		} else {
			name = strings.TrimSuffix(name, blocksSuffix)
		}
		if id, ok := parseID(name); ok && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// parseID reads a zero-padded id.
func parseID(name string) (int64, bool) {
	if len(name) != 20 {
		return 0, false
	}
	id, err := strconv.ParseInt(name, 10, 64)
	return id, err == nil && id > 0
}
