// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Key layout of the concurrency groups:
//
//	concur/<repo>/<key(group)>     CIConcurrencyRecord (running and pending entry)
//	concurmark/<repo>/<escaped key(group)>/<run>/<attempt>/<job id>
//	                               CIConcurrencyMark, written in the same batch
//
// The group key disappears only when both the record and its marks are empty.
// Both prefixes are removed with their repository (ciKeys).
const (
	implicitConcurPrefix = "autoconcur/"
	implicitMarkPrefix   = "autoconcurmark/"
	concurPrefix         = "concur/"
	concurMarkPrefixAll  = "concurmark/"
)

// updateConcurrencyTries bounds the retries of UpdateConcurrency under
// contention.
const updateConcurrencyTries = 32

func concurKey(repo domain.RepoName, group string) []byte {
	return []byte(concurPrefix + repo.String() + "/" + domain.ConcurrencyKey(group))
}

var concurrencyStates = []struct {
	d domain.CIConcurrencyState
	p storagepb.CIConcurrencyState
}{
	{domain.CIConcurrencyPending, storagepb.CIConcurrencyState_CI_CONCURRENCY_STATE_PENDING},
	{domain.CIConcurrencyHolding, storagepb.CIConcurrencyState_CI_CONCURRENCY_STATE_HOLDING},
}

// markStates are the states a mark may hold on top of concurrencyStates.
var markStates = append(slices.Clone(concurrencyStates), []struct {
	d domain.CIConcurrencyState
	p storagepb.CIConcurrencyState
}{
	{domain.CIConcurrencyReplaced, storagepb.CIConcurrencyState_CI_CONCURRENCY_STATE_REPLACED},
}...)

func encodeConcurrency(c domain.CIConcurrency) *storagepb.CIConcurrency {
	if c == (domain.CIConcurrency{}) {
		return nil
	}
	out := &storagepb.CIConcurrency{Implicit: c.Implicit, Group: c.Group, Cancel: c.Cancel, Evaluated: c.Evaluated, FromRun: c.FromRun, Preempted: c.Preempted, QueueMax: c.Max}
	for _, e := range concurrencyStates {
		if e.d == c.State {
			out.State = e.p
		}
	}
	return out
}

func decodeConcurrency(pb *storagepb.CIConcurrency) domain.CIConcurrency {
	if pb == nil {
		return domain.CIConcurrency{}
	}
	c := domain.CIConcurrency{Implicit: pb.GetImplicit(), Group: pb.GetGroup(), Cancel: pb.GetCancel(), Evaluated: pb.GetEvaluated(), FromRun: pb.GetFromRun(), Preempted: pb.GetPreempted(), Max: pb.GetQueueMax()}
	for _, e := range concurrencyStates {
		if e.p == pb.GetState() {
			c.State = e.d
		}
	}
	return c
}

func encodeConcurrencyEntry(e *domain.CIConcurrencyEntry) *storagepb.CIConcurrencyEntry {
	if e == nil {
		return nil
	}
	return &storagepb.CIConcurrencyEntry{Implicit: e.Implicit, RunId: e.RunID, Attempt: e.Attempt, JobId: e.JobID, Cancel: e.Cancel, Canceling: e.Canceling, QueueMax: e.Max}
}

// encodeConcurrencyRecord writes the waiting entries to queue, and the head of
// the line to pending too, for a binary that predates queue (a downgrade).
func encodeConcurrencyRecord(r domain.CIConcurrencyRecord) []byte {
	pb := &storagepb.CIConcurrencyRecord{Running: encodeConcurrencyEntry(r.Running)}
	for i := range r.Pending {
		pb.Queue = append(pb.Queue, encodeConcurrencyEntry(&r.Pending[i]))
	}
	if len(r.Pending) > 0 {
		pb.Pending = encodeConcurrencyEntry(&r.Pending[0])
	}
	return mustMarshal(pb)
}

func decodeConcurrencyRecord(val []byte) (domain.CIConcurrencyRecord, error) {
	var pb storagepb.CIConcurrencyRecord
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.CIConcurrencyRecord{}, fmt.Errorf("decode concurrency group: %w", err)
	}
	rec := domain.CIConcurrencyRecord{Running: decodeConcurrencyEntry(pb.GetRunning())}
	for _, q := range pb.GetQueue() {
		rec.Pending = append(rec.Pending, *decodeConcurrencyEntry(q))
	}
	// A record stored before queue: max holds its only waiting entry in pending.
	if p := decodeConcurrencyEntry(pb.GetPending()); p != nil && len(rec.Pending) == 0 {
		rec.Pending = []domain.CIConcurrencyEntry{*p}
	}
	return rec, nil
}

