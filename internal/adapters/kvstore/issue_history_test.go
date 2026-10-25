// SPDX-License-Identifier: AGPL-3.0-or-later
package kvstore_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIssueHistoryAtomicAndCapped(t *testing.T) {
	ctx := context.Background()
	s, _, repo := repoWithMRs(t)
	is := s.Issues()
	issue, err := is.Create(ctx, newIssue(t, repo, "history"))
	require.NoError(t, err)
	stale := issue
	issue.State = domain.IssueStateResolved
	ev := domain.IssueEvent{Kind: domain.IssueEventResolved, Actor: "merger", MergeRequest: 9, CreatedAt: created}
	require.NoError(t, is.Update(ctx, issue, ev, domain.IssueEvent{Kind: domain.IssueEventReopened, CreatedAt: created.Add(time.Second)}))
	assert.ErrorIs(t, is.Update(ctx, stale, ev), domain.ErrConflict)
	events, err := is.ListEvents(ctx, repo, issue.Number)
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, int64(1), events[0].ID)
	assert.Equal(t, int64(2), events[1].ID)
	assert.Equal(t, issue.Number, events[0].Issue)
	assert.Equal(t, "merger", events[0].Actor)
	open, err := is.ListOpenAll(ctx)
	require.NoError(t, err)
	assert.Empty(t, open)
	page, err := is.List(ctx, repo, ports.ListIssuesParams{State: domain.IssueStateResolved})
	require.NoError(t, err)
	require.Len(t, page.Issues, 1)
	issue = page.Issues[0]
	batch := make([]domain.IssueEvent, kvstore.MaxIssueEvents)
	for j := range batch {
		batch[j] = ev
	}
	require.NoError(t, is.Update(ctx, issue, batch...))
	issue, err = is.Get(ctx, repo, issue.Number)
	require.NoError(t, err)
	issue.State = domain.IssueStateOpen
	require.NoError(t, is.Update(ctx, issue, ev))
	events, err = is.ListEvents(ctx, repo, issue.Number)
	require.NoError(t, err)
	require.Len(t, events, kvstore.MaxIssueEvents)
	for j, e := range events {
		assert.Equal(t, int64(j+1), e.ID)
	}
	issue, err = is.Get(ctx, repo, issue.Number)
	require.NoError(t, err)
	assert.Equal(t, domain.IssueStateOpen, issue.State)
	require.NoError(t, s.Delete(ctx, repo))
	for _, prefix := range []string{"is/", "isc/", "isopen/", "isev/", "mrev/"} {
		assert.Zero(t, countKeys(t, s, prefix+repo.String()+"/"))
	}
}

func TestReferenceHistoryDiff(t *testing.T) {
	ctx := context.Background()
	s, _, repo := repoWithMRs(t)
	links := s.IssueLinks()
	ref := ports.ReferenceEvent{Actor: "writer", At: created}
	replace := func(issues, closes []int64) {
		t.Helper()
		_, _, err := links.ReplaceLinks(ctx, repo, 9, issues, closes, ref)
		require.NoError(t, err)
	}
	replace([]int64{3}, nil)
	replace([]int64{3}, nil)
	ref.At = created.Add(time.Second)
	replace([]int64{3}, []int64{3})
	replace([]int64{3}, []int64{3})
	replace([]int64{3}, nil) // downgrade has no event
	replace(nil, nil)        // removal retains history
	events, err := s.Issues().ListEvents(ctx, repo, 3)
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.False(t, events[0].Closes)
	assert.True(t, events[1].Closes)
	assert.Equal(t, "writer", events[0].Actor)
	assert.Equal(t, domain.IssueEventReferenced, events[0].Kind)
	mrEvents, err := links.EventsOfMergeRequest(ctx, repo, 9)
	require.NoError(t, err)
	assert.Equal(t, events, mrEvents)
	replace([]int64{3}, nil) // reappearance is another event
	events, err = s.Issues().ListEvents(ctx, repo, 3)
	require.NoError(t, err)
	require.Len(t, events, 3)
	assert.Equal(t, int64(3), events[2].ID)
	require.NoError(t, s.Delete(ctx, repo))
	assert.Zero(t, countKeys(t, s, "isev/"+repo.String()+"/"))
	assert.Zero(t, countKeys(t, s, "mrev/"+repo.String()+"/"))
}

