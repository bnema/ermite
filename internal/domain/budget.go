// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "fmt"

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

// DiffBudget owns file selection and retained hunk accounting. Truncated files
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

func (b DiffBudget) ChangedLines(binary bool, n int) bool { return !binary && n > 0 }

func (b DiffBudget) WantsHunks(binary bool, n int) bool {
	return b.ChangedLines(binary, n) && n <= b.limits.MaxFileLines && n <= b.linesLeft && b.bytesLeft > 0
}

func (b DiffBudget) FileLines() int { return min(b.limits.MaxFileLines, b.linesLeft) }

// ChargeLine accounts for decoded text bytes and, for additions or deletions,
// one changed line. Context consumes bytes only. It reports whether the line fits.
func (b *DiffBudget) ChargeLine(bytes int, changed bool) bool {
	if changed {
		b.linesLeft--
	}
	b.bytesLeft -= bytes
	return b.linesLeft >= 0 && b.bytesLeft >= 0
}
