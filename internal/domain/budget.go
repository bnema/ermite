// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"math"
)

// ValidateCommitObjects checks a request before any repository I/O. max is the
// caller's commit-count limit.
func ValidateCommitObjects(ids []ObjectID, max int) error {
	if len(ids) > max {
		return fmt.Errorf("%w: %d commit objects asked, at most %d", ErrInvalid, len(ids), max)
	}
	for _, id := range ids {
		if id.IsZero() {
			return fmt.Errorf("%w: empty commit id", ErrInvalidObjectID)
		}
	}
	return nil
}

// CommitObjectBudget bounds a batch of raw commits. Its zero value is ready to
// use. Admission is a preview; Charge records only successfully read content.
type CommitObjectBudget struct {
	total int64
	seen  map[ObjectID]bool
}

func (b *CommitObjectBudget) Seen(id ObjectID) bool { return b.seen[id] }

func (b *CommitObjectBudget) Admit(id ObjectID, commit bool, size int64) (take, stop bool) {
	if b.Seen(id) || !commit || size < 0 || size > MaxCommitObjectBytes {
		return false, false
	}
	if size > MaxCommitObjectsBytes-b.total {
		return false, true
	}
	return true, false
}

func (b *CommitObjectBudget) Charge(id ObjectID, size int64) {
	if b.seen == nil {
		b.seen = make(map[ObjectID]bool)
	}
	b.seen[id] = true
	b.total += size
}

// DiffBudget owns file selection and retained hunk accounting. Omitted files
// consume no budget; streaming readers may use a copy while attempting a file.
type DiffBudget struct {
	limits               DiffLimits
	linesLeft, bytesLeft int
}

func NewDiffBudget(limits DiffLimits) DiffBudget {
	return DiffBudget{limits: limits, linesLeft: limits.MaxTotalLines, bytesLeft: limits.MaxTotalBytes}
}

func (b DiffBudget) Window(files int) (kept, from, to int, truncated bool) {
	kept = files
	if b.limits.MaxFiles > 0 && kept > b.limits.MaxFiles {
		kept = b.limits.MaxFiles
		truncated = true
	}
	from, to = max(b.limits.From, 0), kept
	if b.limits.To > 0 {
		to = min(b.limits.To, kept)
	}
	return
}

// Admit decides whether the hunks of a file are read. read is false with
// OmitNone for a binary file or one without changed lines (nothing to show).
// Otherwise read is false with the reason the file is omitted. path is the path
// a comment designates (see FileDiff.CommentPath).
func (b DiffBudget) Admit(binary bool, path string, changed int) (read bool, why FileOmission) {
	switch {
	case binary || changed <= 0:
		return false, OmitNone
	case b.limits.SkipGenerated && IsGeneratedPath(path):
		return false, OmitGenerated
	case b.limits.MaxFileLines > 0 && changed > b.limits.MaxFileLines:
		return false, OmitLarge
	case b.limits.MaxTotalLines > 0 && changed > b.linesLeft, b.bytesLeft <= 0:
		return false, OmitBudget
	}
	return true, OmitNone
}

// FileLines is the most changed lines the next file may have; math.MaxInt when
// neither MaxFileLines nor MaxTotalLines limits it.
func (b DiffBudget) FileLines() int {
	n := math.MaxInt
	if b.limits.MaxFileLines > 0 {
		n = b.limits.MaxFileLines
	}
	if b.limits.MaxTotalLines > 0 {
		n = min(n, b.linesLeft)
	}
	return n
}

// ChargeLine accounts for decoded text bytes and, for additions or deletions,
// one changed line. Context consumes bytes only. It returns OmitNone when the
// line fits, OmitBudget when the changed lines ran out and OmitUnavailable when
// the bytes ran out.
func (b *DiffBudget) ChargeLine(bytes int, changed bool) FileOmission {
	if changed && b.limits.MaxTotalLines > 0 {
		b.linesLeft--
	}
	b.bytesLeft -= bytes
	switch {
	case b.limits.MaxTotalLines > 0 && b.linesLeft < 0:
		return OmitBudget
	case b.bytesLeft < 0:
		return OmitUnavailable
	}
	return OmitNone
}
