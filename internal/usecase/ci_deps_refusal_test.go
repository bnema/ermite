// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// refusalRig is a dependency job about to be completed with a report that the
// observer refuses, over mocked stores.
type refusalRig struct {
	*ciUnit
	job    domain.CIJob
	params ports.CompleteJobParams
}

func newRefusalRig(t *testing.T) *refusalRig {
	t.Helper()
	u := newCIUnit(t, nil)
	j := ciJob(t, domain.CIPhaseRunning, jobSecret)
	j.Deps = &domain.CIDepsJob{GoVersion: "1.26.1"}
	run := domain.CIRun{Repo: j.Repo, ID: j.RunID, Attempt: 1, Event: domain.CIEventDependencies}
	u.expectToken(jobSecret, j)
	u.store.EXPECT().GetRun(mock.Anything, j.Repo, j.RunID).Return(run, nil)
	u.store.EXPECT().GetRunSource(mock.Anything, j.RunID).Return(domain.CIRunSource{RunID: j.RunID, Repo: j.Repo}, nil)
	// A fresh slice each time: the use case ends the job in the slice it gets.
	u.store.EXPECT().ListJobs(mock.Anything, j.RunID, int32(1)).RunAndReturn(
		func(context.Context, int64, int32) ([]domain.CIJob, error) { return []domain.CIJob{j}, nil })
	u.meta.EXPECT().Get(mock.Anything, j.Repo).Return(domain.Repo{}, domain.ErrNotFound).Maybe() // nobody to tell
	obs := mocks.NewDepsRunObserver(t)
	obs.EXPECT().CheckDepsReport(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(fmt.Errorf("%w: unexpected report path %q", domain.ErrInvalid, "evil.sh"))
	obs.EXPECT().DepsJobEnded(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	u.ci.SetDepsObserver(obs)
	report := &ports.DepsReport{Files: []ports.DepsFile{{Path: "evil.sh"}}}
	return &refusalRig{ciUnit: u, job: j, params: ports.CompleteJobParams{JobID: j.ID, Conclusion: domain.CISuccess, DepsReport: report}}
}

func (r *refusalRig) complete() (ports.CompleteJobResult, error) {
	return r.ci.CompleteJob(context.Background(), jobSecret, r.params)
}

func TestDepsRefusalIsReturnedWhenItsLogLineCannotBeWritten(t *testing.T) {
	t.Parallel()
	r := newRefusalRig(t)
	r.store.EXPECT().Apply(mock.Anything, mock.Anything).Return(uint64(1), nil)
	r.logs.EXPECT().Count(mock.Anything, mock.Anything).Return(0, nil).Maybe()
	r.logs.EXPECT().AppendForced(mock.Anything, mock.Anything, mock.Anything).Return(0, errors.New("disk full")).Once()

	res, err := r.complete()
	require.NoError(t, err)
	assert.True(t, res.Applied)
	assert.Equal(t, domain.CIFailure, res.Conclusion)
	assert.Contains(t, res.Error, "the server refused the dependency result: ")
}

func TestDepsRefusalIsLoggedOnceWhenTheFlushFailsAndIsRetried(t *testing.T) {
	t.Parallel()
	r := newRefusalRig(t)
	r.store.EXPECT().Apply(mock.Anything, mock.Anything).Return(uint64(0), errors.New("store down")).Once()
	r.store.EXPECT().Apply(mock.Anything, mock.Anything).Return(uint64(1), nil).Once()
	r.logs.EXPECT().Count(mock.Anything, mock.Anything).Return(0, nil).Maybe()
	var logged []domain.CILogLine
	r.logs.EXPECT().AppendForced(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ ports.CILogRef, lines []domain.CILogLine) (int64, error) {
			logged = append(logged, lines...)
			return int64(len(logged)), nil
		}).Maybe()

	_, err := r.complete()
	require.Error(t, err)
	assert.Empty(t, logged, "nothing is logged before the job ended")

	res, err := r.complete()
	require.NoError(t, err)
	assert.True(t, res.Applied)
	assert.Equal(t, domain.CIFailure, res.Conclusion)
	require.Len(t, logged, 1)
	assert.True(t, strings.HasPrefix(logged[0].Text, "##[error]"))
	assert.Contains(t, logged[0].Text, strings.SplitN(res.Error, "\n", 2)[0])
}
