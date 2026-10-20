// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

var errBoom = errors.New("boom")

func (f *mirrorFixture) pushes(t *testing.T, result error) *int {
	t.Helper()
	var mu sync.Mutex
	n := new(int)
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, ports.MirrorPush) (ports.MirrorPushResult, error) {
		mu.Lock()
		defer mu.Unlock()
		*n++
		return ports.MirrorPushResult{}, result
	}).Maybe()
	return n
}

func TestMirrorsNoOpThrottleForce(t *testing.T) {
	f := newMirrorFixture(t)
	f.setSnapshot(f.repo, domain.MirrorSnapshot{URL: "git@example.com:acme/tools", Refs: []domain.MirrorRef{{Name: "refs/heads/main", ID: mirrorOID(t)}}})
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, false)
	require.NoError(t, err)
	require.True(t, v.NoOp)
	_, err = f.m.Sync(context.Background(), domain.Root(), f.repo, false)
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	require.Contains(t, err.Error(), "wait 1m0s", "the interval comes from the domain constant")
	require.Contains(t, err.Error(), "last sync 0s ago")
	f.push.EXPECT().Push(mock.Anything, mock.MatchedBy(func(p ports.MirrorPush) bool {
		return len(p.Refspecs) == 1 && p.Refspecs[0] == "refs/heads/main:refs/heads/main"
	})).Return(ports.MirrorPushResult{}, nil).Once()
	v, err = f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.NoError(t, err)
	require.False(t, v.NoOp)
}

func TestMirrorsIncidentAndRecovery(t *testing.T) {
	f := newMirrorFixture(t)
	f.push.EXPECT().Push(mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, &domain.MirrorError{Class: domain.MirrorClassTransient}).Times(5)
	for i := 1; i <= 5; i++ {
		v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
		require.Error(t, err)
		require.Equal(t, i, v.Status.ConsecutiveFailures)
		if i < 5 {
			require.Equal(t, domain.MirrorFailing, v.Status.State)
			require.Equal(t, domain.MirrorBackoff(i), v.Status.NextRetryAt.Sub(v.Status.LastAttemptAt))
		} else {
			require.Equal(t, domain.MirrorPaused, v.Status.State)
		}
	}
	events := f.eventList()
	require.Len(t, events, 1, "mirror.failed once per incident")
	failed, ok := events[0].(domain.MirrorFailed)
	require.True(t, ok)
	require.Equal(t, domain.MirrorFailing, failed.State)
	// A paused mirror is not retried by the schedule.
	f.advance(24 * time.Hour)
	f.m.ScheduleNow(context.Background())
	f.drain(t)
	f.push.AssertNumberOfCalls(t, "Push", 5)
	f.push.EXPECT().Push(mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, nil).Once()
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.NoError(t, err)
	require.Equal(t, domain.MirrorOK, v.Status.State)
	events = f.eventList()
	require.Len(t, events, 2)
	require.IsType(t, domain.MirrorRecovered{}, events[1])
}

func TestMirrorsSingleFlight(t *testing.T) {
	f := newMirrorFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	calls := 0
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, ports.MirrorPush) (ports.MirrorPushResult, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(entered)
			<-release
		}
		return ports.MirrorPushResult{}, nil
	})
	done := make(chan error, 1)
	go func() { _, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true); done <- err }()
	<-entered
	type outcome struct {
		res ports.MirrorSyncResult
		err error
	}
	results := make(chan outcome, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
			results <- outcome{res, err}
		}()
	}
	wg.Wait()
	close(results)
	for o := range results {
		require.NoError(t, o.err)
		require.True(t, o.res.Queued)
		require.Equal(t, f.repo, o.res.Repo)
	}
	close(release)
	require.NoError(t, <-done)
	f.drain(t)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, calls, "twenty overlapping requests coalesce into one rerun")
}

func TestMirrorsCoalescedManualAppliesThrottle(t *testing.T) {
	f := newMirrorFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, ports.MirrorPush) (ports.MirrorPushResult, error) {
		close(entered)
		<-release
		return ports.MirrorPushResult{}, nil
	}).Once()
	done := make(chan error, 1)
	go func() { _, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true); done <- err }()
	<-entered
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, false)
	require.NoError(t, err)
	require.True(t, v.Queued)
	close(release)
	require.NoError(t, <-done)
	f.drain(t)
	// The rerun is manual and not forced: the throttle (just attempted) stops it, so no second push
	// (a second call would fail the Once expectation).
	f.push.AssertNumberOfCalls(t, "Push", 1)
}

