// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
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

func jobsRig(t *testing.T) (*bernardRig, *[]string) {
	t.Helper()
	r := newBernardRig(t, "0 6 * * *")
	var revoked []string
	b, err := usecase.NewBernard(usecase.BernardConfig{
		Store: r.store, Meta: r.meta, Reader: r.reader, Registry: func() ports.DepsRegistry { return r.reg },
		Bus: r.bus, Clock: r.clock, CheckAt: "0 6 * * *", JobTimeout: 10 * time.Minute, Tick: time.Hour,
		RevokeJob: func(_ context.Context, id string) { revoked = append(revoked, id) },
	})
	require.NoError(t, err)
	r.b = b
	return r, &revoked
}

// enqueueApplying queues a job and records the repository as applying it, as Open does.
func enqueueApplying(r *bernardRig) string {
	id := r.b.Enqueue(r.repo, "abc", oneRef, "1.25")
	r.rec.State, r.rec.JobID = domain.DepsApplying, id
	return id
}

var oneRef = []domain.DepRef{{Ecosystem: domain.EcosystemGoMod, Name: "x/y", Version: "v1.2.3"}}

func TestBernardEnqueueIsOnePerRepo(t *testing.T) {
	t.Parallel()
	r, _ := jobsRig(t)
	id := r.b.Enqueue(r.repo, "abc", oneRef, "1.25")
	assert.Regexp(t, `^deps-[0-9a-f]{16}$`, id)
	assert.Equal(t, id, r.b.Enqueue(r.repo, "def", oneRef, "1.25"))
	other := r.b.Enqueue(name(t, "other"), "abc", oneRef, "1.25")
	assert.NotEqual(t, id, other)
}

func TestBernardNextAssignsAndChecksTheRunner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, _ := jobsRig(t)
	id := r.b.Enqueue(r.repo, "abc", oneRef, "1.25")

	a, ok, err := r.b.Next(ctx, "r1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, id, a.JobID)
	assert.Equal(t, "abc", a.Sha)
	assert.Equal(t, r.at.Add(10*time.Minute), a.Deadline)
	_, ok, err = r.b.Next(ctx, "r2")
	require.NoError(t, err)
	assert.False(t, ok, "assigned once")

	assert.ErrorIs(t, r.b.Heartbeat(ctx, "r2", id), domain.ErrNotFound)
	assert.ErrorIs(t, r.b.Update(ctx, "r2", id, nil, nil), domain.ErrNotFound)
	_, err = r.b.AppendLog(ctx, "r2", id, 0, nil)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.ErrorIs(t, r.b.Complete(ctx, "r2", id, nil, domain.CISuccess), domain.ErrNotFound)
	assert.ErrorIs(t, r.b.Heartbeat(ctx, "r1", "deps-unknown"), domain.ErrNotFound)
	require.NoError(t, r.b.Heartbeat(ctx, "r1", id))
}

func TestBernardDeadlineExpiryFailsTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, revoked := jobsRig(t)
	id := enqueueApplying(r)
	_, ok, err := r.b.Next(ctx, "r1")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = r.b.AppendLog(ctx, "r1", id, 0, []domain.CILogLine{{Text: "go get failed"}})
	require.NoError(t, err)

	r.at = r.at.Add(10 * time.Minute)
	assert.ErrorIs(t, r.b.Heartbeat(ctx, "r1", id), domain.ErrNotFound)
	assert.Equal(t, domain.DepsFailed, r.rec.State)
	assert.True(t, strings.HasPrefix(r.rec.Error, "deps job timed out"), r.rec.Error)
	assert.Contains(t, r.rec.Error, "go get failed")
	assert.Equal(t, []string{id}, *revoked)
	// The job left the queue: the repository can queue a new one.
	assert.NotEqual(t, id, r.b.Enqueue(r.repo, "abc", oneRef, "1.25"))
}

func TestBernardExpiryOnNext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, _ := jobsRig(t)
	enqueueApplying(r)
	_, _, err := r.b.Next(ctx, "r1")
	require.NoError(t, err)
	r.at = r.at.Add(11 * time.Minute)
	_, ok, err := r.b.Next(ctx, "r2")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, domain.DepsFailed, r.rec.State)
}

func TestBernardRestartFailsApplyingRecords(t *testing.T) {
	t.Parallel()
	r, _ := jobsRig(t)
	r.rec.State, r.rec.JobID = domain.DepsApplying, "deps-old"
	r.store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{r.rec, {Repo: name(t, "idle")}}, nil)
	r.bus.EXPECT().Subscribe(mock.Anything).Return(make(<-chan domain.Event)).Maybe()
	r.b.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.b.Close(ctx)
	assert.Equal(t, domain.DepsFailed, r.rec.State)
	assert.Equal(t, "interrupted by a restart", r.rec.Error)
	assert.Empty(t, r.rec.JobID)
}

