// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cilogs implements ports.CILogStore with append-only files under
// $ERMITE_DATA_DIR/ci/logs/<run id>/<job id>/<attempt>.log.
//
// A file holds one line per log line, in GitHub's raw log shape: an RFC 3339
// timestamp with nanoseconds, a space, then the text, behind a one-character
// marker: "-" for a line, "+" for a piece that continues the line before it
// (it keeps its own line number):
//
//   - 2026-10-12T10:00:00.123456789Z text
//   - 2026-10-12T10:00:00.123456789Z more text of the same line
//
// The file is the truth for bytes and line counts: the metadata store is never
// written per chunk. Counts are cached per open log and rebuilt from the file
// after a restart.
package cilogs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Dir is the directory of the logs under the data directory.
func Dir(dataDir string) string { return filepath.Join(dataDir, "ci", "logs") }

// timeLayout is RFC 3339 with nanoseconds, always in UTC and with all nine
// digits, so that every prefix has the same length.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// prefixLen is the length of "- " + timestamp + " ".
const prefixLen = 2 + len(timeLayout) + 1

// Store is the file-backed log store.
type Store struct {
	root string

	mu   sync.Mutex
	logs map[ports.CILogRef]*logState
}

var _ ports.CILogStore = (*Store)(nil)

// logState is what is known of one log file.
type logState struct {
	mu      sync.Mutex
	loaded  bool
	count   int64
	size    int64
	offsets []int64 // byte offset of every 1024th line: index i is line i*1024
	changed chan struct{}
}

const offsetEvery = 1024

// New returns a store rooted at dir, which need not exist yet.
func New(dir string) *Store {
	return &Store{root: dir, logs: make(map[ports.CILogRef]*logState)}
}

func (s *Store) path(ref ports.CILogRef) string {
	return filepath.Join(s.root, strconv.FormatInt(ref.RunID, 10), ref.JobID, strconv.Itoa(int(ref.Attempt))+".log")
}

func (s *Store) state(ref ports.CILogRef) *logState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.logs[ref]
	if !ok {
		st = &logState{changed: make(chan struct{})}
		s.logs[ref] = st
	}
	return st
}

// validRef rejects a reference that could escape the directory.
func validRef(ref ports.CILogRef) bool {
	if ref.RunID < 1 || ref.Attempt < 1 {
		return false
	}
	_, _, _, ok := domain.ParseJobID(ref.JobID)
	return ok
}

// load reads the file to learn its line count and size, once. The caller
// holds st.mu.
func (s *Store) load(ref ports.CILogRef, st *logState) error {
	if st.loaded {
		return nil
	}
	f, err := os.Open(s.path(ref))
	if errors.Is(err, fs.ErrNotExist) {
		st.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<16)
	var count, pos int64
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if count%offsetEvery == 0 {
				st.offsets = append(st.offsets, pos)
			}
			count++
			pos += int64(len(line))
		} else if len(line) > 0 {
			// A torn last line from a crash: cut it, it was never acknowledged.
			if terr := os.Truncate(s.path(ref), pos); terr != nil {
				return fmt.Errorf("repair log: %w", terr)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("read log: %w", err)
		}
	}
	st.count, st.size, st.loaded = count, pos, true
	return nil
}

func encodeLine(l domain.CILogLine) []byte {
	b := make([]byte, 0, prefixLen+len(l.Text)+1)
	if l.Continued {
		b = append(b, '+', ' ')
	} else {
		b = append(b, '-', ' ')
	}
	b = l.Time.UTC().AppendFormat(b, timeLayout)
	b = append(b, ' ')
	b = append(b, l.Text...)
	return append(b, '\n')
}

func decodeLine(b []byte) (domain.CILogLine, error) {
	b = bytes.TrimSuffix(b, []byte{'\n'})
	if len(b) < prefixLen || (b[0] != '-' && b[0] != '+') {
		return domain.CILogLine{}, errors.New("malformed log line")
	}
	t, err := time.Parse(timeLayout, string(b[2:2+len(timeLayout)]))
	if err != nil {
		return domain.CILogLine{}, fmt.Errorf("malformed log time: %w", err)
	}
	return domain.CILogLine{Time: t, Text: string(b[prefixLen:]), Continued: b[0] == '+'}, nil
}

// Append appends lines at offset.
func (s *Store) Append(ctx context.Context, ref ports.CILogRef, offset int64, lines []domain.CILogLine, limit int64) (int64, int, error) {
	return s.append(ctx, ref, offset, lines, limit, false)
}

// AppendForced appends lines whatever the limit.
func (s *Store) AppendForced(ctx context.Context, ref ports.CILogRef, lines []domain.CILogLine) (int64, error) {
	next, _, err := s.append(ctx, ref, -1, lines, 0, true)
	return next, err
}