func TestMirrorsDivergedPausedAtOnce(t *testing.T) {
	f := newMirrorFixture(t)
	f.push.EXPECT().Push(mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, &domain.MirrorError{Class: domain.MirrorClassDiverged}).Once()
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, false)
	require.Error(t, err)
	require.Equal(t, domain.MirrorPaused, v.Status.State)
	for range 3 {
		f.advance(time.Hour)
		f.m.ScheduleNow(context.Background())
	}
	f.drain(t)
	f.push.AssertNumberOfCalls(t, "Push", 1)
	require.Len(t, f.eventList(), 1)
}

func TestMirrorsCloseCancelsWorker(t *testing.T) {
	f := newMirrorFixture(t)
	entered, exited := make(chan struct{}), make(chan struct{})
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ ports.MirrorPush) (ports.MirrorPushResult, error) {
		close(entered)
		<-ctx.Done()
		close(exited)
		return ports.MirrorPushResult{}, context.Canceled
	}).Once()
	done := make(chan error, 1)
	go func() { _, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f.m.Close(ctx)
	require.NoError(t, ctx.Err())
	select {
	case <-exited:
	default:
		t.Fatal("Close returned before worker exited")
	}
	require.ErrorIs(t, <-done, context.Canceled)
	f.m.Close(ctx) // idempotent; Start cannot resurrect workers.
	f.m.Start()
	_, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.ErrorIs(t, err, context.Canceled)
}

func TestMirrorsCronBackoff(t *testing.T) {
	f := newMirrorFixture(t)
	f.push.EXPECT().Push(mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, &domain.MirrorError{Class: domain.MirrorClassTransient}).Twice()
	_, err := f.m.Sync(context.Background(), domain.Root(), f.repo, false)
	require.Error(t, err)
	f.advance(time.Minute)
	f.m.ScheduleNow(context.Background())
	f.drain(t)
	f.push.AssertNumberOfCalls(t, "Push", 1)
	f.advance(4 * time.Minute)
	f.m.ScheduleNow(context.Background())
	f.drain(t)
	f.push.AssertNumberOfCalls(t, "Push", 2)
}

func TestMirrorsCronDueAndDirtySkip(t *testing.T) {
	f := newMirrorFixture(t)
	f.pushes(t, nil)
	_, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.NoError(t, err)
	require.Equal(t, 1, f.refReads())
	f.advance(5 * time.Minute)
	f.m.ScheduleNow(context.Background()) // due and clean: skipped without reading refs
	f.drain(t)
	require.Equal(t, 1, f.refReads())
	// Merges and default branch changes both end in OnPush: an idempotent dirty mark.
	f.m.OnPush(context.Background(), f.repo, []domain.RefUpdate{{Ref: "refs/heads/main"}})
	f.m.OnPush(context.Background(), f.repo, []domain.RefUpdate{{Ref: "HEAD"}})
	f.m.ScheduleNow(context.Background()) // same minute: nothing runs, although dirty
	f.drain(t)
	require.Equal(t, 1, f.refReads())
	f.advance(5 * time.Minute)
	f.m.ScheduleNow(context.Background())
	f.drain(t)
	require.Equal(t, 2, f.refReads(), "the dirty repository is examined on the next due tick")
}

func TestMirrorsScheduleKeepsFullQueueRepositoriesForNextTicks(t *testing.T) {
	f := newMirrorFixtureWith(t, func(c *usecase.MirrorsConfig) { c.Workers, c.QueueSize = 1, 1 })
	repos := []domain.RepoName{f.repo, name(t, "b"), name(t, "c"), name(t, "d")}
	for _, r := range repos[1:] {
		f.addRepo(r)
	}
	gate := make(chan struct{})
	var mu sync.Mutex
	pushed := map[string]int{}
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p ports.MirrorPush) (ports.MirrorPushResult, error) {
		<-gate
		mu.Lock()
		pushed[p.URL]++
		mu.Unlock()
		return ports.MirrorPushResult{}, nil
	})
	f.advance(5 * time.Minute)
	f.m.ScheduleNow(context.Background())
	// The worker holds one repository and the queue one: the other two found it full.
	close(gate)
	for range 6 { // later ticks of the same minute drain the deferred set
		f.drain(t)
		f.m.ScheduleNow(context.Background())
	}
	f.drain(t)
	mu.Lock()
	defer mu.Unlock()
	for _, r := range repos {
		require.Equal(t, 1, pushed["git@example.com:acme/"+r.String()], r.String())
	}
}

