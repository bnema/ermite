// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"slices"
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
	// CIConcurrencyPending: the group record names this entry as a waiting
	// one, behind the running entry.
	CIConcurrencyPending CIConcurrencyState = "pending"
	// CIConcurrencyHolding: this entry is the running one of its group.
	CIConcurrencyHolding CIConcurrencyState = "holding"
	// CIConcurrencyReplaced: only in a mark. A newer entry replaced this one:
	// it is owed a cancellation and never acquires the group.
	CIConcurrencyReplaced CIConcurrencyState = "replaced"
	// CIConcurrencyRejected: only in a mark. A queue: max entry that arrived
	// while CIMaxConcurrencyQueue entries were waiting: like a replaced entry it
	// is owed a cancellation and never acquires the group, but its annotation is
	// ConcurrencyQueueFullMessage.
	CIConcurrencyRejected CIConcurrencyState = "rejected"
)

// Owed reports whether a mark in this state is owed a cancellation: the entry
// was replaced or rejected, and never acquires its group.
func (s CIConcurrencyState) Owed() bool {
	return s == CIConcurrencyReplaced || s == CIConcurrencyRejected
}

// CIMaxConcurrencyGroup is the longest concurrency group name GitHub accepts
// (actions/runner WorkflowTemplateConverter.cs:679-687, "must be less than 400
// characters").
const CIMaxConcurrencyGroup = 400

// CIMaxConcurrencyQueue is how many entries may wait in a group with queue: max
// (docs.github.com, data/reusables/actions/actions-group-concurrency.md: "Up to
// 100 jobs or workflow runs can be pending in the concurrency group. When the
// queue is full, any additional jobs or workflow runs are canceled."; the limits
// page: "up to 100 jobs or workflow runs can be queued per concurrency group").
const CIMaxConcurrencyQueue = 100

// CIConcurrency is the evaluated "concurrency:" of a run (workflow level) or of
// a job.
type CIConcurrency struct {
	// Group is the evaluated group as written; "" for none. Groups are compared
	// without regard to case (ConcurrencyKey).
	Group string
	// Cancel is the evaluated cancel-in-progress.
	Cancel bool
	// Max is set for queue: max (the default, queue: single, leaves it false).
	Max bool
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

// ConcurrencyQueueFullMessage is the annotation of a queue: max entry cancelled
// because CIMaxConcurrencyQueue entries already wait in its group. GitHub's text
// is not documented: this one is ermite's.
func ConcurrencyQueueFullMessage(group string) string {
	return "Canceling since the concurrency group '" + group + "' already has " + strconv.Itoa(CIMaxConcurrencyQueue) + " waiting requests"
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
	// Max is the queue mode of the entry: queue: max when set, else single.
	Max bool
}

// Key identifies the entry in the marks of its group.
func (e CIConcurrencyEntry) Key() string {
	return strconv.FormatInt(e.RunID, 10) + "/" + strconv.FormatInt(int64(e.Attempt), 10) + "/" + e.JobID
}

// Same reports whether two entries are the same run or job.
func (e CIConcurrencyEntry) Same(o CIConcurrencyEntry) bool {
	return e.RunID == o.RunID && e.Attempt == o.Attempt && e.JobID == o.JobID
}

// CIConcurrencyRecord is the state of a group: at most one running entry, and
// the waiting entries in the order they started waiting (first in, first out).
//
// GitHub documents each queue mode alone; a group that mixes them follows
// ermite's rules (Enqueue): a queue: single entry that starts waiting cancels
// and replaces every waiting entry, whatever their mode, and is then the only
// one; a queue: max entry joins the end of the line, or is rejected when
// CIMaxConcurrencyQueue entries already wait. When the running entry leaves,
// the head of the line takes its place (Promote); a waiting entry that leaves
// keeps the order of the others.
type CIConcurrencyRecord struct {
	Running *CIConcurrencyEntry
	Pending []CIConcurrencyEntry
}

// IsEmpty reports whether nothing runs or waits in the group.
func (r CIConcurrencyRecord) IsEmpty() bool { return r.Running == nil && len(r.Pending) == 0 }

// Names reports whether the record names the entry as the running or a pending
// one.
func (r CIConcurrencyRecord) Names(e CIConcurrencyEntry) bool {
	return (r.Running != nil && r.Running.Same(e)) || r.PendingIndex(e) >= 0
}

// PendingIndex is the place of the entry in the waiting line, -1 if it does not
// wait.
func (r CIConcurrencyRecord) PendingIndex(e CIConcurrencyEntry) int {
	for i, p := range r.Pending {
		if p.Same(e) {
			return i
		}
	}
	return -1
}

// RemovePending takes the entry out of the waiting line, keeping the order of
// the others, and reports whether it was there.
func (r *CIConcurrencyRecord) RemovePending(e CIConcurrencyEntry) bool {
	i := r.PendingIndex(e)
	if i < 0 {
		return false
	}
	r.Pending = append(r.Pending[:i:i], r.Pending[i+1:]...)
	if len(r.Pending) == 0 {
		r.Pending = nil
	}
	return true
}

// Enqueue puts a new entry, which neither runs nor waits, in the waiting line.
// A queue: single entry replaces every waiting entry (returned, in line order)
// and is then the only one; a queue: max entry is appended, unless limit
// entries already wait: then the record is left as it is and rejected is set.
func (r *CIConcurrencyRecord) Enqueue(e CIConcurrencyEntry, limit int) (replaced []CIConcurrencyEntry, rejected bool) {
	if !e.Max {
		replaced = r.Pending
		r.Pending = []CIConcurrencyEntry{e}
		return replaced, false
	}
	if len(r.Pending) >= limit {
		return nil, true
	}
	r.Pending = append(r.Pending, e)
	return nil, false
}

// Promote gives the free running place to the head of the waiting line, and
// returns it (nil when the place is taken or nobody waits).
func (r *CIConcurrencyRecord) Promote() *CIConcurrencyEntry {
	if r.Running != nil || len(r.Pending) == 0 {
		return nil
	}
	head := r.Pending[0]
	r.Pending = r.Pending[1:]
	if len(r.Pending) == 0 {
		r.Pending = nil
	}
	r.Running = &head
	return r.Running
}

// Clone is a copy of the record that shares nothing with it.
func (r CIConcurrencyRecord) Clone() CIConcurrencyRecord {
	out := CIConcurrencyRecord{Pending: slices.Clone(r.Pending)}
	if r.Running != nil {
		e := *r.Running
		out.Running = &e
	}
	return out
}

// CIConcurrencyMark says where an entry stands in its group. The marks of a group
// are written in the same batch as its record: an entry is marked pending or
// holding if and only if the record names it as a pending or the running
// entry, an entry that a newer one replaced is marked replaced in the batch
// that unnames it, and a queue: max entry that found the line full is marked
// rejected in the batch that refused it. So a replaced entry is found from the marks alone, whatever
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