func TestBernardCompleteReportErrorFailsWithLogTail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, revoked := jobsRig(t)
	id := enqueueApplying(r)
	_, _, err := r.b.Next(ctx, "r1")
	require.NoError(t, err)
	next, err := r.b.AppendLog(ctx, "r1", id, 0, []domain.CILogLine{{Text: "line one"}, {Text: "line two"}})
	require.NoError(t, err)
	assert.EqualValues(t, 2, next)
	_, err = r.b.AppendLog(ctx, "r1", id, 5, nil)
	var gap *ports.ErrCILogGap
	require.ErrorAs(t, err, &gap)

	require.NoError(t, r.b.Complete(ctx, "r1", id, &ports.DepsReport{Error: "go get: no such version"}, domain.CIFailure))
	assert.Equal(t, domain.DepsFailed, r.rec.State)
	assert.True(t, strings.HasPrefix(r.rec.Error, "go get: no such version\n"), r.rec.Error)
	assert.Contains(t, r.rec.Error, "line two")
	assert.Equal(t, []string{id}, *revoked)
	assert.ErrorIs(t, r.b.Complete(ctx, "r1", id, nil, domain.CISuccess), domain.ErrNotFound, "ended once")
}

func TestBernardCompleteNonSuccessAndUnappliableReportFail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, _ := jobsRig(t)
	for _, c := range []struct {
		conclusion domain.CIConclusion
		want       string
	}{
		{domain.CITimedOut, "the runner reported"},
		{domain.CISuccess, "no Go module ecosystem"},
	} {
		id := enqueueApplying(r)
		_, _, err := r.b.Next(ctx, "r1")
		require.NoError(t, err)
		require.NoError(t, r.b.Complete(ctx, "r1", id, &ports.DepsReport{}, c.conclusion))
		assert.Equal(t, domain.DepsFailed, r.rec.State)
		assert.Contains(t, r.rec.Error, c.want)
	}
}

func TestBernardLogTailIsBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, _ := jobsRig(t)
	id := enqueueApplying(r)
	_, _, err := r.b.Next(ctx, "r1")
	require.NoError(t, err)
	big := strings.Repeat("a", 100<<10)
	_, err = r.b.AppendLog(ctx, "r1", id, 0, []domain.CILogLine{{Text: big}, {Text: "the end"}})
	require.NoError(t, err)
	require.NoError(t, r.b.Complete(ctx, "r1", id, &ports.DepsReport{Error: "x"}, domain.CIFailure))
	assert.LessOrEqual(t, len(r.rec.Error), 2048)
	assert.True(t, strings.HasSuffix(r.rec.Error, "the end"))
}

func TestBernardUntakenJobExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, revoked := jobsRig(t)
	id := enqueueApplying(r)
	r.at = r.at.Add(10 * time.Minute)
	_, ok, err := r.b.Next(ctx, "r1")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, domain.DepsFailed, r.rec.State)
	assert.Contains(t, r.rec.Error, "timed out")
	assert.Equal(t, []string{id}, *revoked)
}

// TestBernardExpiryRunsWhileAScanIsBlocked: job expiry has its own ticker and
// does not wait for the daily scan.
func TestBernardExpiryRunsWhileAScanIsBlocked(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	var now atomic.Int64
	now.Store(r.at.UnixNano())
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().RunAndReturn(func() time.Time { return time.Unix(0, now.Load()).UTC() }).Maybe()
	expired := make(chan string, 1)
	b, err := usecase.NewBernard(usecase.BernardConfig{
		Store: r.store, Meta: r.meta, Reader: r.reader, Registry: func() ports.DepsRegistry { return r.reg },
		Bus: r.bus, Clock: clock, CheckAt: "0 6 * * *", JobTimeout: 10 * time.Minute, Tick: 5 * time.Millisecond,
		RevokeJob: func(_ context.Context, id string) { expired <- id },
	})
	require.NoError(t, err)
	r.b = b
	r.rec.State = domain.DepsApplying
	r.store.EXPECT().List(mock.Anything).Return(nil, nil).Maybe()
	r.bus.EXPECT().Subscribe(mock.Anything).Return(make(<-chan domain.Event)).Maybe()
	release := make(chan struct{})
	scanning := make(chan struct{})
	var once sync.Once
	r.meta.EXPECT().List(mock.Anything).RunAndReturn(func(ctx context.Context) ([]domain.Repo, error) {
		once.Do(func() { close(scanning) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	})
	id := b.Enqueue(r.repo, "abc", oneRef, "1.25")
	r.rec.JobID = id
	b.Start()
	select {
	case <-scanning: // 06:00 is due: the scan starts and blocks
	case <-time.After(5 * time.Second):
		t.Fatal("the scan did not start")
	}
	now.Store(r.at.Add(11 * time.Minute).UnixNano()) // past the job's deadline
	select {
	case got := <-expired:
		assert.Equal(t, id, got)
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not expire while the scan was blocked")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.Close(ctx)
	assert.Equal(t, domain.DepsFailed, r.rec.State)
}

// TestBernardScheduledCheckHasADeadline: each repository of a scheduled scan
// is checked under the 60 s deadline of a manual check.
func TestBernardScheduledCheckHasADeadline(t *testing.T) {
	r, _ := jobsRig(t)
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: r.repo}}, nil)
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	var left time.Duration
	r.reader.EXPECT().ResolveRevision(mock.Anything, r.repo, domain.Revision{}).RunAndReturn(
		func(ctx context.Context, _ domain.RepoName, _ domain.Revision) (domain.ResolvedRevision, error) {
			dl, ok := ctx.Deadline()
			require.True(t, ok, "no deadline")
			left = time.Until(dl)
			return domain.ResolvedRevision{}, domain.ErrNotFound
		})
	r.b.ScheduleNow(context.Background())
	assert.Positive(t, left)
	assert.LessOrEqual(t, left, 60*time.Second)
}
