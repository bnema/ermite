// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
)

func entry(run int64) *domain.CIConcurrencyEntry {
	return &domain.CIConcurrencyEntry{RunID: run, Attempt: 1}
}

// update runs one transaction on a group.
func update(t *testing.T, ci interface {
	UpdateConcurrency(context.Context, domain.RepoName, string, func(*domain.CIConcurrencyGroupState) error) error
}, n domain.RepoName, group string, fn func(*domain.CIConcurrencyGroupState)) {
	t.Helper()
	require.NoError(t, ci.UpdateConcurrency(context.Background(), n, group, func(st *domain.CIConcurrencyGroupState) error {
		fn(st)
		return nil
	}))
}

type txer interface {
	UpdateConcurrency(context.Context, domain.RepoName, string, func(*domain.CIConcurrencyGroupState) error) error
}

func read(t *testing.T, ci txer, n domain.RepoName, group string) (st domain.CIConcurrencyGroupState) {
	t.Helper()
	update(t, ci, n, group, func(s *domain.CIConcurrencyGroupState) { st = *s })
	return st
}

func markOf(e domain.CIConcurrencyEntry, state domain.CIConcurrencyState) domain.CIConcurrencyMark {
	return domain.CIConcurrencyMark{Entry: e, Group: "Deploy", State: state}
}

// A group is one record per repository, compared with domain.ConcurrencyKey, and
// an empty group (no record, no mark) leaves no key.
func TestCIConcurrencyGroupRecord(t *testing.T) {
	_, ci, n := ciRepo(t)
	update(t, ci, n, "Deploy", func(s *domain.CIConcurrencyGroupState) {
		s.Record.Running, s.Record.Pending = entry(1), []domain.CIConcurrencyEntry{{RunID: 2, Attempt: 1, JobID: "2-0-0", Cancel: true}}
	})
	got := read(t, ci, n, "DEPLOY")
	require.NotNil(t, got.Record.Running)
	require.Len(t, got.Record.Pending, 1)
	assert.EqualValues(t, 1, got.Record.Running.RunID)
	assert.Equal(t, "2-0-0", got.Record.Pending[0].JobID)
	assert.True(t, got.Record.Pending[0].Cancel)
	assert.True(t, read(t, ci, n, "other").IsEmpty(), "another group is another record")

	update(t, ci, n, "deploy", func(s *domain.CIConcurrencyGroupState) { *s = domain.CIConcurrencyGroupState{} })
	assert.True(t, read(t, ci, n, "Deploy").IsEmpty())
	groups, err := ci.ListConcurrency(context.Background())
	require.NoError(t, err)
	assert.Empty(t, groups, "no key is left")
}

// The record and the marks of its entries are written by one batch, and a mark
// that a transaction removes goes in the same batch.
func TestCIConcurrencyMarksAreWrittenWithTheRecord(t *testing.T) {
	_, ci, n := ciRepo(t)
	run, wait := *entry(1), *entry(2)
	update(t, ci, n, "Deploy", func(s *domain.CIConcurrencyGroupState) {
		s.Record.Running, s.Record.Pending = &run, []domain.CIConcurrencyEntry{wait}
		s.Marks = map[string]domain.CIConcurrencyMark{
			run.Key():  markOf(run, domain.CIConcurrencyHolding),
			wait.Key(): markOf(wait, domain.CIConcurrencyPending),
		}
	})
	got := read(t, ci, n, "deploy")
	assert.Equal(t, domain.CIConcurrencyHolding, got.Marks[run.Key()].State)
	assert.Equal(t, domain.CIConcurrencyPending, got.Marks[wait.Key()].State)
	assert.Equal(t, "Deploy", got.Marks[wait.Key()].Group)

	// The waiting entry is replaced: unnamed and marked in one transaction.
	update(t, ci, n, "Deploy", func(s *domain.CIConcurrencyGroupState) {
		s.Record.Pending = []domain.CIConcurrencyEntry{*entry(3)}
		s.Marks[wait.Key()] = markOf(wait, domain.CIConcurrencyReplaced)
		s.Marks[entry(3).Key()] = markOf(*entry(3), domain.CIConcurrencyPending)
	})
	got = read(t, ci, n, "Deploy")
	require.Len(t, got.Record.Pending, 1)
	assert.EqualValues(t, 3, got.Record.Pending[0].RunID)
	assert.Equal(t, domain.CIConcurrencyReplaced, got.Marks[wait.Key()].State)
	assert.Len(t, got.Marks, 3)

	// A group with marks only (nothing named) is kept, and listed with them.
	update(t, ci, n, "Deploy", func(s *domain.CIConcurrencyGroupState) {
		s.Record = domain.CIConcurrencyRecord{}
		delete(s.Marks, run.Key())
		delete(s.Marks, entry(3).Key())
	})
	groups, err := ci.ListConcurrency(context.Background())
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, domain.ConcurrencyKey("Deploy"), groups[0].Group)
	assert.Equal(t, n, groups[0].Repo)
	assert.True(t, groups[0].State.Record.IsEmpty())
	assert.Equal(t, domain.CIConcurrencyReplaced, groups[0].State.Marks[wait.Key()].State)

	update(t, ci, n, "Deploy", func(s *domain.CIConcurrencyGroupState) { delete(s.Marks, wait.Key()) })
	groups, err = ci.ListConcurrency(context.Background())
	require.NoError(t, err)
	assert.Empty(t, groups, "no record and no mark: no key")
}

