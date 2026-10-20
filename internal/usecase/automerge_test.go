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
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

func autoRig(t *testing.T) (*mrRig, domain.MergeRequest, *mocks.CIChecksReader) {
	t.Helper()
	r := newMRRig(t)
	r.seed()
	r.feature("feat")
	mr := r.open("feat")
	checks := mocks.NewCIChecksReader(t)
	r.mrs.SetChecks(checks)
	_, err := r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{AllowAutoMerge: ptr(true)})
	require.NoError(t, err)
	return r, mr, checks
}

func autoEvaluator(r *mrRig, checks ports.CIChecksReader, activity ports.CIActivity, tokens ports.TokenStore) *usecase.AutoMerge {
	return usecase.NewAutoMerge(r.ctx, usecase.AutoMergeConfig{MRs: r.mrs, Store: r.store.MergeRequests(), Meta: r.store, Git: r.git, Checks: checks, Activity: activity, Tokens: tokens, Bus: r.bus, Clock: fixedClock{}})
}

func enableAuto(t *testing.T, r *mrRig, mr domain.MergeRequest) domain.MergeRequest {
	t.Helper()
	enabled, err := r.mrs.EnableAutoMerge(r.ctx, root, r.repo, mr.Number, ports.EnableAutoMergeParams{HeadID: r.head()})
	require.NoError(t, err)
	require.NotNil(t, enabled.AutoMerge)
	return enabled
}

func storedAuto(t *testing.T, r *mrRig, mr domain.MergeRequest) domain.MergeRequest {
	t.Helper()
	got, err := r.store.MergeRequests().Get(r.ctx, r.repo, mr.Number)
	require.NoError(t, err)
	return got
}

func TestEnableAutoMergeNeedsSettingScopeAndRef(t *testing.T) {
	for _, tc := range []string{"setting", "scope", "ref", "draft", "no CI", "closed", "mode"} {
		t.Run(tc, func(t *testing.T) {
			r, mr, _ := autoRig(t)
			p := root
			want := domain.ErrPreconditionFailed
			switch tc {
			case "setting":
				_, err := r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{AllowAutoMerge: ptr(false)})
				require.NoError(t, err)
			case "scope":
				p = tokenPrincipal(domain.ScopeMRWrite, r.repo.String())
				want = domain.ErrNotFound
			case "ref":
				p = domain.TokenPrincipal(domain.Token{ID: "limited", Scopes: []domain.Scope{domain.ScopeMRMerge}, Repos: []domain.RepoName{r.repo}, BranchPrefixes: []string{"feat"}})
				want = domain.ErrForbidden
			case "draft":
				mr.Title, _ = domain.ParseMRTitle("WIP: work")
				require.NoError(t, r.store.MergeRequests().Update(r.ctx, mr))
			case "no CI":
				r.mrs.SetChecks(nil)
			case "closed":
				_, err := r.mrs.Close(r.ctx, root, r.repo, mr.Number)
				require.NoError(t, err)
			case "mode":
				_, err := r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{AllowedMergeModes: ptr([]domain.MergeMode{domain.MergeModeSquash})})
				require.NoError(t, err)
			}
			_, err := r.mrs.EnableAutoMerge(r.ctx, p, r.repo, mr.Number, ports.EnableAutoMergeParams{HeadID: r.head()})
			require.ErrorIs(t, err, want)
		})
	}
}

func TestEnableAutoMergeRecordsEnablerModeHeadAndPublishes(t *testing.T) {
	r, mr, _ := autoRig(t)
	trigger := mocks.NewAutoMergeTrigger(t)
	trigger.EXPECT().OnRepoChanged(mock.Anything, r.repo).Return().Twice()
	r.mrs.WithAutoMerge(trigger)
	r.drain()
	got, err := r.mrs.EnableAutoMerge(r.ctx, domain.Owner(), r.repo, mr.Number, ports.EnableAutoMergeParams{Mode: domain.MergeModeSquash, HeadID: r.head()})
	require.NoError(t, err)
	assert.Equal(t, &domain.AutoMerge{Mode: domain.MergeModeSquash, HeadID: r.head(), EnabledBy: domain.PusherOf(domain.Owner()), EnablerLabel: domain.AuthorName(domain.Owner()), EnabledAt: now}, got.AutoMerge)
	events := r.drain()
	require.Len(t, events, 1)
	assert.IsType(t, domain.MergeRequestAutoMergeEnabled{}, events[0])
	got = enableAuto(t, r, mr)
	assert.Equal(t, domain.PusherOf(root), got.AutoMerge.EnabledBy)
	assert.Equal(t, domain.MergeModeFastForward, got.AutoMerge.Mode)
}