func TestMRHistoryOrderAndCap(t *testing.T) {
	ctx := context.Background()
	s, _, repo := repoWithMRs(t)
	// Insert in reverse timestamp order; issue and seq break timestamp ties.
	for n := int64(1); n <= 3; n++ {
		issue, err := s.Issues().Create(ctx, newIssue(t, repo, "many"))
		require.NoError(t, err)
		events := make([]domain.IssueEvent, 201)
		for j := range events {
			events[j] = domain.IssueEvent{Kind: domain.IssueEventResolved, MergeRequest: 9, CreatedAt: created.Add(time.Duration(200-j) * time.Second)}
		}
		require.NoError(t, s.Issues().Update(ctx, issue, events...))
	}
	events, err := s.IssueLinks().EventsOfMergeRequest(ctx, repo, 9)
	require.NoError(t, err)
	require.Len(t, events, 500)
	for j := 1; j < len(events); j++ {
		a, b := events[j-1], events[j]
		assert.False(t, a.CreatedAt.After(b.CreatedAt))
		if a.CreatedAt.Equal(b.CreatedAt) {
			assert.Less(t, a.Issue, b.Issue)
		}
	}
}

func TestResolvedMigrationAndRestart(t *testing.T) {
	ctx := context.Background()
	for _, version := range []uint32{5, 7, 8} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			dir := t.TempDir()
			putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: version}))
			for n := int64(1); n <= 3; n++ {
				state := storagepb.IssueState_ISSUE_STATE_CLOSED
				mr := int64(0)
				if n == 1 {
					mr = 9
				}
				if n == 3 {
					state = storagepb.IssueState_ISSUE_STATE_OPEN
				}
				putRaw(t, dir, fmt.Sprintf("is/proj/%010d", n), mustProto(t, &storagepb.Issue{Number: n, Title: "old", State: state, ClosedByMergeRequest: mr}))
			}
			putRaw(t, dir, "isopen/proj/0000000003", []byte{1})
			repo := name(t, "proj")
			var migratedVersion uint64
			for pass := 0; pass < 2; pass++ {
				s, err := kvstore.Open(ctx, dir)
				require.NoError(t, err)
				for n, state := range []domain.IssueState{domain.IssueStateResolved, domain.IssueStateClosed, domain.IssueStateOpen} {
					issue, err := s.Issues().Get(ctx, repo, int64(n+1))
					require.NoError(t, err)
					if version == 8 && n == 0 {
						// Schema 8 already ran the rewrite; 8→9 only bumps the version.
						state = domain.IssueStateClosed
					}
					assert.Equal(t, state, issue.State)
					assert.True(t, issue.UpdatedAt.IsZero())
					if n == 0 {
						assert.Equal(t, int64(9), issue.ClosedByMergeRequest)
						if pass == 0 {
							migratedVersion = issue.Version
						} else {
							assert.Equal(t, migratedVersion, issue.Version)
						}
					}
				}
				assert.Equal(t, 1, countKeys(t, s, "isopen/proj/"))
				assert.Zero(t, countKeys(t, s, "isev/"))
				assert.Zero(t, countKeys(t, s, "mrev/"))
				require.NoError(t, s.Close(ctx))
				assert.Equal(t, uint32(kvstore.SchemaVersion), readSchema(t, dir))
			}
		})
	}
}

func TestIssueEventSequenceSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo := name(t, "proj")
	for pass := 0; pass < 2; pass++ {
		s, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		if pass == 0 {
			require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, Visibility: domain.VisibilityPrivate}))
			_, err := s.Issues().Create(ctx, newIssue(t, repo, "restart"))
			require.NoError(t, err)
		}
		issue, err := s.Issues().Get(ctx, repo, 1)
		require.NoError(t, err)
		require.NoError(t, s.Issues().Update(ctx, issue, domain.IssueEvent{Kind: domain.IssueEventClosed, MergeRequest: 9, CreatedAt: created}))
		events, err := s.IssueLinks().EventsOfMergeRequest(ctx, repo, 9)
		require.NoError(t, err)
		require.Len(t, events, pass+1)
		assert.Equal(t, int64(pass+1), events[pass].ID)
		require.NoError(t, s.Close(ctx))
	}
}

