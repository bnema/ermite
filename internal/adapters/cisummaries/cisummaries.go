// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cisummaries implements ports.CISummaryStore with one Markdown file per
// step under $ERMITE_DATA_DIR/ci/summaries/<run id>/<job id>/<attempt>/<step>.md.
//
// A summary is written to a temporary file of the same directory, synced, then
// renamed over the final name: a reader sees the old summary or the new one,
// never a part. The files of a run go with the logs of the run: the use case
// that removes the logs removes them too.
package cisummaries

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Dir is the directory of the summaries under the data directory.
func Dir(dataDir string) string { return filepath.Join(dataDir, "ci", "summaries") }

const ext = ".md"

// Store is the file-backed summary store.
type Store struct{ root string }

var _ ports.CISummaryStore = (*Store)(nil)

// New returns a store rooted at dir, which need not exist yet.
func New(dir string) *Store { return &Store{root: dir} }

// validRef rejects a reference that could escape the directory.
func validRef(ref ports.CILogRef) bool {
	if ref.RunID < 1 || ref.Attempt < 1 {
		return false
	}
	_, _, _, ok := domain.ParseJobID(ref.JobID)
	return ok
}

func (s *Store) jobDir(ref ports.CILogRef) string {
	return filepath.Join(s.root, strconv.FormatInt(ref.RunID, 10), ref.JobID, strconv.Itoa(int(ref.Attempt)))
}

func (s *Store) path(ref ports.CILogRef, step int32) string {
	return filepath.Join(s.jobDir(ref), strconv.Itoa(int(step))+ext)
}

// Put stores the summary of a step, or removes it when markdown is empty.
func (s *Store) Put(_ context.Context, ref ports.CILogRef, step int32, markdown string) error {
	if !validRef(ref) || step < 1 || step > domain.CIMaxSteps {
		return domain.ErrNotFound
	}
	if markdown == "" {
		if err := os.Remove(s.path(ref, step)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove summary: %w", err)
		}
		return nil
	}
	dir := s.jobDir(ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create summary directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create summary: %w", err)
	}
	tmp := f.Name()
	_, werr := f.WriteString(markdown)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write summary: %w", err)
	}
	if err := os.Rename(tmp, s.path(ref, step)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store summary: %w", err)
	}
	return nil
}

// List returns the summaries of the steps that have one, by step number.
func (s *Store) List(_ context.Context, ref ports.CILogRef, limit int) ([]domain.CIStepSummary, bool, error) {
	if !validRef(ref) {
		return nil, false, nil
	}
	entries, err := os.ReadDir(s.jobDir(ref))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("list summaries: %w", err)
	}
	var steps []int32
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ext)
		if !ok || e.IsDir() {
			continue // a temporary file, or something else
		}
		n, err := strconv.ParseInt(name, 10, 32)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != name {
			continue
		}
		steps = append(steps, int32(n)) //nolint:gosec // parsed with a 32-bit size
	}
	slices.Sort(steps)
	more := limit > 0 && len(steps) > limit
	if more {
		steps = steps[:limit]
	}
	out := make([]domain.CIStepSummary, 0, len(steps))
	for _, n := range steps {
		b, err := os.ReadFile(s.path(ref, n))
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed meanwhile
		}
		if err != nil {
			return nil, false, fmt.Errorf("read summary: %w", err)
		}
		out = append(out, domain.CIStepSummary{Step: n, Markdown: string(b)})
	}
	return out, more, nil
}

// RemoveJob deletes the summaries of one execution of a job.
func (s *Store) RemoveJob(_ context.Context, ref ports.CILogRef) error {
	if !validRef(ref) {
		return nil
	}
	if err := os.RemoveAll(s.jobDir(ref)); err != nil {
		return fmt.Errorf("remove summaries: %w", err)
	}
	return nil
}

// RemoveRuns deletes the summaries of the runs.
func (s *Store) RemoveRuns(_ context.Context, runIDs []int64) error {
	var errs []error
	for _, id := range runIDs {
		if id < 1 {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, strconv.FormatInt(id, 10))); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RunIDs lists the runs that have a summary directory.
func (s *Store) RunIDs(_ context.Context) ([]int64, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list summaries: %w", err)
	}
	var ids []int64
	for _, e := range entries {
		if id, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && e.IsDir() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
