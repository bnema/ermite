// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"slices"
	"sync"
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

// memDeps is an in-memory ports.DepsStore over several repositories.
type memDeps struct {
	ports.DepsStore // the methods the queue does not use panic
	mu              sync.Mutex
	recs            map[domain.RepoName]domain.RepoDeps
}

func (m *memDeps) Get(_ context.Context, repo domain.RepoName) (domain.RepoDeps, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.recs[repo]
	if !ok {
		return domain.RepoDeps{Repo: repo}, nil
	}
	return cloneDeps(d), nil
}

func (m *memDeps) List(context.Context) ([]domain.RepoDeps, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.RepoDeps
	for _, d := range m.recs {
		out = append(out, cloneDeps(d))
	}
	slices.SortFunc(out, func(a, b domain.RepoDeps) int { return cmpStr(a.Repo.String(), b.Repo.String()) })
	return out, nil
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (m *memDeps) Mutate(_ context.Context, repo domain.RepoName, fn func(*domain.RepoDeps) (bool, error)) (domain.RepoDeps, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := cloneDeps(m.recs[repo])
	cp.Repo = repo
	if _, err := fn(&cp); err != nil {
		return domain.RepoDeps{}, err
	}
	m.recs[repo] = cp
	return cloneDeps(cp), nil
}

func (m *memDeps) Ignore(context.Context, domain.RepoName, []domain.DepRef) error { return nil }

func (m *memDeps) state(repo domain.RepoName) domain.DepsState {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.recs[repo]
	return d.Base().State
}

// queueRig is Bernard over memDeps with a runner-side ecosystem, a mocked
// run starter and the jobs waiting for a runner.
type queueRig struct {
	t      *testing.T
	store  *memDeps
	b      *usecase.Bernard
	bus    *mocks.EventBus
	events chan domain.Event

	mu      sync.Mutex
	started []domain.RepoName
	waiting []domain.CIJob
	jobsErr error
	reads   int
}

func (q *queueRig) jobReads() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reads
}

var queueUp = domain.DepUpdate{DepRef: domain.DepRef{Ecosystem: "custom", Name: "lib", Version: "v2"}, Current: "v1"}

func newQueueRig(t *testing.T, limit int) *queueRig {
	t.Helper()
	q := &queueRig{t: t, store: &memDeps{recs: map[domain.RepoName]domain.RepoDeps{}}, bus: mocks.NewEventBus(t), events: make(chan domain.Event, 8)}
	eco := mocks.NewDepsEcosystem(t)
	eco.EXPECT().Name().Return("custom").Maybe()
	eco.EXPECT().ApplyInServer().Return(false).Maybe()
	meta, repos, git, runs, lister, jobs, clock := mocks.NewRepoMetaStore(t), mocks.NewGitRepos(t), mocks.NewMergeGit(t), mocks.NewDepsRunner(t), mocks.NewRunnerLister(t), mocks.NewCIReadyJobLister(t), mocks.NewClock(t)
	clock.EXPECT().Now().Return(time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)).Maybe()
	repos.EXPECT().DefaultBranch(mock.Anything, mock.Anything).Return("main", true, nil).Maybe()
	git.EXPECT().BranchHead(mock.Anything, mock.Anything, "main").Return(oid("a"), nil).Maybe()
	lister.EXPECT().ListRunners(mock.Anything).Return([]domain.CIRunner{{Labels: []string{"ubuntu-latest"}, Kinds: []string{"deps"}}}, nil).Maybe()
	jobs.EXPECT().ListReadyJobs(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, int) ([]domain.CIJob, error) {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.reads++
		return slices.Clone(q.waiting), q.jobsErr
	}).Maybe()
	runs.EXPECT().StartDepsRun(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p ports.DepsRunParams) (domain.CIRun, error) {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.started = append(q.started, p.Repo)
		return domain.CIRun{Repo: p.Repo, ID: int64(len(q.started))}, nil
	}).Maybe()
	runs.EXPECT().CancelDepsRun(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	runs.EXPECT().GetDepsRun(mock.Anything, mock.Anything, mock.Anything).Return(domain.CIRun{Status: domain.CIInProgress}, nil).Maybe()
	q.bus.EXPECT().Publish(mock.Anything, mock.Anything).Maybe()
	q.bus.EXPECT().Subscribe(mock.Anything).Return((<-chan domain.Event)(q.events)).Maybe()
	b, err := usecase.NewBernard(usecase.BernardConfig{
		Store: q.store, Meta: meta, Reader: mocks.NewRepoReader(t), Ecosystems: []ports.DepsEcosystem{eco},
		Registry: func() ports.DepsRegistry { return mocks.NewDepsRegistry(t) }, Bus: q.bus, Clock: clock, CheckAt: "0 6 * * *", Tick: time.Hour,
		Repos: repos, Git: git, Runs: runs, Runners: lister, RunnerLabel: "ubuntu-latest", Jobs: jobs, Concurrency: limit,
	})
	require.NoError(t, err)
	q.b = b
	return q
}

