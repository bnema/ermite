// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
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

func linkedRig(t *testing.T) (*mrRig, *usecase.IssueLinks, *usecase.Issues) {
	t.Helper()
	r := newMRRig(t)
	r.seed()
	_, err := r.store.Mutate(r.ctx, r.repo, func(repo *domain.Repo) (bool, error) { repo.Settings.Issues = domain.IssuesOwner; return true, nil })
	require.NoError(t, err)
	issueStore := &linkIssueStore{IssueStore: r.store.Issues(), issues: make(map[int64]domain.Issue)}
	links := usecase.NewIssueLinks(usecase.IssueLinksConfig{Meta: r.store, Issues: issueStore, MRs: r.store.MergeRequests(), Links: r.store.IssueLinks(), Git: r.git, Bus: r.bus, Clock: fixedClock{}})
	r.mrs = usecase.NewMergeRequests(usecase.MergeRequestsConfig{Meta: r.store, Store: r.store.MergeRequests(), Repos: r.git, Git: r.git, Bus: r.bus, Clock: fixedClock{}, Maint: r.sched, Links: links})
	issues := usecase.NewIssues(usecase.IssuesConfig{Meta: r.store, Store: issueStore, MergeRequests: r.store.MergeRequests(), Links: r.store.IssueLinks(), Bus: r.bus, Clock: fixedClock{}})
	return r, links, issues
}

func TestIssueLinksTextsPushAndReferences(t *testing.T) {
	r, links, issues := linkedRig(t)
	issue, err := issues.Create(r.ctx, root, r.repo, ports.CreateIssueParams{Title: "bug"})
	require.NoError(t, err)
	r.feature("feat")
	mr, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "mention", Source: "feat", Description: fmt.Sprintf("see #%d #999", issue.Number)})
	require.NoError(t, err)
	got, err := issues.Get(r.ctx, root, r.repo, issue.Number)
	require.NoError(t, err)
	require.Len(t, got.References, 1)
	assert.Equal(t, mr.Title, got.References[0].Title)
	other, err := issues.Create(r.ctx, root, r.repo, ports.CreateIssueParams{Title: "other"})
	require.NoError(t, err)
	note, err := r.mrs.AddComment(r.ctx, root, r.repo, mr.Number, ports.AddCommentParams{Text: fmt.Sprintf("Fixes #%d; see #%d #999", other.Number, mr.Number)})
	require.NoError(t, err)
	got, err = issues.Get(r.ctx, root, r.repo, other.Number)
	require.NoError(t, err)
	require.Len(t, got.References, 1)
	assert.False(t, got.References[0].Closes)
	indexed, err := r.store.IssueLinks().LinksOfMergeRequest(r.ctx, r.repo, mr.Number)
	require.NoError(t, err)
	require.Len(t, indexed, 2, "MR and missing numbers must not be stored")
	_, err = r.mrs.EditComment(r.ctx, root, r.repo, mr.Number, note.ID, "no mention")
	require.NoError(t, err)
	got, err = issues.Get(r.ctx, root, r.repo, other.Number)
	require.NoError(t, err)
	assert.Empty(t, got.References)
	head := r.commit(author, "push.txt", "push", fmt.Sprintf("Fixes #%d", other.Number))
	r.push("feat")
	links.OnBranchesMoved(r.ctx, domain.Principal{}, r.repo, []domain.RefUpdate{{Ref: "refs/heads/feat", New: head.String()}})
	got, err = issues.Get(r.ctx, root, r.repo, other.Number)
	require.NoError(t, err)
	require.Len(t, got.References, 1)
	assert.True(t, got.References[0].Closes)
	stored, err := r.store.IssueLinks().LinksOfMergeRequest(r.ctx, r.repo, mr.Number)
	require.NoError(t, err)
	require.Len(t, stored, 2)
	_, err = r.mrs.Close(r.ctx, root, r.repo, mr.Number)
	require.NoError(t, err)
	got, err = issues.Get(r.ctx, root, r.repo, other.Number)
	require.NoError(t, err)
	assert.True(t, got.Issue.IsOpen())
	assert.Equal(t, domain.MRStateClosed, got.References[0].State)
}