func (s *Store) append(_ context.Context, ref ports.CILogRef, offset int64, lines []domain.CILogLine, limit int64, force bool) (int64, int, error) {
	if !validRef(ref) {
		return 0, 0, domain.ErrNotFound
	}
	st := s.state(ref)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := s.load(ref, st); err != nil {
		return 0, 0, err
	}
	if offset < 0 {
		offset = st.count
	}
	if offset > st.count {
		return 0, 0, &ports.ErrCILogGap{Count: st.count}
	}
	skip := st.count - offset
	if skip >= int64(len(lines)) {
		return st.count, 0, nil
	}
	fresh := lines[skip:]
	var buf bytes.Buffer
	for _, l := range fresh {
		buf.Write(encodeLine(l))
	}
	if !force && st.size+int64(buf.Len()) > limit {
		return st.count, 0, ports.ErrCILogLimit
	}
	if err := os.MkdirAll(filepath.Dir(s.path(ref)), 0o700); err != nil {
		return 0, 0, fmt.Errorf("create log directory: %w", err)
	}
	f, err := os.OpenFile(s.path(ref), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return 0, 0, fmt.Errorf("open log: %w", err)
	}
	n, werr := f.Write(buf.Bytes())
	cerr := f.Close()
	if werr != nil || cerr != nil {
		// Cut what was written so that the file keeps whole lines.
		_ = os.Truncate(s.path(ref), st.size)
		return 0, 0, fmt.Errorf("write log: %w", errors.Join(werr, cerr))
	}
	pos := st.size
	for i, l := range fresh {
		if (st.count+int64(i))%offsetEvery == 0 {
			st.offsets = append(st.offsets, pos)
		}
		pos += int64(len(encodeLine(l)))
	}
	st.count += int64(len(fresh))
	st.size += int64(n)
	close(st.changed)
	st.changed = make(chan struct{})
	return st.count, len(fresh), nil
}

// Count returns the number of lines held.
func (s *Store) Count(_ context.Context, ref ports.CILogRef) (int64, error) {
	if !validRef(ref) {
		return 0, nil
	}
	st := s.state(ref)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := s.load(ref, st); err != nil {
		return 0, err
	}
	return st.count, nil
}

// Read returns lines from line number from.
func (s *Store) Read(_ context.Context, ref ports.CILogRef, from int64, maxLines, maxBytes int) ([]domain.CILogLine, error) {
	if !validRef(ref) || from < 0 {
		return nil, domain.ErrNotFound
	}
	st := s.state(ref)
	st.mu.Lock()
	if err := s.load(ref, st); err != nil {
		st.mu.Unlock()
		return nil, err
	}
	count, size := st.count, st.size
	var start int64
	var first int64
	if idx := from / offsetEvery; idx < int64(len(st.offsets)) {
		start, first = st.offsets[idx], idx*offsetEvery
	}
	st.mu.Unlock()
	if from > count {
		return nil, domain.ErrOutOfRange
	}
	if _, err := os.Stat(s.path(ref)); errors.Is(err, fs.ErrNotExist) {
		if from == 0 && count == 0 {
			return nil, domain.ErrNotFound
		}
		return nil, domain.ErrNotFound
	}
	if from == count {
		return nil, nil
	}
	f, err := os.Open(s.path(ref))
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	defer func() { _ = f.Close() }()
	// Never read past the size seen under the lock: a concurrent append is
	// for the next call.
	r := bufio.NewReaderSize(io.NewSectionReader(f, start, size-start), 1<<16)
	var out []domain.CILogLine
	total := 0
	for n := first; n < count; n++ {
		raw, err := r.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read log: %w", err)
		}
		if n < from {
			continue
		}
		l, err := decodeLine(raw)
		if err != nil {
			return nil, fmt.Errorf("log line %d: %w", n, err)
		}
		if len(out) > 0 && (len(out) >= maxLines || total+len(l.Text) > maxBytes) {
			break
		}
		total += len(l.Text)
		out = append(out, l)
	}
	return out, nil
}

// Changed returns a channel closed at the next append.
func (s *Store) Changed(ref ports.CILogRef) <-chan struct{} {
	st := s.state(ref)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.changed
}

// Reset empties the log.
func (s *Store) Reset(_ context.Context, ref ports.CILogRef) error {
	if !validRef(ref) {
		return nil
	}
	st := s.state(ref)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.Remove(s.path(ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove log: %w", err)
	}
	st.loaded, st.count, st.size, st.offsets = true, 0, 0, nil
	close(st.changed)
	st.changed = make(chan struct{})
	return nil
}

// RemoveRuns deletes the logs of the runs.
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
	s.mu.Lock()
	for ref := range s.logs {
		for _, id := range runIDs {
			if ref.RunID == id {
				delete(s.logs, ref)
			}
		}
	}
	s.mu.Unlock()
	return errors.Join(errs...)
}

// RunIDs lists the runs that have a log directory.
func (s *Store) RunIDs(_ context.Context) ([]int64, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list logs: %w", err)
	}
	var ids []int64
	for _, e := range entries {
		if id, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && e.IsDir() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