func TestMirrorsSyncRefusedWhenQueueFull(t *testing.T) {
	f := newMirrorFixtureWith(t, func(c *usecase.MirrorsConfig) { c.Workers, c.QueueSize = 1, 1 })
	second, third := name(t, "b"), name(t, "c")
	f.addRepo(second)
	f.addRepo(third)
	gate := make(chan struct{})
	entered := make(chan struct{}, 8)
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, ports.MirrorPush) (ports.MirrorPushResult, error) {
		entered <- struct{}{}
		<-gate
		return ports.MirrorPushResult{}, nil
	})
	first := make(chan error, 1)
	go func() { _, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true); first <- err }()
	<-entered // the single worker is busy
	// Enqueueing is synchronous: this request takes the only queue slot, then waits and gives up.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.m.Sync(ctx, domain.Root(), second, true)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = f.m.Sync(context.Background(), domain.Root(), third, true)
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	close(gate)
	require.NoError(t, <-first)
	f.drain(t)
}

func TestMirrorsSlowPushTimestamps(t *testing.T) {
	f := newMirrorFixture(t)
	start := f.at
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, ports.MirrorPush) (ports.MirrorPushResult, error) {
		f.advance(10 * time.Minute)
		return ports.MirrorPushResult{}, &domain.MirrorError{Class: domain.MirrorClassTransient}
	}).Once()
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.Error(t, err)
	require.True(t, v.Status.LastAttemptAt.Equal(start))
	require.True(t, v.Status.NextRetryAt.Equal(start.Add(10*time.Minute+domain.MirrorBackoff(1))))
}

func TestMirrorsPushDoesNotHoldConfigLock(t *testing.T) {
	f := newMirrorFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, ports.MirrorPush) (ports.MirrorPushResult, error) {
		close(entered)
		<-release
		return ports.MirrorPushResult{}, nil
	}).Once()
	done := make(chan error, 1)
	go func() { _, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true); done <- err }()
	<-entered
	changed := make(chan error, 1)
	go func() {
		_, err := f.m.SetGlobalConfig(context.Background(), domain.Root(), ports.MirrorGlobalPatch{Cron: ptr("*/10 * * * *")})
		changed <- err
	}()
	select {
	case err := <-changed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second): // only on failure: the push holds the configuration lock
		t.Fatal("configuration change blocked by a running push")
	}
	close(release)
	require.NoError(t, <-done)
}

func TestMirrorsResultDiscardedWhenTargetChangedDuringPush(t *testing.T) {
	f := newMirrorFixture(t)
	f.setStatus(domain.MirrorStatus{Repo: f.repo, State: domain.MirrorFailing, Class: domain.MirrorClassTransient, ConsecutiveFailures: 1})
	entered, release := make(chan struct{}), make(chan struct{})
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, ports.MirrorPush) (ports.MirrorPushResult, error) {
		close(entered)
		<-release
		return ports.MirrorPushResult{}, nil
	}).Once()
	done := make(chan error, 1)
	go func() { _, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true); done <- err }()
	<-entered
	_, err := f.m.SetGlobalConfig(context.Background(), domain.Root(), ports.MirrorGlobalPatch{URLTemplate: ptr("git@other.example:acme/{repo}")})
	require.NoError(t, err)
	close(release)
	require.NoError(t, <-done)
	_, ok := f.snapshotOf(f.repo)
	require.False(t, ok, "no snapshot for a target that no longer applies")
	require.Empty(t, f.eventList(), "no mirror.recovered for the old target")
	s := f.statusOf(f.repo)
	require.Equal(t, domain.MirrorOK, s.State)
	require.True(t, s.LastSuccessAt.IsZero(), "the push result was not recorded")
}

func TestMirrorsSyncAllContinuesPastFailures(t *testing.T) {
	f := newMirrorFixture(t)
	bad, off := name(t, "bad"), name(t, "off")
	f.addRepo(bad)
	f.addRepo(off)
	f.setRepoConfig(domain.MirrorRepoConfig{Repo: off, Excluded: true})
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p ports.MirrorPush) (ports.MirrorPushResult, error) {
		if strings.HasSuffix(p.URL, "/bad") {
			return ports.MirrorPushResult{}, &domain.MirrorError{Class: domain.MirrorClassDiverged, Detail: "rejected"}
		}
		return ports.MirrorPushResult{}, nil
	})
	got, err := f.m.SyncAll(context.Background(), domain.Root(), true)
	require.NoError(t, err)
	require.Len(t, got, 2, "the excluded repository is skipped")
	byRepo := map[domain.RepoName]ports.MirrorSyncResult{}
	for _, r := range got {
		byRepo[r.Repo] = r
	}
	require.NoError(t, byRepo[f.repo].Err)
	require.Equal(t, domain.MirrorOK, byRepo[f.repo].Status.State)
	var me *domain.MirrorError
	require.ErrorAs(t, byRepo[bad].Err, &me)
	require.Equal(t, domain.MirrorPaused, byRepo[bad].Status.State)
	_, listed := byRepo[off]
	require.False(t, listed)
	_, err = f.m.SyncAll(context.Background(), tokenPrincipal(domain.ScopeRepoAdmin), true)
	require.ErrorIs(t, err, domain.ErrForbidden)
}