func TestIssueLinksMergeClosing(t *testing.T) {
	for _, mode := range []string{"open", "off", "closed"} {
		t.Run(mode, func(t *testing.T) {
			r, _, issues := linkedRig(t)
			issue, err := issues.Create(r.ctx, root, r.repo, ports.CreateIssueParams{Title: "bug"})
			require.NoError(t, err)
			if mode == "closed" {
				issue, err = issues.Close(r.ctx, root, r.repo, issue.Number)
				require.NoError(t, err)
			}
			r.feature("feat")
			head := r.commit(author, "fix.txt", "fix", fmt.Sprintf("Fixes #%d", issue.Number))
			r.push("feat")
			mr := r.open("feat")
			if mode == "off" {
				_, err = r.store.Mutate(r.ctx, r.repo, func(repo *domain.Repo) (bool, error) { repo.Settings.Issues = domain.IssuesOff; return true, nil })
				require.NoError(t, err)
			}
			issue, err = r.store.Issues().Get(r.ctx, r.repo, issue.Number)
			require.NoError(t, err)
			r.drain()
			_, err = r.mrs.Merge(r.ctx, root, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: domain.MergeModeFastForward, DeleteSource: true})
			require.NoError(t, err)
			events := r.drain()
			detail, err := issues.Get(r.ctx, root, r.repo, issue.Number)
			if mode == "off" {
				stored, readErr := r.store.Issues().Get(r.ctx, r.repo, issue.Number)
				require.NoError(t, readErr)
				detail.Issue = stored
			} else {
				require.NoError(t, err)
			}
			got := detail.Issue
			refs, err := r.store.IssueLinks().LinksOfMergeRequest(r.ctx, r.repo, mr.Number)
			require.NoError(t, err)
			require.Len(t, refs, 1)
			assert.True(t, refs[0].Closes)
			closedEvents, referenced := 0, 0
			for _, ev := range events {
				switch ev.(type) {
				case domain.IssueResolved:
					closedEvents++
				case domain.IssueReferenced:
					referenced++
				}
			}
			assert.Positive(t, referenced)
			if mode == "open" {
				assert.Equal(t, mr.Number, got.ClosedByMergeRequest)
				assert.Equal(t, domain.IssueStateResolved, got.State)
				assert.Equal(t, 1, closedEvents)
				reopened, err := issues.Reopen(r.ctx, root, r.repo, issue.Number)
				require.NoError(t, err)
				assert.Zero(t, reopened.ClosedByMergeRequest)
			} else {
				assert.Zero(t, got.ClosedByMergeRequest)
				assert.Equal(t, 0, closedEvents)
				if mode == "closed" {
					assert.Equal(t, issue, got)
				} else {
					assert.True(t, got.IsOpen())
				}
			}
		})
	}
}

type gatedBranchGit struct {
	ports.MergeGit
	entered atomic.Int32
	started chan struct{}
	gate    chan struct{}
}

func (g *gatedBranchGit) BranchHead(ctx context.Context, name domain.RepoName, branch string) (domain.ObjectID, error) {
	if g.entered.Add(1) == 1 {
		close(g.started)
		<-g.gate
	}
	return g.MergeGit.BranchHead(ctx, name, branch)
}

func TestIssueLinksReindexSerializedPerMergeRequest(t *testing.T) {
	r, _, issues := linkedRig(t)
	issue, err := issues.Create(r.ctx, root, r.repo, ports.CreateIssueParams{Title: "bug"})
	require.NoError(t, err)
	r.feature("feat")
	mr := r.open("feat")
	g := &gatedBranchGit{MergeGit: r.git, started: make(chan struct{}), gate: make(chan struct{})}
	links := usecase.NewIssueLinks(usecase.IssueLinksConfig{Meta: r.store, Issues: r.store.Issues(), MRs: r.store.MergeRequests(), Links: r.store.IssueLinks(), Git: g, Bus: r.bus, Clock: fixedClock{}})
	repo, err := r.store.Get(r.ctx, r.repo)
	require.NoError(t, err)
	done := make(chan error, 2)
	go func() { done <- links.Reindex(r.ctx, domain.Principal{}, repo, mr.Number) }()
	<-g.started
	// The first reindex read the old texts; a note is added afterwards.
	_, err = r.mrs.AddComment(r.ctx, root, r.repo, mr.Number, ports.AddCommentParams{Text: fmt.Sprintf("see #%d", issue.Number)})
	require.NoError(t, err)
	go func() { done <- links.Reindex(r.ctx, domain.Principal{}, repo, mr.Number) }()
	time.Sleep(100 * time.Millisecond)
	assert.EqualValues(t, 1, g.entered.Load(), "the second reindex waits for the first")
	close(g.gate)
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	got, err := r.store.IssueLinks().LinksOfMergeRequest(r.ctx, r.repo, mr.Number)
	require.NoError(t, err)
	require.Len(t, got, 1, "the final index reflects the latest content")
}