// drafted stores a batch of repo whose draft is pushed, waiting for a slot.
func (q *queueRig) drafted(repo string, manual bool, minute int) domain.RepoName {
	n := name(q.t, repo)
	q.store.recs[n] = domain.RepoDeps{Repo: n, Batches: []domain.DepBatch{{
		Updates: []domain.DepUpdate{queueUp}, MRUpdates: []domain.DepUpdate{queueUp}, MR: 3, Head: oid("d"),
		State: domain.DepsDrafted, Manual: manual, QueuedAt: time.Date(2026, 11, 4, 7, minute, 0, 0, time.UTC),
	}}}
	return n
}

func (q *queueRig) startedRepos() []domain.RepoName {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.started)
}

func TestBernardQueueRespectsConcurrency(t *testing.T) {
	q := newQueueRig(t, 2)
	a, b, c := q.drafted("a", false, 1), q.drafted("b", false, 2), q.drafted("c", false, 3)
	q.b.Drain(t.Context())
	assert.Equal(t, []domain.RepoName{a, b}, q.startedRepos(), "two slots, oldest first")
	assert.Equal(t, domain.DepsApplying, q.store.state(a))
	assert.Equal(t, domain.DepsDrafted, q.store.state(c))

	queue, order, err := q.b.Queue(t.Context(), domain.Owner())
	require.NoError(t, err)
	assert.Equal(t, ports.DepsQueue{Running: 2, Limit: 2, Queued: 1}, queue)
	assert.Equal(t, 1, domain.DepsQueuePosition(order, c, ""))

	q.b.Drain(t.Context())
	assert.Len(t, q.startedRepos(), 2, "still no slot")
	_, err = q.store.Mutate(t.Context(), a, func(d *domain.RepoDeps) (bool, error) { d.Base().State = domain.DepsIdle; return true, nil })
	require.NoError(t, err)
	q.b.Drain(t.Context())
	assert.Equal(t, []domain.RepoName{a, b, c}, q.startedRepos(), "a slot freed")
}

func TestBernardManualBeforeAuto(t *testing.T) {
	q := newQueueRig(t, 1)
	q.drafted("auto", false, 1)
	manual := q.drafted("manual", true, 9)
	q.b.Drain(t.Context())
	assert.Equal(t, []domain.RepoName{manual}, q.startedRepos())
}

func TestBernardAutoWaitsForWorkflowJobs(t *testing.T) {
	q := newQueueRig(t, 2)
	q.waiting = []domain.CIJob{{Deps: &domain.CIDepsJob{}}, {}} // a deps job and a normal one
	auto := q.drafted("auto", false, 1)
	q.b.Drain(t.Context())
	assert.Empty(t, q.startedRepos(), "normal CI goes first")
	manual := q.drafted("manual", true, 2)
	q.b.Drain(t.Context())
	assert.Equal(t, []domain.RepoName{manual}, q.startedRepos(), "a manual run does not wait")

	q.mu.Lock()
	q.waiting = []domain.CIJob{{Deps: &domain.CIDepsJob{}}} // only dependency jobs wait
	q.mu.Unlock()
	q.b.Drain(t.Context())
	assert.Equal(t, []domain.RepoName{manual, auto}, q.startedRepos())
}