func TestMirrorsLocalFailuresAreInternalAndNeverPush(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*mirrorFixture)
	}{
		{"ListRefs", func(f *mirrorFixture) { f.mu.Lock(); f.refsErr = errBoom; f.mu.Unlock() }},
		{"DefaultBranch", func(f *mirrorFixture) { f.mu.Lock(); f.branchErr = errBoom; f.mu.Unlock() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMirrorFixture(t)
			tc.set(f)
			v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true) // a Push call would fail the strict mock
			var me *domain.MirrorError
			require.ErrorAs(t, err, &me)
			require.Equal(t, domain.MirrorClassInternal, me.Class)
			require.NotContains(t, me.Detail, "boom")
			require.Equal(t, domain.MirrorFailing, v.Status.State)
		})
	}
}

func TestMirrorsMissingSecretsKeyFailsBeforeReadingRefs(t *testing.T) {
	f := newMirrorFixtureWith(t, func(c *usecase.MirrorsConfig) { c.Sealer = nil })
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	var me *domain.MirrorError
	require.ErrorAs(t, err, &me)
	require.Equal(t, domain.MirrorClassInternal, me.Class)
	require.Contains(t, me.Detail, "ERMITE_SECRETS_KEY")
	require.Equal(t, domain.MirrorFailing, v.Status.State)
	require.Zero(t, f.refReads())
}

func TestMirrorsErrorsAndLogsDoNotExposeSecrets(t *testing.T) {
	f := newMirrorFixture(t)
	var logs bytes.Buffer
	ctx := zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &logs}))
	credentialURL := "ssh://alice:secret-password@example.com/acme/tools"
	_, err := f.m.SetRepoMirror(ctx, domain.Root(), domain.MirrorRepoConfig{Repo: f.repo, URL: credentialURL})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-password")
	require.NotContains(t, err.Error(), "alice")
	f.push.EXPECT().Push(mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, errors.New("local key PRIVATE-KEY-MARKER ssh://alice:secret-password@example.com/acme/tools")).Once()
	_, err = f.m.Sync(ctx, domain.Root(), f.repo, true)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "PRIVATE-KEY-MARKER")
	require.NotContains(t, err.Error(), "secret-password")
	require.NotContains(t, logs.String(), "PRIVATE-KEY-MARKER")
	require.NotContains(t, logs.String(), "secret-password")
}

func TestMirrorsKeyLazyGenerationAndRotation(t *testing.T) {
	f := newKeyFixture(t)
	f.keys.EXPECT().GetMirrorKey(mock.Anything).Return([]byte(nil), time.Time{}, domain.ErrNotFound).Once()
	f.keys.EXPECT().PutMirrorKey(mock.Anything, mock.Anything, now, true).Return(nil).Once()
	f.keys.EXPECT().PutMirrorKey(mock.Anything, mock.Anything, now, false).Return(nil).Once()
	first, err := f.m.ShowKey(context.Background(), domain.Root())
	require.NoError(t, err)
	cached, err := f.m.ShowKey(context.Background(), domain.Root())
	require.NoError(t, err)
	require.Equal(t, first, cached)
	rotated, err := f.m.RotateKey(context.Background(), domain.Root())
	require.NoError(t, err)
	require.Equal(t, first.Fingerprint, rotated.Previous.Fingerprint)
	require.NotEqual(t, first.Fingerprint, rotated.Key.Fingerprint)
	require.Contains(t, rotated.Key.AuthorizedKey, "ermite-mirror@forge.example.com")
	require.Len(t, f.seeds(), 2)
	require.NotEqual(t, f.seeds()[0], f.seeds()[1])
}

func TestMirrorsRotateWithoutExistingKeyGeneratesOnce(t *testing.T) {
	f := newKeyFixture(t)
	f.keys.EXPECT().GetMirrorKey(mock.Anything).Return([]byte(nil), time.Time{}, domain.ErrNotFound).Once()
	f.keys.EXPECT().PutMirrorKey(mock.Anything, mock.Anything, now, false).Return(nil).Once() // never ifAbsent=true
	rotated, err := f.m.RotateKey(context.Background(), domain.Root())
	require.NoError(t, err)
	require.Empty(t, rotated.Previous.Fingerprint)
	require.NotEmpty(t, rotated.Key.Fingerprint)
	require.Len(t, f.seeds(), 1, "no throwaway key")
}