func decodeConcurrencyEntry(pb *storagepb.CIConcurrencyEntry) *domain.CIConcurrencyEntry {
	if pb == nil {
		return nil
	}
	return &domain.CIConcurrencyEntry{Implicit: pb.GetImplicit(), RunID: pb.GetRunId(), Attempt: pb.GetAttempt(), JobID: pb.GetJobId(), Cancel: pb.GetCancel(), Canceling: pb.GetCanceling(), Max: pb.GetQueueMax()}
}

// concurMarkPrefix is where the marks of a group are: the group key is escaped,
// so that the marks of "a" never fall under the prefix of "a/b".
func concurMarkPrefix(repo domain.RepoName, group string) string {
	return concurMarkPrefixAll + repo.String() + "/" + url.PathEscape(domain.ConcurrencyKey(group)) + "/"
}

func encodeConcurrencyMark(m domain.CIConcurrencyMark) []byte {
	pb := &storagepb.CIConcurrencyMark{Entry: encodeConcurrencyEntry(&m.Entry), Group: m.Group}
	for _, e := range markStates {
		if e.d == m.State {
			pb.State = e.p
		}
	}
	if m.State == domain.CIConcurrencyRejected {
		pb.State, pb.QueueFull = storagepb.CIConcurrencyState_CI_CONCURRENCY_STATE_REPLACED, true
	}
	return mustMarshal(pb)
}

func decodeConcurrencyMark(val []byte) (domain.CIConcurrencyMark, error) {
	var pb storagepb.CIConcurrencyMark
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.CIConcurrencyMark{}, fmt.Errorf("decode concurrency mark: %w", err)
	}
	m := domain.CIConcurrencyMark{Group: pb.GetGroup()}
	if e := decodeConcurrencyEntry(pb.GetEntry()); e != nil {
		m.Entry = *e
	}
	for _, e := range markStates {
		if e.p == pb.GetState() {
			m.State = e.d
		}
	}
	if m.State == domain.CIConcurrencyReplaced && pb.GetQueueFull() {
		m.State = domain.CIConcurrencyRejected
	}
	return m, nil
}

// encodeConcurrencyState is a canonical image of a group: the record, then the
// marks in key order. Two states are the same when their images are.
func encodeConcurrencyState(s domain.CIConcurrencyGroupState) []byte {
	out := encodeConcurrencyRecord(s.Record)
	for _, k := range slices.Sorted(maps.Keys(s.Marks)) {
		out = append(out, 0)
		out = append(out, k...)
		out = append(out, 0)
		out = append(out, encodeConcurrencyMark(s.Marks[k])...)
	}
	return out
}

// UpdateConcurrency reads a group with its marks, calls fn on it and stores the
// result in ONE batch: the record (key "concur/..."), every mark that changed
// ("concurmark/...", written or deleted) and, when something changed, the guard
// of the repository. The batch requires the version the record had when it was
// read (or its absence) whatever fn does, and every change rewrites the record,
// so of two transactions that read the same state only one commits; the other
// calls fn again on the new state.
func (c *CI) UpdateConcurrency(ctx context.Context, repo domain.RepoName, group string, fn func(*domain.CIConcurrencyGroupState) error) error {
	return c.updateConcurrency(ctx, repo, group, fn, false)
}

// UpdateImplicitConcurrency stores server groups outside the user namespace.
func (c *CI) UpdateImplicitConcurrency(ctx context.Context, repo domain.RepoName, group string, fn func(*domain.CIConcurrencyGroupState) error) error {
	return c.updateConcurrency(ctx, repo, group, fn, true)
}

