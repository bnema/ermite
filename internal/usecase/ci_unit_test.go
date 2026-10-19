// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

// mergeUnit is a merge request that is ready to merge, with a checks reader.
type mergeUnit struct {
	*mrUnit
	checks *mocks.CIChecksReader
	trig   *mocks.WorkflowTrigger
	head   domain.ObjectID
}

func newMergeUnit(t *testing.T, block bool) *mergeUnit {
	t.Helper()
	u := &mrUnit{
		meta:  mocks.NewRepoMetaStore(t),
		store: mocks.NewMergeRequestStore(t),
		repos: mocks.NewGitRepos(t),
		git:   mocks.NewMergeGit(t),
		repo:  domain.Repo{Name: name(t, "proj"), Visibility: domain.VisibilityPrivate},
	}
	u.repo.Settings = domain.DefaultRepoSettings()
	u.repo.Settings.FailingChecksBlock = block
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	sched := mocks.NewMaintenanceScheduler(t)
	sched.EXPECT().Schedule(mock.Anything, mock.Anything).Return().Maybe()
	m := &mergeUnit{mrUnit: u, checks: mocks.NewCIChecksReader(t), trig: mocks.NewWorkflowTrigger(t), head: someID(t)}
	u.mrs = usecase.NewMergeRequests(usecase.MergeRequestsConfig{
		Meta: u.meta, Store: u.store, Repos: u.repos, Git: u.git,
		Signer: mocks.NewCommitSigner(t), Bus: mocks.NewEventBus(t), Clock: clock, Maint: sched,
		CI: m.trig, Checks: m.checks,
	})
	u.meta.EXPECT().Get(mock.Anything, u.repo.Name).Return(u.repo, nil)
	u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(domain.MergeRequest{
		Repo: u.repo.Name, Number: 1, State: domain.MRStateOpen, Source: "feat", Target: "main",
	}, nil)
	tip, err := domain.ParseObjectID("1111111111111111111111111111111111111111")
	require.NoError(t, err)
	u.git.EXPECT().BranchHead(mock.Anything, u.repo.Name, "feat").Return(m.head, nil)
	u.git.EXPECT().BranchHead(mock.Anything, u.repo.Name, "main").Return(tip, nil)
	u.git.EXPECT().IsAncestor(mock.Anything, u.repo.Name, tip, m.head).Return(true, nil)
	u.git.EXPECT().IsAncestor(mock.Anything, u.repo.Name, m.head, tip).Return(false, nil)
	return m
}

func (m *mergeUnit) merge() error {
	_, err := m.mrs.Merge(context.Background(), domain.Root(), m.repo.Name, 1, ports.MergeParams{HeadID: m.head})
	return err
}

// AC5: the merge rule under FailingChecksBlock.
func TestMergeRuleOfFailingChecksBlock(t *testing.T) {
	t.Parallel()
	check := func(status domain.CIStatus, c domain.CIConclusion, name string) domain.CICheck {
		return domain.CICheck{Name: name, Status: status, Conclusion: c}
	}
	tests := []struct {
		name    string
		checks  []domain.CICheck
		blocked string // part of the message
	}{
		{"a failure blocks", []domain.CICheck{check(domain.CICompleted, domain.CISuccess, "a"), check(domain.CICompleted, domain.CIFailure, "build")}, "failing checks block the merge: build"},
		{"a timeout blocks", []domain.CICheck{check(domain.CICompleted, domain.CITimedOut, "slow")}, "slow"},
		{"a cancelled check blocks", []domain.CICheck{check(domain.CICompleted, domain.CICancelled, "c")}, "failing checks"},
		{"action required blocks", []domain.CICheck{check(domain.CICompleted, domain.CIActionRequired, "env")}, "failing checks"},
		{"a queued check blocks", []domain.CICheck{check(domain.CIQueued, "", "q")}, "have not finished: q"},
		{"a running check blocks", []domain.CICheck{check(domain.CIInProgress, "", "r")}, "have not finished: r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := newMergeUnit(t, true)
			m.checks.EXPECT().CommitChecks(mock.Anything, m.repo.Name, m.head.String()).Return(tt.checks, nil)
			// The merge stops at the rule: the branch mock expects no move.
			err := m.merge()
			require.ErrorIs(t, err, domain.ErrPreconditionFailed)
			assert.Contains(t, err.Error(), tt.blocked)
		})
	}
	t.Run("too many checks to tell", func(t *testing.T) {
		t.Parallel()
		m := newMergeUnit(t, true)
		m.checks.EXPECT().CommitChecks(mock.Anything, m.repo.Name, m.head.String()).Return(nil, domain.ErrResourceExhausted)
		err := m.merge()
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "too many checks")
	})
	// The cases where the merge goes on (no check, all passing, rule off) need
	// the whole merge: see TestCIMergeIsBlockedByChecks, which merges for real.
}