func TestIssueLinksNotifiesRemovalsAndClosingFlips(t *testing.T) {
	r, links, issues := linkedRig(t)
	issue, err := issues.Create(r.ctx, root, r.repo, ports.CreateIssueParams{Title: "bug"})
	require.NoError(t, err)
	r.feature("feat")
	mr := r.open("feat")
	referenced := func() (n int) {
		for _, ev := range r.drain() {
			if e, ok := ev.(domain.IssueReferenced); ok && e.Issue == issue.Number {
				n++
			}
		}
		return n
	}
	note, err := r.mrs.AddComment(r.ctx, root, r.repo, mr.Number, ports.AddCommentParams{Text: fmt.Sprintf("see #%d", issue.Number)})
	require.NoError(t, err)
	assert.Equal(t, 1, referenced())
	_, err = r.mrs.EditComment(r.ctx, root, r.repo, mr.Number, note.ID, "nothing")
	require.NoError(t, err)
	assert.Equal(t, 1, referenced(), "removing the last mention notifies")

	// closes true -> false emits one event: a commit message fixes the issue,
	// then is rewritten to a plain mention.
	head := r.commit(author, "fix.txt", "fix", fmt.Sprintf("Fixes #%d", issue.Number))
	r.push("feat")
	links.OnBranchesMoved(r.ctx, domain.Principal{}, r.repo, []domain.RefUpdate{{Ref: "refs/heads/feat", New: head.String()}})
	referenced()
	r.sh("commit", "-q", "--amend", "-m", fmt.Sprintf("See #%d", issue.Number))
	r.push("feat")
	links.OnBranchesMoved(r.ctx, domain.Principal{}, r.repo, []domain.RefUpdate{{Ref: "refs/heads/feat", New: r.branchHead("feat").String()}})
	assert.Equal(t, 1, referenced(), "a closing flip notifies once")
}

func TestIssueLinksMergeReindexesRequestsWhoseSourceMoved(t *testing.T) {
	r, _, issues := linkedRig(t)
	issue, err := issues.Create(r.ctx, root, r.repo, ports.CreateIssueParams{Title: "bug"})
	require.NoError(t, err)
	r.sh("checkout", "-q", "-B", "x", "main")
	r.commit(author, "x.txt", "x", "work on x")
	r.push("x")
	r.sh("checkout", "-q", "-B", "feat", "x")
	head := r.commit(author, "fix.txt", "fix", fmt.Sprintf("Fixes #%d", issue.Number))
	r.push("feat")
	b, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "B", Source: "x"})
	require.NoError(t, err)
	a, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "A", Source: "feat", Target: "x"})
	require.NoError(t, err)
	before, err := r.store.IssueLinks().LinksOfMergeRequest(r.ctx, r.repo, b.Number)
	require.NoError(t, err)
	require.Empty(t, before)
	_, err = r.mrs.Merge(r.ctx, root, r.repo, a.Number, ports.MergeParams{HeadID: head, Mode: domain.MergeModeFastForward})
	require.NoError(t, err)
	r.mrs.Wait(r.ctx)
	after, err := r.store.IssueLinks().LinksOfMergeRequest(r.ctx, r.repo, b.Number)
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, issue.Number, after[0].Issue)
}