//hexcheck:ignore suspicious-business-logic-in-adapter compare-and-swap loop over a record and its marks in one batch: the rule is fn, decided by the caller
func (c *CI) updateConcurrency(ctx context.Context, repo domain.RepoName, group string, fn func(*domain.CIConcurrencyGroupState) error, implicit bool) error {
	key, mprefix := concurKey(repo, group), concurMarkPrefix(repo, group)
	if implicit {
		key = []byte(implicitConcurPrefix + repo.String() + "/" + group)
		mprefix = implicitMarkPrefix + repo.String() + "/" + url.PathEscape(group) + "/"
	}
	for range updateConcurrencyTries {
		var state domain.CIConcurrencyGroupState
		var version uint64
		if item, ok := c.db.Get(ctx, key); ok {
			var err error
			if state.Record, err = decodeConcurrencyRecord(item.Value); err != nil {
				return err
			}
			version = item.Version
		}
		// The marks are read after the record: a transaction that commits in
		// between has changed the record, and the batch below conflicts.
		state.Marks = map[string]domain.CIConcurrencyMark{}
		for _, item := range c.db.Snapshot(ctx).Prefix(ctx, []byte(mprefix)) {
			m, err := decodeConcurrencyMark(item.Value)
			if err != nil {
				return err
			}
			state.Marks[m.Entry.Key()] = m
		}
		// The state as read, in bytes, before fn sees it: fn may change the
		// entries it is given in place, and a copy of the struct shares them.
		before, stored := encodeConcurrencyState(state), maps.Clone(state.Marks)
		if err := fn(&state); err != nil {
			return err
		}
		var b kv.Batch
		if version == 0 {
			b.RequireAbsent(key)
		} else {
			b.RequireVersion(key, version)
		}
		// A state left as it was is not written, but the batch still holds the
		// precondition: the decision was made on the state that was read.
		if !bytes.Equal(before, encodeConcurrencyState(state)) {
			for k, m := range state.Marks {
				if old, ok := stored[k]; !ok || old != m {
					b.Put([]byte(mprefix+k), encodeConcurrencyMark(m))
				}
			}
			for k := range stored {
				if _, ok := state.Marks[k]; !ok {
					b.Delete([]byte(mprefix + k))
				}
			}
			if state.IsEmpty() {
				b.Delete(key)
			} else {
				b.Put(key, encodeConcurrencyRecord(state.Record)) // rewritten even if equal: it is the version gate
				if err := c.guardRepo(ctx, &b, repo); err != nil {
					return err
				}
			}
		}
		if _, err := c.db.Apply(ctx, &b); err != nil {
			if errors.Is(err, kv.ErrConflict) {
				continue
			}
			return fmt.Errorf("write concurrency group: %w", err)
		}
		return nil
	}
	return fmt.Errorf("%w: concurrency group is busy", domain.ErrConflict)
}

// ListConcurrency returns every stored group with its marks.
func (c *CI) ListConcurrency(ctx context.Context) ([]ports.CIConcurrencyGroup, error) {
	type id struct {
		repo, group string
		implicit    bool
	}
	implicit := false
	snap := c.db.Snapshot(ctx)
	byID := map[id]*ports.CIConcurrencyGroup{}
	get := func(repoName, group string) *ports.CIConcurrencyGroup {
		repo, err := domain.ParseRepoName(repoName)
		if err != nil {
			return nil
		}
		g, ok := byID[id{repoName, group, implicit}]
		if !ok {
			g = &ports.CIConcurrencyGroup{Implicit: implicit, Repo: repo, Group: group, State: domain.CIConcurrencyGroupState{Marks: map[string]domain.CIConcurrencyMark{}}}
			byID[id{repoName, group, implicit}] = g
		}
		return g
	}
	for _, namespace := range []bool{false, true} {
		implicit = namespace
		prefix, markPrefix := concurPrefix, concurMarkPrefixAll
		if implicit {
			prefix, markPrefix = implicitConcurPrefix, implicitMarkPrefix
		}
		for key, item := range snap.Prefix(ctx, []byte(prefix)) {
			repoName, group, ok := strings.Cut(strings.TrimPrefix(string(key), prefix), "/")
			if !ok {
				continue
			}
			g := get(repoName, group)
			if g == nil {
				continue
			}
			rec, err := decodeConcurrencyRecord(item.Value)
			if err != nil {
				return nil, err
			}
			g.State.Record = rec
		}
		for key, item := range snap.Prefix(ctx, []byte(markPrefix)) {
			repoName, rest, ok := strings.Cut(strings.TrimPrefix(string(key), markPrefix), "/")
			if !ok {
				continue
			}
			escaped, _, ok := strings.Cut(rest, "/")
			if !ok {
				continue
			}
			group, err := url.PathUnescape(escaped)
			if err != nil {
				continue
			}
			g := get(repoName, group)
			if g == nil {
				continue
			}
			m, err := decodeConcurrencyMark(item.Value)
			if err != nil {
				return nil, err
			}
			g.State.Marks[m.Entry.Key()] = m
		}
	}
	out := make([]ports.CIConcurrencyGroup, 0, len(byID))
	for _, g := range byID {
		out = append(out, *g)
	}
	return out, nil
}