func TestLinkEventsDropAtCapWithoutChangingIssueVersion(t *testing.T) {
	ctx := context.Background()
	s, _, repo := repoWithMRs(t)
	issue, err := s.Issues().Create(ctx, newIssue(t, repo, "full"))
	require.NoError(t, err)
	events := make([]domain.IssueEvent, kvstore.MaxIssueEvents)
	for j := range events {
		events[j].Kind = domain.IssueEventClosed
	}
	require.NoError(t, s.Issues().Update(ctx, issue, events...))
	before, err := s.Issues().Get(ctx, repo, issue.Number)
	require.NoError(t, err)
	added, _, err := s.IssueLinks().ReplaceLinks(ctx, repo, 9, []int64{issue.Number}, []int64{issue.Number}, ports.ReferenceEvent{Actor: "writer", At: created})
	require.NoError(t, err)
	require.Len(t, added, 1)
	after, err := s.Issues().Get(ctx, repo, issue.Number)
	require.NoError(t, err)
	assert.Equal(t, before.Version, after.Version)
	history, err := s.Issues().ListEvents(ctx, repo, issue.Number)
	require.NoError(t, err)
	require.Len(t, history, kvstore.MaxIssueEvents)
	mrHistory, err := s.IssueLinks().EventsOfMergeRequest(ctx, repo, 9)
	require.NoError(t, err)
	assert.Empty(t, mrHistory)
	links, err := s.IssueLinks().LinksOfMergeRequest(ctx, repo, 9)
	require.NoError(t, err)
	assert.Equal(t, added, links)
}

func TestEditedIssueHistoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _, repo := repoWithMRs(t)
	issue, err := s.Issues().Create(ctx, newIssue(t, repo, "before"))
	require.NoError(t, err)
	issue.Title, err = domain.ParseIssueTitle("after")
	require.NoError(t, err)
	require.NoError(t, s.Issues().Update(ctx, issue, domain.IssueEvent{Kind: domain.IssueEventEdited, Actor: "owner", ChangedFields: []string{"title", "body"}, CreatedAt: created}))
	got, err := s.Issues().Get(ctx, repo, issue.Number)
	require.NoError(t, err)
	assert.Equal(t, "after", got.Title.String())
	events, err := s.Issues().ListEvents(ctx, repo, issue.Number)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, domain.IssueEventEdited, events[0].Kind)
	assert.Equal(t, []string{"title", "body"}, events[0].ChangedFields)
}

func TestIssueEditPersistsAtHistoryCap(t *testing.T) {
	ctx := context.Background()
	s, _, repo := repoWithMRs(t)
	issue, err := s.Issues().Create(ctx, newIssue(t, repo, "before"))
	require.NoError(t, err)
	events := make([]domain.IssueEvent, kvstore.MaxIssueEvents)
	for j := range events {
		events[j] = domain.IssueEvent{Kind: domain.IssueEventClosed, CreatedAt: created}
	}
	require.NoError(t, s.Issues().Update(ctx, issue, events...))
	issue, err = s.Issues().Get(ctx, repo, issue.Number)
	require.NoError(t, err)
	issue.Title, err = domain.ParseIssueTitle("after cap")
	require.NoError(t, err)
	issue.UpdatedAt = created.Add(time.Hour)
	require.NoError(t, s.Issues().Update(ctx, issue, domain.IssueEvent{Kind: domain.IssueEventEdited, ChangedFields: []string{"title"}, CreatedAt: issue.UpdatedAt}))
	got, err := s.Issues().Get(ctx, repo, issue.Number)
	require.NoError(t, err)
	assert.Equal(t, "after cap", got.Title.String())
	assert.Equal(t, issue.UpdatedAt, got.UpdatedAt)
	history, err := s.Issues().ListEvents(ctx, repo, issue.Number)
	require.NoError(t, err)
	require.Len(t, history, kvstore.MaxIssueEvents)
	assert.Equal(t, domain.IssueEventClosed, history[len(history)-1].Kind)
}
