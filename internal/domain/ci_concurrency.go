// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"strconv"
	"unicode"
	"unicode/utf16"
)

// CIConcurrencyState is where a run or a job is in its concurrency group.
type CIConcurrencyState string

// Concurrency states.
const (
	// CIConcurrencyIdle: the group is evaluated but the entry has not asked for
	// its turn yet.
	CIConcurrencyIdle CIConcurrencyState = ""
	// CIConcurrencyPending: the group record names this entry as the waiting
	// one, behind the running entry.
	CIConcurrencyPending CIConcurrencyState = "pending"
	// CIConcurrencyHolding: this entry is the running one of its group.
	CIConcurrencyHolding CIConcurrencyState = "holding"
	// CIConcurrencyReplaced: only in a mark. A newer entry replaced this one:
	// it is owed a cancellation and never acquires the group.
	CIConcurrencyReplaced CIConcurrencyState = "replaced"
)

// CIMaxConcurrencyGroup is the longest concurrency group name GitHub accepts
// (actions/runner WorkflowTemplateConverter.cs:679-687, "must be less than 400
// characters").
const CIMaxConcurrencyGroup = 400

// CIConcurrency is the evaluated "concurrency:" of a run (workflow level) or of
// a job.
type CIConcurrency struct {
	// Group is the evaluated group as written; "" for none. Groups are compared
	// without regard to case (ConcurrencyKey).
	Group string
	// Cancel is the evaluated cancel-in-progress.
	Cancel bool
	// State is written to the record of the entry only after the group record
	// named it (acquire committed): an entry that waits in a group that does
	// not name it any more was replaced, and is cancelled.
	State CIConcurrencyState
	// Evaluated is set once the expressions were evaluated, whatever the result
	// (a run starts unevaluated, and a re-run evaluates again).
	Evaluated bool
	// FromRun is set on a job that shows the group of its run: the entry is the
	// run's, the job holds nothing of its own.
	FromRun bool
	// Preempted is set when a newer entry of the group cancelled this one.
	Preempted bool
}

// ConcurrencyKey is the identity of a group within a repository. GitHub compares
// groups with .NET's OrdinalIgnoreCase, which compares strings upper-cased
// character by character with the simple case mapping (OrdinalCasing in
// dotnet/runtime, System.Private.CoreLib/src/System/Globalization/
// OrdinalCasing.cs; the source was not available here, so this follows its
// documented rule). A character that is not ASCII and whose upper case is ASCII
// keeps itself: ı (U+0131) and ſ (U+017F) are neither I nor S. So σ, ς and Σ are
// one letter, ı and İ are distinct from I, and i equals I. The key is upper
// case and is never lowered back.
func ConcurrencyKey(group string) string {
	out := make([]rune, 0, len(group))
	for _, r := range group {
		if u := unicode.ToUpper(r); r <= unicode.MaxASCII || u > unicode.MaxASCII {
			r = u
		}
		out = append(out, r)
	}
	return string(out)
}

// ConcurrencyGroupLength is the length of a group as C#'s String.Length counts
// it: in UTF-16 code units.
func ConcurrencyGroupLength(group string) int { return len(utf16.Encode([]rune(group))) }

// ConcurrencyCancelMessage is GitHub's annotation for an entry cancelled by a
// newer one (docs.github.com, "Control the concurrency of workflows and jobs").
func ConcurrencyCancelMessage(group string) string {
	return "Canceling since a higher priority waiting request for '" + group + "' exists"
}

// CIConcurrencyEntry is a run (JobID empty) or a job waiting for or holding a
// concurrency group.
type CIConcurrencyEntry struct {
	RunID   int64
	Attempt int32
	JobID   string
	// Cancel is the cancel-in-progress of the entry: while it waits, the
	// running entry of the group is cancelled.
	Cancel bool
	// Canceling is set on a running entry that a waiting entry with
	// cancel-in-progress asked to stop; it stays named until it has ended, and
	// the reconciliation asks it again.
	Canceling bool
}

// Key identifies the entry in the marks of its group.
func (e CIConcurrencyEntry) Key() string {
	return strconv.FormatInt(e.RunID, 10) + "/" + strconv.FormatInt(int64(e.Attempt), 10) + "/" + e.JobID
}

// Same reports whether two entries are the same run or job.
func (e CIConcurrencyEntry) Same(o CIConcurrencyEntry) bool {
	return e.RunID == o.RunID && e.Attempt == o.Attempt && e.JobID == o.JobID
}

// CIConcurrencyRecord is the state of a group: at most one running entry and
// one pending entry (queue: single).
type CIConcurrencyRecord struct {
	Running *CIConcurrencyEntry
	Pending *CIConcurrencyEntry
}

// IsEmpty reports whether nothing runs or waits in the group.
func (r CIConcurrencyRecord) IsEmpty() bool { return r.Running == nil && r.Pending == nil }

// Names reports whether the record names the entry as the running or the
// pending one.
func (r CIConcurrencyRecord) Names(e CIConcurrencyEntry) bool {
	return (r.Running != nil && r.Running.Same(e)) || (r.Pending != nil && r.Pending.Same(e))
}

// CIConcurrencyMark says where an entry stands in its group. The marks of a group
// are written in the same batch as its record: an entry is marked pending or
// holding if and only if the record names it as the pending or the running
// entry, and an entry that a newer one replaced is marked replaced in the batch
// that unnames it. So a replaced entry is found from the marks alone, whatever
// failed after that batch.
type CIConcurrencyMark struct {
	Entry CIConcurrencyEntry
	// Group is the group as the newest request wrote it (the cancel annotation
	// names it).
	Group string
	State CIConcurrencyState
}

// CIConcurrencyGroupState is a group as one transaction sees it: its record and
// the marks of the entries that belong to it, by CIConcurrencyEntry.Key.
type CIConcurrencyGroupState struct {
	Record CIConcurrencyRecord
	Marks  map[string]CIConcurrencyMark
}

// IsEmpty reports whether the group has no entry and no mark left.
func (s CIConcurrencyGroupState) IsEmpty() bool { return s.Record.IsEmpty() && len(s.Marks) == 0 }