// The marks of "a" are not the marks of "a/b": a group may hold a slash.
func TestCIConcurrencyMarksBelongToTheirOwnGroup(t *testing.T) {
	_, ci, n := ciRepo(t)
	for _, g := range []string{"a", "a/b"} {
		e := *entry(int64(len(g)))
		update(t, ci, n, g, func(s *domain.CIConcurrencyGroupState) {
			s.Record.Running = &e
			s.Marks = map[string]domain.CIConcurrencyMark{e.Key(): {Entry: e, Group: g, State: domain.CIConcurrencyHolding}}
		})
	}
	a, ab := read(t, ci, n, "a"), read(t, ci, n, "a/b")
	assert.Len(t, a.Marks, 1)
	assert.Len(t, ab.Marks, 1)
	assert.Equal(t, "a", a.Marks["1/1/"].Group)
	assert.Equal(t, "a/b", ab.Marks["3/1/"].Group)
}

// Two writers that both read an empty group never both claim it: the batch of
// the second carries the precondition of the state it read, so it conflicts and
// its callback runs again on the winner's record.
func TestCIConcurrencyGroupCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)

	// Both first callbacks see the empty group before either write happens.
	var arrived sync.WaitGroup
	arrived.Add(2)
	type writer struct {
		claimed bool // reset by every call: it is the outcome of the last one
		calls   int
	}
	ws := []*writer{{}, {}}
	var wg sync.WaitGroup
	for i, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := ci.UpdateConcurrency(ctx, n, "g", func(s *domain.CIConcurrencyGroupState) error {
				w.calls++
				w.claimed = false
				if w.calls == 1 {
					assert.Nil(t, s.Record.Running, "the first call sees the empty group")
					arrived.Done()
					arrived.Wait()
				}
				if s.Record.Running == nil {
					e := *entry(int64(i + 1))
					s.Record.Running, w.claimed = &e, true
					s.Marks = map[string]domain.CIConcurrencyMark{e.Key(): markOf(e, domain.CIConcurrencyHolding)}
				}
				return nil
			})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	claimed, winner := 0, int64(0)
	for i, w := range ws {
		if w.claimed {
			claimed, winner = claimed+1, int64(i+1)
		}
	}
	assert.Equal(t, 1, claimed, "exactly one writer holds the group")
	assert.Equal(t, 3, ws[0].calls+ws[1].calls, "the loser's callback ran again on the winner's record")
	final := read(t, ci, n, "g")
	require.NotNil(t, final.Record.Running)
	assert.Equal(t, winner, final.Record.Running.RunID, "the stored owner is the one that claimed")
	assert.Len(t, final.Marks, 1, "and it alone is marked")
}

// A transaction that only changes a mark still conflicts with one that changed
// the record meanwhile: the mark rewrites the record, the version gate.
func TestCIConcurrencyAMarkChangeIsGatedByTheRecord(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	update(t, ci, n, "g", func(s *domain.CIConcurrencyGroupState) {
		s.Record.Running = entry(1)
		s.Marks = map[string]domain.CIConcurrencyMark{entry(1).Key(): markOf(*entry(1), domain.CIConcurrencyHolding)}
	})
	calls := 0
	require.NoError(t, ci.UpdateConcurrency(ctx, n, "g", func(s *domain.CIConcurrencyGroupState) error {
		calls++
		if calls == 1 {
			update(t, ci, n, "g", func(o *domain.CIConcurrencyGroupState) { o.Record.Pending = []domain.CIConcurrencyEntry{*entry(2)} })
		}
		m := s.Marks[entry(1).Key()]
		m.State = domain.CIConcurrencyReplaced
		s.Marks[entry(1).Key()] = m
		return nil
	}))
	assert.Equal(t, 2, calls, "the first attempt was made on a stale record")
	got := read(t, ci, n, "g")
	require.Len(t, got.Record.Pending, 1, "the concurrent change is kept")
	assert.Equal(t, domain.CIConcurrencyReplaced, got.Marks[entry(1).Key()].State)
}