func TestAutoMergeClosesLinkedIssue(t *testing.T) {
	r, _, issues := linkedRig(t)
	issue, err := issues.Create(r.ctx, root, r.repo, ports.CreateIssueParams{Title: "bug"})
	require.NoError(t, err)
	r.feature("feat")
	mr, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "fix", Source: "feat", Description: fmt.Sprintf("closes #%d", issue.Number)})
	require.NoError(t, err)
	_, err = r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{AllowAutoMerge: ptr(true)})
	require.NoError(t, err)
	checks := mocks.NewCIChecksReader(t)
	r.mrs.SetChecks(checks)
	enableAuto(t, r, mr)
	checks.EXPECT().CommitChecks(mock.Anything, r.repo, r.head().String()).Return([]domain.CICheck{{Status: domain.CICompleted, Conclusion: domain.CISuccess}}, nil).Twice()
	a := autoEvaluator(r, checks, nil, nil)
	defer a.Close(r.ctx)
	require.True(t, a.Evaluate(r.repo))
	r.mrs.Wait(r.ctx)
	merged := storedAuto(t, r, mr)
	require.Equal(t, domain.MRStateMerged, merged.State)
	require.True(t, merged.AutoMerged)
	got, err := issues.Get(r.ctx, root, r.repo, issue.Number)
	require.NoError(t, err)
	assert.False(t, got.Issue.IsOpen())
}

// linkIssueStore isolates merge state writes from the independently developed
// kvstore resolved-state encoder. All other operations use the existing rig.
type linkIssueStore struct {
	ports.IssueStore
	mu     sync.Mutex
	issues map[int64]domain.Issue
}

func (s *linkIssueStore) Get(ctx context.Context, repo domain.RepoName, n int64) (domain.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue, ok := s.issues[n]; ok {
		return issue, nil
	}
	return s.IssueStore.Get(ctx, repo, n)
}
func (s *linkIssueStore) Update(ctx context.Context, issue domain.Issue, events ...domain.IssueEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue.State == domain.IssueStateResolved {
		s.issues[issue.Number] = issue
		return nil
	}
	if _, ok := s.issues[issue.Number]; ok {
		s.issues[issue.Number] = issue
		return nil
	}
	return s.IssueStore.Update(ctx, issue, events...)
}

func TestMergeResolutionActorAndReferenceEvent(t *testing.T) {
	for _, state := range []domain.IssueState{domain.IssueStateOpen, domain.IssueStateClosed, domain.IssueStateResolved} {
		t.Run(string(state), func(t *testing.T) {
			u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
			links := mocks.NewIssueLinkStore(t)
			clock := mocks.NewClock(t)
			clock.EXPECT().Now().Return(now).Maybe()
			p := issuePrincipal(domain.ScopeIssueWrite)
			title, err := domain.ParseMRTitle("Fixes #3")
			require.NoError(t, err)
			mr := domain.MergeRequest{Repo: u.repo.Name, Number: 4, Title: title, State: domain.MRStateMerged}
			u.mrs.EXPECT().Get(mock.Anything, u.repo.Name, int64(4)).Return(mr, nil)
			u.mrs.EXPECT().ListComments(mock.Anything, u.repo.Name, int64(4)).Return(nil, nil)
			u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(domain.Issue{Repo: u.repo.Name, Number: 3, State: state}, nil).Twice()
			links.EXPECT().ReplaceLinks(mock.Anything, u.repo.Name, int64(4), []int64{3}, []int64{3}, ports.ReferenceEvent{Actor: "agent-1", At: now}).Return(nil, nil, nil)
			links.EXPECT().LinksOfMergeRequest(mock.Anything, u.repo.Name, int64(4)).Return(nil, nil)
			if state == domain.IssueStateOpen {
				u.store.EXPECT().Update(mock.Anything, mock.MatchedBy(func(i domain.Issue) bool { return i.State == domain.IssueStateResolved && i.ClosedByMergeRequest == 4 }), mock.MatchedBy(func(events []domain.IssueEvent) bool {
					return len(events) == 1 && events[0].Kind == domain.IssueEventResolved && events[0].MergeRequest == 4 && events[0].Actor == "agent-1" && events[0].CreatedAt.Equal(now)
				})).Return(nil).Once()
				u.bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(e domain.Event) bool { return e.EventKind() == "issue.resolved" })).Return().Once()
			}
			svc := usecase.NewIssueLinks(usecase.IssueLinksConfig{Meta: u.meta, Issues: u.store, MRs: u.mrs, Links: links, Bus: u.bus, Clock: clock})
			svc.OnMerged(context.Background(), p, u.repo, mr)
		})
	}
}
