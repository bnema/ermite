// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

type journalRig struct {
	j       *usecase.Journal
	store   *mocks.JournalStore
	git     *mocks.GitRepos
	commits *mocks.MergeGit
	bus     *mocks.EventBus
	at      time.Time
	got     []domain.JournalEntry
}

func newJournalRig(t *testing.T) *journalRig {
	t.Helper()
	r := &journalRig{
		store: mocks.NewJournalStore(t), git: mocks.NewGitRepos(t), commits: mocks.NewMergeGit(t), bus: mocks.NewEventBus(t),
		at: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC),
	}
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().RunAndReturn(func() time.Time { return r.at }).Maybe()
	r.store.EXPECT().Append(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, e domain.JournalEntry) error {
		r.got = append(r.got, e)
		return nil
	}).Maybe()
	r.store.EXPECT().Prune(mock.Anything, mock.Anything).Return(0, nil).Maybe()
	r.j = usecase.NewJournal(r.store, r.git, r.commits, r.bus, clock)
	return r
}

// hexOID is the text of oid(c), as a ref update holds it.
func hexOID(c string) string { return oid(c).String() }

// A push records its tags and what it brought to the default branch, and
// nothing of the other branches; one signal tells the inbox.
func TestJournalOnPushRecordsTagsAndTheDefaultBranch(t *testing.T) {
	t.Parallel()
	r := newJournalRig(t)
	ctx := context.Background()
	repo := name(t, "proj")
	r.git.EXPECT().HeadBranch(mock.Anything, repo).Return("main", nil).Once()
	r.commits.EXPECT().ListRange(mock.Anything, repo, oid("1"), oid("2"), domain.MaxJournalCommits).
		Return([]domain.Commit{{ID: oid("2"), Summary: "fix the build"}, {ID: oid("3")}}, false, nil).Once()
	r.bus.EXPECT().Publish(mock.Anything, domain.JournalUpdated{Repo: repo, At: r.at}).Once()

	r.j.OnPush(ctx, "owner", repo, []domain.RefUpdate{
		{Old: hexOID("1"), New: hexOID("2"), Ref: "refs/heads/main"},
		{Old: hexOID("4"), New: hexOID("5"), Ref: "refs/heads/feature"}, // not the default branch
		{Old: domain.ZeroOID, New: hexOID("6"), Ref: "refs/tags/v1.0.0"},
		{Old: hexOID("7"), New: domain.ZeroOID, Ref: "refs/tags/v0.9.0"},
		{Old: hexOID("7"), New: hexOID("8"), Ref: "refs/tags/moved"}, // neither created nor deleted
		{Old: domain.ZeroOID, New: hexOID("9"), Ref: "refs/pull/1/head"},
	})

	require.Len(t, r.got, 3)
	assert.Equal(t, domain.JournalEntry{
		At: r.at, Repo: repo, Kind: domain.JournalDefaultBranchPush, Actor: "owner", Ref: "main",
		Commits: 2, Commit: oid("2"), Summary: "fix the build",
	}, r.got[0])
	assert.Equal(t, domain.JournalEntry{At: r.at, Repo: repo, Kind: domain.JournalTagCreated, Actor: "owner", Ref: "v1.0.0", Commit: oid("6")}, r.got[1])
	assert.Equal(t, domain.JournalEntry{At: r.at, Repo: repo, Kind: domain.JournalTagDeleted, Actor: "owner", Ref: "v0.9.0"}, r.got[2])
}

func TestJournalOnPushIgnoresOtherBranches(t *testing.T) {
	t.Parallel()
	r := newJournalRig(t) // the bus is never told
	repo := name(t, "proj")
	r.git.EXPECT().HeadBranch(mock.Anything, repo).Return("main", nil).Once()
	r.j.OnPush(context.Background(), "owner", repo, []domain.RefUpdate{
		{Old: hexOID("1"), New: hexOID("2"), Ref: "refs/heads/feature"},
		{Old: hexOID("1"), New: domain.ZeroOID, Ref: "refs/heads/main"}, // a deleted branch brings nothing
	})
	assert.Empty(t, r.got)
}

func TestJournalOnPushCountsANewDefaultBranchAndSkipsARewind(t *testing.T) {
	t.Parallel()
	r := newJournalRig(t)
	ctx := context.Background()
	repo := name(t, "proj")
	r.git.EXPECT().HeadBranch(mock.Anything, repo).Return("main", nil)
	many := make([]domain.Commit, domain.MaxJournalCommits)
	r.commits.EXPECT().ListRange(mock.Anything, repo, domain.ObjectID{}, oid("2"), domain.MaxJournalCommits).Return(many, true, nil).Once()
	r.bus.EXPECT().Publish(mock.Anything, mock.AnythingOfType("domain.JournalUpdated")).Once()
	r.j.OnPush(ctx, "bot", repo, []domain.RefUpdate{{Old: domain.ZeroOID, New: hexOID("2"), Ref: "refs/heads/main"}})
	require.Len(t, r.got, 1)
	assert.Equal(t, domain.MaxJournalCommits, r.got[0].Commits)
	assert.True(t, r.got[0].More, "a new branch counts its history, bounded")

	r.got = nil
	r.commits.EXPECT().ListRange(mock.Anything, repo, oid("3"), oid("4"), domain.MaxJournalCommits).Return(nil, false, nil).Once()
	r.j.OnPush(ctx, "bot", repo, []domain.RefUpdate{{Old: hexOID("3"), New: hexOID("4"), Ref: "refs/heads/main"}})
	assert.Empty(t, r.got, "a force-push back brings no commit")
}

func TestJournalRecordLogsFailuresAndPrunesHourly(t *testing.T) {
	t.Parallel()
	store, clock := mocks.NewJournalStore(t), mocks.NewClock(t)
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().RunAndReturn(func() time.Time { return at })
	j := usecase.NewJournal(store, mocks.NewGitRepos(t), mocks.NewMergeGit(t), mocks.NewEventBus(t), clock)
	e := domain.JournalEntry{Repo: name(t, "proj"), Kind: domain.JournalIssueOpened, Number: 1}

	store.EXPECT().Append(mock.Anything, domain.JournalEntry{At: at, Repo: e.Repo, Kind: e.Kind, Number: 1}).Return(errors.New("disk full")).Once()
	store.EXPECT().Prune(mock.Anything, at.Add(-domain.JournalRetention)).Return(3, nil).Once()
	j.Record(context.Background(), e) // a failure is logged, never returned

	at = at.Add(30 * time.Minute)
	store.EXPECT().Append(mock.Anything, mock.Anything).Return(nil).Once()
	j.Record(context.Background(), e) // no prune within the hour

	at = at.Add(31 * time.Minute)
	store.EXPECT().Append(mock.Anything, mock.Anything).Return(nil).Once()
	store.EXPECT().Prune(mock.Anything, at.Add(-domain.JournalRetention)).Return(0, nil).Once()
	j.Record(context.Background(), e)
}