func TestCombineChecks(t *testing.T) {
	t.Parallel()
	ck := func(s domain.CIStatus, c domain.CIConclusion) domain.CICheck {
		return domain.CICheck{Status: s, Conclusion: c}
	}
	assert.Equal(t, domain.CheckStateNone, domain.CombineChecks(nil))
	assert.Equal(t, domain.CheckStateSuccess, domain.CombineChecks([]domain.CICheck{ck(domain.CICompleted, domain.CISuccess), ck(domain.CICompleted, domain.CISkipped)}))
	assert.Equal(t, domain.CheckStatePending, domain.CombineChecks([]domain.CICheck{ck(domain.CICompleted, domain.CISuccess), ck(domain.CIQueued, "")}))
	assert.Equal(t, domain.CheckStateFailure, domain.CombineChecks([]domain.CICheck{ck(domain.CIInProgress, ""), ck(domain.CICompleted, domain.CIFailure)}), "a failure wins over pending")
}

func TestLabelsMatchAndRunnerLabels(t *testing.T) {
	t.Parallel()
	have := domain.RunnerLabels([]string{"ubuntu-latest", "gpu"}, "linux", "x64")
	assert.Equal(t, []string{"gpu", "linux", "self-hosted", "ubuntu-latest", "x64"}, have)
	assert.True(t, domain.LabelsMatch([]string{"Ubuntu-Latest"}, have), "case-insensitive")
	assert.True(t, domain.LabelsMatch([]string{"self-hosted", "linux", "x64"}, have))
	assert.False(t, domain.LabelsMatch([]string{"self-hosted", "arm64"}, have))
	assert.False(t, domain.LabelsMatch([]string{"windows-latest"}, have))
	assert.True(t, domain.LabelsMatch(nil, have), "no requirement matches")
	assert.False(t, domain.LabelsMatch([]string{"runner-name"}, have), "the runner name is not a label")
}

func TestStepRanges(t *testing.T) {
	t.Parallel()
	steps := []domain.CIStep{
		{Number: 1, HasLines: true, LogStart: 0},
		{Number: 2},
		{Number: 3, HasLines: true, LogStart: 4},
		{Number: 4, HasLines: true, LogStart: 9},
		{Number: 5},
	}
	got := domain.StepRanges(steps, 12)
	assert.Equal(t, []domain.StepRange{{Start: 0, End: 4}, {Start: 4, End: 4}, {Start: 4, End: 9}, {Start: 9, End: 12}, {Start: 12, End: 12}}, got)
	assert.EqualValues(t, 3, domain.StepOfLine(steps, 5))
	assert.EqualValues(t, 1, domain.StepOfLine(steps, 0))
	assert.EqualValues(t, 4, domain.StepOfLine(steps, 11))
}

func TestJobIDsRoundTripAndRejectOthers(t *testing.T) {
	t.Parallel()
	id := domain.NewJobID(42, 3, 1)
	run, index, combo, ok := domain.ParseJobID(id)
	require.True(t, ok)
	assert.Equal(t, []int{42, 3, 1}, []int{int(run), index, combo})
	for _, bad := range []string{"", "1-2", "1-2-3-4", "a-b-c", "0-1-1", "1--2", "01-2-3", "../../x", "1-2-3/4"} {
		_, _, _, ok := domain.ParseJobID(bad)
		assert.False(t, ok, bad)
	}
}

func TestOutputLimitsAndNames(t *testing.T) {
	t.Parallel()
	assert.NoError(t, domain.CheckOutputs(map[string]string{"a": "1", "_b-c": "2"}))
	assert.Error(t, domain.CheckOutputs(map[string]string{"1a": "x"}), "starts with a digit")
	assert.Error(t, domain.CheckOutputs(map[string]string{"": "x"}))
	assert.Error(t, domain.CheckOutputs(map[string]string{"A": "x", "a": "y"}), "names differ only by case")
	assert.Error(t, domain.CheckOutputs(map[string]string{"big": string(make([]byte, domain.CIMaxOutputBytes+1))}))
	assert.Error(t, domain.CheckOutputs(map[string]string{"a": string(make([]byte, domain.CIMaxOutputBytes/2+1)), "b": string(make([]byte, domain.CIMaxOutputBytes/2+1))}), "1 MiB together")
}

func TestTokenShapes(t *testing.T) {
	t.Parallel()
	good := "ermitejob_" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN-_0"
	assert.True(t, domain.IsJobToken(good))
	assert.False(t, domain.IsRunnerToken(good))
	assert.False(t, domain.IsJobToken(good[:len(good)-1]))
	assert.False(t, domain.IsJobToken("ermite_"+good[10:]))
	assert.False(t, domain.IsJobToken(good+"="))
}
