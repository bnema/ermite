// SPDX-License-Identifier: AGPL-3.0-or-later
package usecase_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func textPtr(s string) *string { return &s }

func TestIssueEdit(t *testing.T) {
	for _, state := range []domain.IssueState{domain.IssueStateOpen, domain.IssueStateClosed, domain.IssueStateResolved, "pending"} {
		t.Run(string(state), func(t *testing.T) {
			u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)

			title, _ := domain.ParseIssueTitle("old")
			old := domain.Issue{Repo: u.repo.Name, Number: 1, State: state, Title: title, UpdatedAt: now.Add(-time.Hour)}
			u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(old, nil)
			if state != "pending" {
				u.store.EXPECT().Update(mock.Anything, mock.MatchedBy(func(i domain.Issue) bool { return i.Title.String() == "new" && i.UpdatedAt.Equal(now) }), mock.MatchedBy(func(events []domain.IssueEvent) bool {
					return len(events) == 1 && events[0].Kind == domain.IssueEventEdited && len(events[0].ChangedFields) == 1 && events[0].ChangedFields[0] == "title"
				})).Return(nil)
				updated := old
				updated.Title, _ = domain.ParseIssueTitle("new")
				updated.UpdatedAt = now
				u.bus.EXPECT().Publish(mock.Anything, domain.IssueEdited{Issue: updated, ChangedFields: []string{"title"}, At: now, Visibility: u.repo.Visibility, CreatedAt: u.repo.CreatedAt}).Return().Once()
			}
			got, err := u.issues.Edit(context.Background(), domain.Root(), u.repo.Name, 1, ports.EditIssueParams{Title: textPtr("new")})
			if state == "pending" {
				require.ErrorIs(t, err, domain.ErrPreconditionFailed)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "new", got.Title.String())
			}
		})
	}
	t.Run("no-op", func(t *testing.T) {
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		title, _ := domain.ParseIssueTitle("old")
		old := domain.Issue{State: domain.IssueStateOpen, Title: title, UpdatedAt: now.Add(-time.Hour)}
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(old, nil)
		got, err := u.issues.Edit(context.Background(), domain.Root(), u.repo.Name, 1, ports.EditIssueParams{Title: textPtr("old")})
		require.NoError(t, err)
		assert.Equal(t, old, got)
		u.bus.AssertNotCalled(t, "Publish", mock.Anything, mock.Anything)
	})
	for _, p := range []ports.EditIssueParams{{}, {Title: textPtr("")}, {Title: textPtr(strings.Repeat("x", 121))}, {Body: textPtr(strings.Repeat("x", 10001))}} {
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		_, err := u.issues.Edit(context.Background(), domain.Root(), u.repo.Name, 1, p)
		require.Error(t, err)
		assert.True(t, domain.IsInvalid(err))
	}
	t.Run("authorization", func(t *testing.T) {
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		_, err := u.issues.Edit(context.Background(), issuePrincipal(domain.ScopeRepoRead), u.repo.Name, 1, ports.EditIssueParams{Title: textPtr("new")})
		require.ErrorIs(t, err, domain.ErrForbidden)
	})
}

func TestMergeRequestEdit(t *testing.T) {
	for _, state := range []domain.MRState{domain.MRStateOpen, domain.MRStateClosed, domain.MRStateMerged} {
		t.Run(string(state), func(t *testing.T) {
			meta, store, bus, clock, links := mocks.NewRepoMetaStore(t), mocks.NewMergeRequestStore(t), mocks.NewEventBus(t), mocks.NewClock(t), mocks.NewIssueLinkTrigger(t)
			repo := domain.Repo{Name: name(t, "proj"), Visibility: domain.VisibilityPublic, Settings: domain.DefaultRepoSettings(), CreatedAt: now.Add(-24 * time.Hour)}
			meta.EXPECT().Get(mock.Anything, repo.Name).Return(repo, nil).Maybe()
			expectRepoMutation(t, meta, repo, mock.Anything).Maybe()
			clock.EXPECT().Now().Return(now).Maybe()
			m := usecase.NewMergeRequests(usecase.MergeRequestsConfig{Meta: meta, Store: store, Bus: bus, Clock: clock, Links: links})
			title, _ := domain.ParseMRTitle("old")
			old := domain.MergeRequest{Repo: repo.Name, Number: 1, State: state, Title: title, UpdatedAt: now.Add(-time.Hour)}
			store.EXPECT().Get(mock.Anything, repo.Name, int64(1)).Return(old, nil).Maybe()
			if state != domain.MRStateMerged {
				got, err := m.Edit(context.Background(), domain.Root(), repo.Name, 1, ports.EditMergeRequestParams{Title: textPtr("old")})
				require.NoError(t, err)
				assert.Equal(t, old, got)
				bus.AssertNotCalled(t, "Publish", mock.Anything, mock.Anything)
				store.EXPECT().Update(mock.Anything, mock.MatchedBy(func(m domain.MergeRequest) bool {
					return m.Description.String() == "Fixes #2" && m.Title == title && m.UpdatedAt.Equal(now)
				})).Return(nil)
				updated := old
				updated.Description, _ = domain.ParsePlainText("Fixes #2")
				updated.UpdatedAt = now
				bus.EXPECT().Publish(mock.Anything, domain.MergeRequestEdited{MergeRequest: updated, ChangedFields: []string{"description"}, At: now, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt}).Return().Once()
				links.EXPECT().OnMergeRequestChanged(mock.Anything, domain.Root(), repo, int64(1)).Return()
			}
			_, err := m.Edit(context.Background(), domain.Root(), repo.Name, 1, ports.EditMergeRequestParams{Description: textPtr("Fixes #2")})
			if state == domain.MRStateMerged {
				require.ErrorIs(t, err, domain.ErrPreconditionFailed)
				assert.Contains(t, err.Error(), "merged merge requests cannot be edited")
			} else {
				require.NoError(t, err)
			}
			for _, params := range []ports.EditMergeRequestParams{{}, {Title: textPtr("")}, {Description: textPtr(strings.Repeat("x", 8001))}} {
				_, err := m.Edit(context.Background(), domain.Root(), repo.Name, 1, params)
				require.Error(t, err)
				assert.True(t, domain.IsInvalid(err))
			}
		})
	}
}