// A callback that leaves the group as it was writes nothing, but its batch
// still carries the version precondition: a writer that got in meanwhile makes
// it run again.
func TestCIConcurrencyUnchangedRecordKeepsItsPrecondition(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	calls := 0
	var second domain.CIConcurrencyGroupState
	err := ci.UpdateConcurrency(ctx, n, "g", func(s *domain.CIConcurrencyGroupState) error {
		calls++
		if calls == 1 {
			// Another writer creates the group while this decision is made.
			update(t, ci, n, "g", func(o *domain.CIConcurrencyGroupState) { o.Record.Running = entry(7) })
		} else {
			second = *s
		}
		return nil // unchanged
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "the unchanged decision was made on stale state: it ran again")
	require.NotNil(t, second.Record.Running)
	assert.EqualValues(t, 7, second.Record.Running.RunID)
}

// fn may change the entries it is given in place: the update is still written,
// because the state is compared with its bytes as read, not with a shallow
// copy that shares the entries.
func TestCIConcurrencyInPlaceMutationIsWritten(t *testing.T) {
	_, ci, n := ciRepo(t)
	update(t, ci, n, "g", func(s *domain.CIConcurrencyGroupState) {
		s.Record.Running, s.Record.Pending = entry(1), []domain.CIConcurrencyEntry{*entry(2)}
	})

	update(t, ci, n, "g", func(s *domain.CIConcurrencyGroupState) {
		s.Record.Running.Canceling = true // through the pointer, the struct is returned as given
	})
	got := read(t, ci, n, "g")
	require.NotNil(t, got.Record.Running)
	assert.True(t, got.Record.Running.Canceling, "the in-place change of the running entry is stored")

	update(t, ci, n, "g", func(s *domain.CIConcurrencyGroupState) {
		s.Record.Pending[0].JobID, s.Record.Pending[0].Cancel = "2-0-0", true
	})
	got = read(t, ci, n, "g")
	require.Len(t, got.Record.Pending, 1)
	assert.Equal(t, "2-0-0", got.Record.Pending[0].JobID)
	assert.True(t, got.Record.Pending[0].Cancel)
}

// ListConcurrency returns every group with its repository and canonical key.
func TestCIConcurrencyListGroups(t *testing.T) {
	_, ci, n := ciRepo(t)
	for i, g := range []string{"Deploy", "Build/Linux"} {
		update(t, ci, n, g, func(s *domain.CIConcurrencyGroupState) { s.Record.Running = entry(int64(i + 1)) })
	}
	got, err := ci.ListConcurrency(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)
	keys := []string{got[0].Group, got[1].Group}
	assert.ElementsMatch(t, []string{"DEPLOY", "BUILD/LINUX"}, keys, "a group may hold a slash")
	assert.Equal(t, n, got[0].Repo)
	assert.NotNil(t, got[0].State.Record.Running)
}

// Groups and their marks go with their repository, and nothing is created under
// one that is gone.
func TestCIConcurrencyGroupsAreDeletedWithTheRepository(t *testing.T) {
	ctx := context.Background()
	s, ci, n := ciRepo(t)
	update(t, ci, n, "g", func(st *domain.CIConcurrencyGroupState) {
		st.Record.Running = entry(1)
		st.Marks = map[string]domain.CIConcurrencyMark{entry(1).Key(): markOf(*entry(1), domain.CIConcurrencyHolding)}
	})
	require.NoError(t, s.Delete(ctx, n))
	err := ci.UpdateConcurrency(ctx, n, "g", func(st *domain.CIConcurrencyGroupState) error {
		st.Record.Running = entry(2)
		return nil
	})
	assert.ErrorIs(t, err, domain.ErrNotFound, "no key is created under a repository that is gone")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
	assert.True(t, read(t, ci, n, "g").IsEmpty(), "the repository created again starts without groups or marks")
	groups, err := ci.ListConcurrency(ctx)
	require.NoError(t, err)
	assert.Empty(t, groups)
}

// queue: max: the waiting line is stored in order with the queue mode of each
// entry, and a rejected mark is kept as such.
func TestCIConcurrencyQueueRoundTrip(t *testing.T) {
	_, ci, n := ciRepo(t)
	line := []domain.CIConcurrencyEntry{
		{RunID: 4, Attempt: 1, Max: true},
		{RunID: 2, Attempt: 1, JobID: "2-0-0", Max: true},
		{RunID: 3, Attempt: 2},
	}
	rejected := domain.CIConcurrencyEntry{RunID: 5, Attempt: 1, Max: true}
	update(t, ci, n, "Deploy", func(s *domain.CIConcurrencyGroupState) {
		s.Record.Running, s.Record.Pending = entry(1), line
		s.Marks = map[string]domain.CIConcurrencyMark{rejected.Key(): markOf(rejected, domain.CIConcurrencyRejected)}
	})
	got := read(t, ci, n, "deploy")
	assert.Equal(t, line, got.Record.Pending, "the order and the queue modes are kept")
	assert.Equal(t, domain.CIConcurrencyRejected, got.Marks[rejected.Key()].State)
	assert.True(t, got.Marks[rejected.Key()].Entry.Max)

	// Stored as replaced with queue_full: a binary that predates it still
	// cancels the entry.
	var pb storagepb.CIConcurrencyMark
	require.NoError(t, proto.Unmarshal(kvstore.EncodeConcurrencyMark(markOf(rejected, domain.CIConcurrencyRejected)), &pb))
	assert.Equal(t, storagepb.CIConcurrencyState_CI_CONCURRENCY_STATE_REPLACED, pb.GetState())
	assert.True(t, pb.GetQueueFull())
}

// A record stored before queue: max holds its only waiting entry in the
// pending field: it reads as a line of one, and is written back in the new
// layout.
func TestCIConcurrencyRecordOldLayout(t *testing.T) {
	old, err := proto.Marshal(&storagepb.CIConcurrencyRecord{
		Running: &storagepb.CIConcurrencyEntry{RunId: 1, Attempt: 1},
		Pending: &storagepb.CIConcurrencyEntry{RunId: 2, Attempt: 1, JobId: "2-0-0", Cancel: true},
	})
	require.NoError(t, err)
	rec, err := kvstore.DecodeConcurrencyRecord(old)
	require.NoError(t, err)
	require.NotNil(t, rec.Running)
	assert.EqualValues(t, 1, rec.Running.RunID)
	assert.Equal(t, []domain.CIConcurrencyEntry{{RunID: 2, Attempt: 1, JobID: "2-0-0", Cancel: true}}, rec.Pending)

	var pb storagepb.CIConcurrencyRecord
	require.NoError(t, proto.Unmarshal(kvstore.EncodeConcurrencyRecord(rec), &pb))
	assert.EqualValues(t, 2, pb.GetPending().GetRunId(), "the head of the line is written to the old field too, for a downgrade")
	assert.Len(t, pb.GetQueue(), 1)

	// A line of two: an older binary sees its head only.
	rec.Pending = append(rec.Pending, domain.CIConcurrencyEntry{RunID: 3, Attempt: 1, Max: true})
	pb.Reset()
	require.NoError(t, proto.Unmarshal(kvstore.EncodeConcurrencyRecord(rec), &pb))
	assert.EqualValues(t, 2, pb.GetPending().GetRunId())
	assert.Len(t, pb.GetQueue(), 2)
	back, err := kvstore.DecodeConcurrencyRecord(kvstore.EncodeConcurrencyRecord(rec))
	require.NoError(t, err)
	assert.Equal(t, rec, back, "the queue wins over the old field")
}

func TestCIImplicitConcurrencyNamespace(t *testing.T) {
	_, ci, n := ciRepo(t)
	ctx := context.Background()
	const text = `[".github/workflows/ci.yml","push","refs/heads/main"]`
	update(t, ci, n, text, func(st *domain.CIConcurrencyGroupState) { st.Record.Running = entry(1) })
	require.NoError(t, ci.UpdateImplicitConcurrency(ctx, n, text, func(st *domain.CIConcurrencyGroupState) error {
		assert.True(t, st.IsEmpty(), "identical user text cannot reach server namespace")
		st.Record.Running = &domain.CIConcurrencyEntry{RunID: 2, Attempt: 1, Implicit: true}
		return nil
	}))
	assert.EqualValues(t, 1, read(t, ci, n, text).Record.Running.RunID)
	groups, err := ci.ListConcurrency(ctx)
	require.NoError(t, err)
	partitions := map[bool][]domain.CIConcurrencyEntry{}
	for _, g := range groups {
		require.NotNil(t, g.State.Record.Running)
		partitions[g.Implicit] = append(partitions[g.Implicit], *g.State.Record.Running)
	}
	require.Len(t, partitions[false], 1, "exactly one user group")
	require.Len(t, partitions[true], 1, "exactly one implicit group")
	assert.EqualValues(t, 1, partitions[false][0].RunID)
	assert.False(t, partitions[false][0].Implicit)
	assert.EqualValues(t, 2, partitions[true][0].RunID)
	assert.True(t, partitions[true][0].Implicit)
}