func TestCancelAutoMergePublishesReason(t *testing.T) {
	r, mr, _ := autoRig(t)
	enableAuto(t, r, mr)
	r.drain()
	got, err := r.mrs.CancelAutoMerge(r.ctx, root, r.repo, mr.Number)
	require.NoError(t, err)
	assert.Nil(t, got.AutoMerge)
	events := r.drain()
	require.Len(t, events, 1)
	ev, ok := events[0].(domain.MergeRequestAutoMergeCancelled)
	require.True(t, ok)
	assert.NotEmpty(t, ev.Reason)
	_, err = r.mrs.CancelAutoMerge(r.ctx, root, r.repo, mr.Number)
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
}

func TestAutoMergeWaitsWhileCINotSettledOrChecksPending(t *testing.T) {
	r, mr, checks := autoRig(t)
	enableAuto(t, r, mr)
	activity := mocks.NewCIActivity(t)
	activity.EXPECT().Settled(mock.Anything, r.repo, mr.Number, mr.Source, r.head()).Return(false).Once()
	activity.EXPECT().Settled(mock.Anything, r.repo, mr.Number, mr.Source, r.head()).Return(true).Once()
	checks.EXPECT().CommitChecks(mock.Anything, r.repo, r.head().String()).Return([]domain.CICheck{{Status: domain.CICompleted, Conclusion: domain.CISuccess}, {Status: domain.CIQueued, Name: "pending"}}, nil)
	a := autoEvaluator(r, checks, activity, nil)
	defer a.Close(r.ctx)
	require.True(t, a.Evaluate(r.repo))
	require.True(t, a.Evaluate(r.repo))
	assert.True(t, storedAuto(t, r, mr).IsOpen())
	assert.NotNil(t, storedAuto(t, r, mr).AutoMerge)
}

func TestAutoMergeMergesAsEnablerWhenAChecksPassed(t *testing.T) {
	r, mr, checks := autoRig(t)
	trigger := mocks.NewWorkflowTrigger(t)
	trigger.EXPECT().OnMerge(mock.Anything, r.repo, mock.Anything, root).Return().Once()
	r.mrs.SetWorkflowTrigger(trigger)
	enableAuto(t, r, mr)
	checks.EXPECT().CommitChecks(mock.Anything, r.repo, r.head().String()).Return([]domain.CICheck{{Status: domain.CICompleted, Conclusion: domain.CISuccess}}, nil)
	a := autoEvaluator(r, checks, nil, nil)
	defer a.Close(r.ctx)
	r.drain()
	require.True(t, a.Evaluate(r.repo))
	got := storedAuto(t, r, mr)
	assert.Equal(t, domain.MRStateMerged, got.State)
	assert.True(t, got.AutoMerged)
	assert.Nil(t, got.AutoMerge)
	events := r.drain()
	found := false
	for _, ev := range events {
		if merged, ok := ev.(domain.MergeRequestMerged); ok {
			assert.True(t, merged.Auto)
			found = true
		}
	}
	require.True(t, found)
}

func TestAutoMergeIgnoresFailingChecksBlockOff(t *testing.T) {
	r, mr, checks := autoRig(t)
	enableAuto(t, r, mr)
	_, err := r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{FailingChecksBlock: ptr(false)})
	require.NoError(t, err)
	// No successful check: an unsuccessful head waits rather than merging.
	checks.EXPECT().CommitChecks(mock.Anything, r.repo, r.head().String()).Return([]domain.CICheck{{Status: domain.CICompleted, Conclusion: domain.CIFailure}}, nil)
	a := autoEvaluator(r, checks, nil, nil)
	defer a.Close(r.ctx)
	require.True(t, a.Evaluate(r.repo))
	assert.True(t, storedAuto(t, r, mr).IsOpen())
	assert.NotNil(t, storedAuto(t, r, mr).AutoMerge)
}