func TestBernardQueueResumesAfterRestart(t *testing.T) {
	q := newQueueRig(t, 1)
	a := q.drafted("a", true, 1) // left Drafted by a previous process
	q.b.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		q.b.Close(ctx)
	})
	require.Eventually(t, func() bool { return q.store.state(a) == domain.DepsApplying }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []domain.RepoName{a}, q.startedRepos())
}

// Closing the draft of a batch waiting for a slot, or ignoring a batch whose
// server step has not run, takes it out of the queue.
func TestBernardCloseWhileQueued(t *testing.T) {
	t.Run("close a drafted batch", func(t *testing.T) {
		q := newQueueRig(t, 1)
		a := q.drafted("a", true, 1)
		other := name(t, "z") // holds the only slot, so the batch stays put
		q.store.recs[other] = domain.RepoDeps{Repo: other, Batches: []domain.DepBatch{{State: domain.DepsApplying, RunID: 50}}}
		q.b.Start()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			q.b.Close(ctx)
		})
		q.events <- domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: a, Number: 3}}
		require.Eventually(t, func() bool { return q.store.state(a) == domain.DepsIdle }, 5*time.Second, 5*time.Millisecond)
		_, err := q.store.Mutate(t.Context(), other, func(d *domain.RepoDeps) (bool, error) { d.Base().State = domain.DepsIdle; return true, nil })
		require.NoError(t, err)
		q.b.Drain(t.Context())
		assert.Empty(t, q.startedRepos())
	})
	t.Run("ignore a queued batch", func(t *testing.T) {
		q := newQueueRig(t, 1)
		a := q.drafted("a", true, 1)
		q.store.recs[a].Batches[0].State = domain.DepsQueued
		q.b.MetaForTest().(*mocks.RepoMetaStore).EXPECT().Get(mock.Anything, a).Return(domain.Repo{Name: a}, nil)
		_, err := q.b.Ignore(t.Context(), domain.Owner(), a, "", []domain.DepRef{queueUp.DepRef})
		require.NoError(t, err)
		assert.Equal(t, domain.DepsIdle, q.store.state(a))
		q.b.Drain(t.Context())
		assert.Empty(t, q.startedRepos())
	})
}

// Normal jobs behind many dependency jobs still hold an automatic run back;
// jobs that cannot be read hold it back too; the jobs are read only when an
// automatic batch would take a slot.
func TestBernardAutoWaitsForEveryReadyJob(t *testing.T) {
	q := newQueueRig(t, 1)
	for range 100 {
		q.waiting = append(q.waiting, domain.CIJob{Deps: &domain.CIDepsJob{}})
	}
	q.waiting = append(q.waiting, domain.CIJob{})
	auto := q.drafted("auto", false, 1)
	q.b.Drain(t.Context())
	assert.Empty(t, q.startedRepos())

	q.mu.Lock()
	q.waiting, q.jobsErr = nil, errors.New("store down")
	q.mu.Unlock()
	q.b.Drain(t.Context())
	assert.Empty(t, q.startedRepos(), "an unreadable queue of jobs holds automatic runs")

	q.mu.Lock()
	q.jobsErr = nil
	q.mu.Unlock()
	q.b.Drain(t.Context())
	assert.Equal(t, []domain.RepoName{auto}, q.startedRepos())
	reads := q.jobReads()
	q.drafted("manual", true, 2)
	q.b.Drain(t.Context())
	assert.Equal(t, reads, q.jobReads(), "a manual batch does not read the jobs")
}