func TestMirrorsRotateReplacesUnreadableKey(t *testing.T) {
	f := newKeyFixture(t)
	f.keys.EXPECT().GetMirrorKey(mock.Anything).Return([]byte("garbage"), now, nil).Once()
	f.sealerOpenFails = true
	f.keys.EXPECT().PutMirrorKey(mock.Anything, mock.Anything, now, false).Return(nil).Once()
	rotated, err := f.m.RotateKey(context.Background(), domain.Root())
	require.NoError(t, err)
	require.Empty(t, rotated.Previous.Fingerprint)
}

func TestMirrorsAuthorizationAndMissingSecretsKey(t *testing.T) {
	f := newMirrorFixtureWith(t, func(c *usecase.MirrorsConfig) { c.Sealer = nil })
	_, err := f.m.ShowKey(context.Background(), domain.Anonymous())
	require.ErrorIs(t, err, domain.ErrUnauthenticated)
	_, err = f.m.ShowKey(context.Background(), domain.Root())
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	require.Contains(t, err.Error(), "ERMITE_SECRETS_KEY")
	_, err = f.m.RotateKey(context.Background(), domain.Root())
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	_, err = f.m.RotateKey(context.Background(), tokenPrincipal(domain.ScopeRepoRead))
	require.ErrorIs(t, err, domain.ErrForbidden)
}

// gatedPush makes every push announce itself on entered and block until release is closed.
func (f *mirrorFixture) gatedPush(release <-chan struct{}) <-chan struct{} {
	entered := make(chan struct{}, 16)
	f.push.EXPECT().Push(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ ports.MirrorPush) (ports.MirrorPushResult, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ports.MirrorPushResult{}, nil
	}).Maybe()
	return entered
}

func TestMirrorsSyncAllRunsRepositoriesConcurrently(t *testing.T) {
	f := newMirrorFixtureWith(t, func(c *usecase.MirrorsConfig) { c.Workers = 3; c.SyncWait = time.Minute })
	f.addRepo(name(t, "b"))
	f.addRepo(name(t, "c"))
	release := make(chan struct{})
	entered := f.gatedPush(release)
	type outcome struct {
		got []ports.MirrorSyncResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		got, err := f.m.SyncAll(context.Background(), domain.Root(), true)
		done <- outcome{got, err}
	}()
	// All three pushes are in flight together: a sequential SyncAll would never get past the first.
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d pushes started: repositories are not synced concurrently", i)
		}
	}
	close(release)
	o := <-done
	require.NoError(t, o.err)
	require.Len(t, o.got, 3)
	for i, want := range []string{"tools", "b", "c"} {
		r := o.got[i]
		require.Equal(t, want, r.Repo.String(), "results keep the listing order")
		require.NoError(t, r.Err)
		require.False(t, r.Queued)
		require.Equal(t, domain.MirrorOK, r.Status.State)
	}
}

func TestMirrorsSyncReturnsQueuedWhenPushOutlastsTheWait(t *testing.T) {
	f := newMirrorFixtureWith(t, func(c *usecase.MirrorsConfig) { c.SyncWait = 20 * time.Millisecond })
	other := name(t, "other")
	f.addRepo(other)
	release := make(chan struct{})
	entered := f.gatedPush(release)

	res, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.NoError(t, err)
	require.True(t, res.Queued, "the push is still running at the bound")
	require.Equal(t, f.repo, res.Repo)
	<-entered

	all, err := f.m.SyncAll(context.Background(), domain.Root(), true)
	require.NoError(t, err)
	require.Len(t, all, 2)
	for _, r := range all {
		require.NoError(t, r.Err)
		require.True(t, r.Queued, "%s is coalesced or still running", r.Repo)
		require.Equal(t, r.Repo, r.Status.Repo)
	}

	close(release)
	f.drain(t)
	require.Equal(t, domain.MirrorOK, f.statusOf(f.repo).State, "the sync completed in the background")
}

func TestMirrorsSyncAllHonoursCallerCancellation(t *testing.T) {
	f := newMirrorFixtureWith(t, func(c *usecase.MirrorsConfig) { c.SyncWait = time.Minute })
	release := make(chan struct{})
	entered := f.gatedPush(release)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := f.m.SyncAll(ctx, domain.Root(), true); errc <- err }()
	<-entered
	cancel()
	require.ErrorIs(t, <-errc, context.Canceled)
	close(release)
}