func TestAutoMergeCancelsOnNewHead(t *testing.T)               { testAutoCancellation(t, "head") }
func TestAutoMergeCancelsWhenModeNoLongerAllowed(t *testing.T) { testAutoCancellation(t, "mode") }
func TestAutoMergeCancelsWhenSettingTurnedOff(t *testing.T)    { testAutoCancellation(t, "setting") }
func testAutoCancellation(t *testing.T, change string) {
	r, mr, checks := autoRig(t)
	enableAuto(t, r, mr)
	switch change {
	case "head":
		r.commit(author, "new", "new", "new head")
		r.push("feat")
	case "mode":
		_, err := r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{AllowedMergeModes: ptr([]domain.MergeMode{domain.MergeModeSquash})})
		require.NoError(t, err)
	case "setting":
		_, err := r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{AllowAutoMerge: ptr(false)})
		require.NoError(t, err)
	}
	a := autoEvaluator(r, checks, nil, nil)
	defer a.Close(r.ctx)
	r.drain()
	require.True(t, a.Evaluate(r.repo))
	assert.Nil(t, storedAuto(t, r, mr).AutoMerge)
	events := r.drain()
	require.Len(t, events, 1)
	assert.IsType(t, domain.MergeRequestAutoMergeCancelled{}, events[0])
}

func TestAutoMergeCancelsWhenEnablerTokenRevokedOrExpired(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "revoked", true: "expired"}[expired], func(t *testing.T) {
			r, mr, checks := autoRig(t)
			enableAuto(t, r, mr)
			enabled := storedAuto(t, r, mr)
			enabled.AutoMerge.EnabledBy = domain.CIPusher{Kind: domain.CIPusherToken, TokenID: "gone"}
			require.NoError(t, r.store.MergeRequests().Update(r.ctx, enabled))
			tokens := mocks.NewTokenStore(t)
			if expired {
				tokens.EXPECT().Get(mock.Anything, "gone").Return(domain.Token{ExpiresAt: now.Add(-time.Hour)}, nil)
			} else {
				tokens.EXPECT().Get(mock.Anything, "gone").Return(domain.Token{}, domain.ErrNotFound)
			}
			a := autoEvaluator(r, checks, nil, tokens)
			defer a.Close(r.ctx)
			require.True(t, a.Evaluate(r.repo))
			assert.Nil(t, storedAuto(t, r, mr).AutoMerge)
			assert.True(t, storedAuto(t, r, mr).IsOpen())
		})
	}
}

func TestAutoMergeCancelsOnPermanentMergeFailure(t *testing.T) {
	for _, mode := range []domain.MergeMode{domain.MergeModeFastForward, domain.MergeModeSquash} {
		t.Run(mode.String(), func(t *testing.T) {
			r, mr, checks := autoRig(t)
			_, err := r.mrs.EnableAutoMerge(r.ctx, root, r.repo, mr.Number, ports.EnableAutoMergeParams{Mode: mode, HeadID: r.head()})
			require.NoError(t, err)
			head := r.head()
			r.sh("checkout", "main")
			r.commit(author, "a.txt", "conflicting target\n", "move target")
			r.push("main")
			checks.EXPECT().CommitChecks(mock.Anything, r.repo, head.String()).Return([]domain.CICheck{{Status: domain.CICompleted, Conclusion: domain.CISuccess}}, nil)
			a := autoEvaluator(r, checks, nil, nil)
			defer a.Close(r.ctx)
			require.True(t, a.Evaluate(r.repo))
			assert.Nil(t, storedAuto(t, r, mr).AutoMerge)
			assert.True(t, storedAuto(t, r, mr).IsOpen())
		})
	}
}

func TestManualMergeOrCloseClearsAutoMerge(t *testing.T) {
	for _, closeMR := range []bool{false, true} {
		t.Run(map[bool]string{false: "merge", true: "close"}[closeMR], func(t *testing.T) {
			r, mr, checks := autoRig(t)
			enabled := enableAuto(t, r, mr)
			if closeMR {
				_, err := r.mrs.Close(r.ctx, root, r.repo, mr.Number)
				require.NoError(t, err)
			} else {
				checks.EXPECT().CommitChecks(mock.Anything, r.repo, r.head().String()).Return(nil, nil)
				_, err := r.mrs.Merge(r.ctx, root, r.repo, mr.Number, ports.MergeParams{HeadID: enabled.AutoMerge.HeadID})
				require.NoError(t, err)
			}
			got := storedAuto(t, r, mr)
			assert.Nil(t, got.AutoMerge)
			assert.False(t, got.AutoMerged)
		})
	}
}

func TestAutoMergeStartupRescan(t *testing.T) {
	r, mr, checks := autoRig(t)
	enableAuto(t, r, mr)
	checks.EXPECT().CommitChecks(mock.Anything, r.repo, r.head().String()).Return([]domain.CICheck{{Status: domain.CICompleted, Conclusion: domain.CISuccess}}, nil)
	a := autoEvaluator(r, checks, nil, nil)
	a.Start()
	defer a.Close(r.ctx)
	require.Eventually(t, func() bool {
		got, err := r.store.MergeRequests().Get(r.ctx, r.repo, mr.Number)
		return err == nil && got.AutoMerged
	}, 5*time.Second, time.Millisecond)
	a.Close(r.ctx)
	r.mrs.Wait(r.ctx)
}

