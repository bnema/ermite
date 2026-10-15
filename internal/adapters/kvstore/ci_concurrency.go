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
	concurPrefix        = "concur/"
	concurMarkPrefixAll = "concurmark/"
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

func encodeConcurrency(c domain.CIConcurrency) *storagepb.CIConcurrency {
	if c == (domain.CIConcurrency{}) {
		return nil
	}
	out := &storagepb.CIConcurrency{Group: c.Group, Cancel: c.Cancel, Evaluated: c.Evaluated, FromRun: c.FromRun, Preempted: c.Preempted}
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
	c := domain.CIConcurrency{Group: pb.GetGroup(), Cancel: pb.GetCancel(), Evaluated: pb.GetEvaluated(), FromRun: pb.GetFromRun(), Preempted: pb.GetPreempted()}
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
	return &storagepb.CIConcurrencyEntry{RunId: e.RunID, Attempt: e.Attempt, JobId: e.JobID, Cancel: e.Cancel, Canceling: e.Canceling}
}

func encodeConcurrencyRecord(r domain.CIConcurrencyRecord) []byte {
	return mustMarshal(&storagepb.CIConcurrencyRecord{Running: encodeConcurrencyEntry(r.Running), Pending: encodeConcurrencyEntry(r.Pending)})
}

//hexcheck:ignore suspicious-business-logic-in-adapter maps the stored record to the domain record field by field: no rule is decided here
func decodeConcurrencyRecord(val []byte) (domain.CIConcurrencyRecord, error) {
	var pb storagepb.CIConcurrencyRecord
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.CIConcurrencyRecord{}, fmt.Errorf("decode concurrency group: %w", err)
	}
	return domain.CIConcurrencyRecord{Running: decodeConcurrencyEntry(pb.GetRunning()), Pending: decodeConcurrencyEntry(pb.GetPending())}, nil
}

func decodeConcurrencyEntry(pb *storagepb.CIConcurrencyEntry) *domain.CIConcurrencyEntry {
	if pb == nil {
		return nil
	}
	return &domain.CIConcurrencyEntry{RunID: pb.GetRunId(), Attempt: pb.GetAttempt(), JobID: pb.GetJobId(), Cancel: pb.GetCancel(), Canceling: pb.GetCanceling()}
}

// concurMarkPrefix is where the marks of a group are: the group key is escaped,
// so that the marks of "a" never fall under the prefix of "a/b".
func concurMarkPrefix(repo domain.RepoName, group string) string {
	return concurMarkPrefixAll + repo.String() + "/" + url.PathEscape(domain.ConcurrencyKey(group)) + "/"
}

func encodeConcurrencyMark(m domain.CIConcurrencyMark) []byte {
	pb := &storagepb.CIConcurrencyMark{Entry: encodeConcurrencyEntry(&m.Entry), Group: m.Group}
	for _, e := range concurrencyStates {
		if e.d == m.State {
			pb.State = e.p
		}
	}
	if m.State == domain.CIConcurrencyReplaced {
		pb.State = storagepb.CIConcurrencyState_CI_CONCURRENCY_STATE_REPLACED
	}
	return mustMarshal(pb)
}

//hexcheck:ignore suspicious-business-logic-in-adapter maps the stored mark to the domain mark field by field: no rule is decided here
func decodeConcurrencyMark(val []byte) (domain.CIConcurrencyMark, error) {
	var pb storagepb.CIConcurrencyMark
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.CIConcurrencyMark{}, fmt.Errorf("decode concurrency mark: %w", err)
	}
	m := domain.CIConcurrencyMark{Group: pb.GetGroup()}
	if e := decodeConcurrencyEntry(pb.GetEntry()); e != nil {
		m.Entry = *e
	}
	switch pb.GetState() {
	case storagepb.CIConcurrencyState_CI_CONCURRENCY_STATE_PENDING:
		m.State = domain.CIConcurrencyPending
	case storagepb.CIConcurrencyState_CI_CONCURRENCY_STATE_HOLDING:
		m.State = domain.CIConcurrencyHolding
	case storagepb.CIConcurrencyState_CI_CONCURRENCY_STATE_REPLACED:
		m.State = domain.CIConcurrencyReplaced
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
//
//hexcheck:ignore suspicious-business-logic-in-adapter compare-and-swap loop over a record and its marks in one batch: the rule is fn, decided by the caller
func (c *CI) UpdateConcurrency(ctx context.Context, repo domain.RepoName, group string, fn func(*domain.CIConcurrencyGroupState) error) error {
	key, mprefix := concurKey(repo, group), concurMarkPrefix(repo, group)
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
//
//hexcheck:ignore suspicious-business-logic-in-adapter lists and decodes records and marks: no rule is decided here
func (c *CI) ListConcurrency(ctx context.Context) ([]ports.CIConcurrencyGroup, error) {
	type id struct{ repo, group string }
	snap := c.db.Snapshot(ctx)
	byID := map[id]*ports.CIConcurrencyGroup{}
	get := func(repoName, group string) *ports.CIConcurrencyGroup {
		repo, err := domain.ParseRepoName(repoName)
		if err != nil {
			return nil
		}
		g, ok := byID[id{repoName, group}]
		if !ok {
			g = &ports.CIConcurrencyGroup{Repo: repo, Group: group, State: domain.CIConcurrencyGroupState{Marks: map[string]domain.CIConcurrencyMark{}}}
			byID[id{repoName, group}] = g
		}
		return g
	}
	for key, item := range snap.Prefix(ctx, []byte(concurPrefix)) {
		repoName, group, ok := strings.Cut(strings.TrimPrefix(string(key), concurPrefix), "/")
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
	for key, item := range snap.Prefix(ctx, []byte(concurMarkPrefixAll)) {
		repoName, rest, ok := strings.Cut(strings.TrimPrefix(string(key), concurMarkPrefixAll), "/")
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
	out := make([]ports.CIConcurrencyGroup, 0, len(byID))
	for _, g := range byID {
		out = append(out, *g)
	}
	return out, nil
}
