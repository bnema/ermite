// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ciartifacts reads and removes legacy artifacts recorded before the
// blob store (ADR 0034), under $ERMITE_DATA_DIR/ci/artifacts. Paths use ids
// only. The adapter is removed once all legacy artifacts have expired.
package ciartifacts

import (
	"context"
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
)

// Store is the file-backed artifact store.
type Store struct{ root string }

var _ ports.CIArtifactFiles = (*Store)(nil)

// New returns a store rooted at dir, which need not exist yet.
func New(dir string) *Store { return &Store{root: dir} }

func validRef(ref ports.CIArtifactRef) bool { return ref.RunID > 0 && ref.ID > 0 }

func (s *Store) runDir(runID int64) string {
	return filepath.Join(s.root, fmt.Sprintf(idFormat, runID))
}

func (s *Store) contentPath(ref ports.CIArtifactRef) string {
	return filepath.Join(s.runDir(ref.RunID), fmt.Sprintf(idFormat, ref.ID))
}

func (s *Store) blocksDir(ref ports.CIArtifactRef) string {
	return s.contentPath(ref) + blocksSuffix
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
func (s *Store) Remove(_ context.Context, ref ports.CIArtifactRef) error {
	if !validRef(ref) {
		return nil
	}
	var errs []error
	if err := os.Remove(s.contentPath(ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	errs = append(errs, os.RemoveAll(s.blocksDir(ref)))
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