func TestAutoMergeRetriesTransientErrors(t *testing.T) {
	ctx := context.Background()
	store := mocks.NewMergeRequestStore(t)
	meta := mocks.NewRepoMetaStore(t)
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	repo := name(t, "proj")
	store.EXPECT().ListOpenAll(mock.Anything).Return([]domain.MergeRequest{{Repo: repo, AutoMerge: &domain.AutoMerge{}}}, nil)
	meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{}, errors.New("temporary read error")).Once()
	meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo}, nil).Once()
	done := make(chan struct{}, 1)
	store.EXPECT().ListOpen(mock.Anything, repo).Run(func(context.Context, domain.RepoName) { done <- struct{}{} }).Return(nil, nil).Once()
	timer := make(chan time.Time, 1)
	armed := make(chan time.Duration, 1)
	a := usecase.NewAutoMerge(ctx, usecase.AutoMergeConfig{Store: store, Meta: meta, Clock: clock, After: func(d time.Duration) <-chan time.Time { armed <- d; return timer }})
	a.Start()
	defer a.Close(ctx)
	select {
	case d := <-armed:
		assert.Equal(t, time.Second, d)
	case <-time.After(5 * time.Second):
		t.Fatal("no retry scheduled")
	}
	timer <- now.Add(time.Second)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not evaluate")
	}
}

func TestAutoMergeKickFromUnderRunLockDoesNotDeadlock(t *testing.T) {
	r, mr, checks := autoRig(t)
	enableAuto(t, r, mr)
	checks.EXPECT().CommitChecks(mock.Anything, r.repo, r.head().String()).Return([]domain.CICheck{{Status: domain.CICompleted, Conclusion: domain.CISuccess}}, nil)
	a := autoEvaluator(r, checks, nil, nil)
	ci := newCIUnit(t, nil).ci
	ci.SetAutoMergeTrigger(a)
	gate := make(chan struct{})
	entered := make(chan struct{})
	r.mrs.SetAfterMergeGate(func() { close(entered); <-gate })
	defer func() { close(gate); a.Close(r.ctx); r.mrs.Wait(r.ctx) }()
	require.NoError(t, r.mrs.UnderRepoLock(r.ctx, r.repo, func() {
		a.Start()
		done := make(chan struct{})
		go func() {
			ci.UnderRunLock(1, func() { a.OnRepoChanged(r.ctx, r.repo) })
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("kick blocked behind merge lock")
		}
	}))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("merge never reached post-merge gate")
	}
}

func TestAutoMergeRequiresSuccessfulCheck(t *testing.T) {
	for _, checksOnHead := range [][]domain.CICheck{nil, {{Status: domain.CICompleted, Conclusion: domain.CISkipped}}, {{Status: domain.CICompleted, Conclusion: domain.CINeutral}}} {
		r, mr, checks := autoRig(t)
		enableAuto(t, r, mr)
		checks.EXPECT().CommitChecks(mock.Anything, r.repo, r.head().String()).Return(checksOnHead, nil)
		a := autoEvaluator(r, checks, nil, nil)
		require.True(t, a.Evaluate(r.repo))
		a.Close(r.ctx)
		assert.True(t, storedAuto(t, r, mr).IsOpen())
		assert.NotNil(t, storedAuto(t, r, mr).AutoMerge)
	}
}

func TestEnableAutoMergeNeedsTheHeadTheCallerSaw(t *testing.T) {
	t.Run("stale head", func(t *testing.T) {
		r, mr, _ := autoRig(t)
		stale := r.head()
		r.commit(author, "later.txt", "later\n", "push after review")
		r.push("feat")
		r.drain()
		_, err := r.mrs.EnableAutoMerge(r.ctx, root, r.repo, mr.Number, ports.EnableAutoMergeParams{HeadID: stale})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "branch moved, read again")
		assert.Nil(t, storedAuto(t, r, mr).AutoMerge)
		assert.Empty(t, r.drain(), "nothing is published")
	})
	t.Run("missing head", func(t *testing.T) {
		r, mr, _ := autoRig(t)
		_, err := r.mrs.EnableAutoMerge(r.ctx, root, r.repo, mr.Number, ports.EnableAutoMergeParams{})
		require.ErrorIs(t, err, domain.ErrInvalid)
		assert.Nil(t, storedAuto(t, r, mr).AutoMerge)
	})
}
