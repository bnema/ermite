// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"github.com/bnema/zerowrap"
	"sync"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type mirrorFixture struct {
	m        *usecase.Mirrors
	store    *mocks.MirrorStore
	push     *mocks.MirrorPusher
	reader   *mocks.RepoReader
	bus      *mocks.EventBus
	keys     *mocks.MirrorKeyStore
	sealer   *mocks.SecretSealer
	mu       sync.Mutex
	at       time.Time
	status   domain.MirrorStatus
	snapshot domain.MirrorSnapshot
	events   []domain.Event
	repo     domain.RepoName
}

func newMirrorFixture(t *testing.T) *mirrorFixture {
	t.Helper()
	f := &mirrorFixture{store: mocks.NewMirrorStore(t), push: mocks.NewMirrorPusher(t), reader: mocks.NewRepoReader(t), bus: mocks.NewEventBus(t), keys: mocks.NewMirrorKeyStore(t), sealer: mocks.NewSecretSealer(t), at: now, repo: name(t, "tools")}
	meta, git, clock := mocks.NewRepoMetaStore(t), mocks.NewGitRepos(t), mocks.NewClock(t)
	clock.On("Now").Return(func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.at })
	meta.On("Get", mock.Anything, f.repo).Return(domain.Repo{Name: f.repo, Visibility: domain.VisibilityPublic, CreatedAt: now}, nil).Maybe()
	meta.On("List", mock.Anything).Return([]domain.Repo{{Name: f.repo}}, nil).Maybe()
	f.store.On("GetGlobal", mock.Anything).Return(domain.MirrorGlobalConfig{Enabled: true, URLTemplate: "git@example.com:acme/{repo}", Cron: "*/5 * * * *"}, nil)
	f.store.On("GetRepoConfig", mock.Anything, f.repo).Return(domain.MirrorRepoConfig{}, domain.ErrNotFound)
	f.store.On("GetStatus", mock.Anything, f.repo).Return(func(context.Context, domain.RepoName) domain.MirrorStatus {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.status
	}, nil)
	f.store.On("PutStatus", mock.Anything, mock.Anything).Run(func(a mock.Arguments) { f.mu.Lock(); defer f.mu.Unlock(); f.status = a.Get(1).(domain.MirrorStatus) }).Return(nil).Maybe()
	f.store.On("GetSnapshot", mock.Anything, f.repo).Return(func(context.Context, domain.RepoName) domain.MirrorSnapshot {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.snapshot
	}, nil).Maybe()
	f.store.On("PutSnapshot", mock.Anything, f.repo, mock.Anything).Run(func(a mock.Arguments) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.snapshot = a.Get(2).(domain.MirrorSnapshot)
	}).Return(nil).Maybe()
	f.reader.On("ListRefs", mock.Anything, f.repo).Return([]domain.Ref{{Name: "refs/heads/main", ID: mirrorOID(t)}}, nil).Maybe()
	git.On("DefaultBranch", mock.Anything, f.repo).Return("main", true, nil).Maybe()
	f.bus.On("Publish", mock.Anything, mock.Anything).Run(func(a mock.Arguments) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = append(f.events, a.Get(1).(domain.Event))
	}).Maybe()
	f.keys.On("GetMirrorKey", mock.Anything).Return([]byte("sealed"), now, nil).Maybe()
	f.sealer.On("Open", []byte("sealed"), []byte(domain.MirrorKeyPath)).Return(make([]byte, ed25519.SeedSize), nil).Maybe()
	f.status = domain.MirrorStatus{Repo: f.repo, State: domain.MirrorOK}
	f.m = usecase.NewMirrors(usecase.MirrorsConfig{Store: f.store, Keys: f.keys, Sealer: f.sealer, Pusher: f.push, Meta: meta, Git: git, Reader: f.reader, Bus: f.bus, Clock: clock, ScheduleTick: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f.m.Close(ctx)
	})
	return f
}
func (f *mirrorFixture) advance(d time.Duration) { f.mu.Lock(); f.at = f.at.Add(d); f.mu.Unlock() }
func TestMirrorsNoOpThrottleForce(t *testing.T) {
	f := newMirrorFixture(t)
	f.snapshot = domain.MirrorSnapshot{URL: "git@example.com:acme/tools", Refs: []domain.MirrorRef{{Name: "refs/heads/main", ID: mirrorOID(t)}}}
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, false)
	require.NoError(t, err)
	require.True(t, v.NoOp)
	_, err = f.m.Sync(context.Background(), domain.Root(), f.repo, false)
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	f.push.On("Push", mock.Anything, mock.MatchedBy(func(p ports.MirrorPush) bool {
		return len(p.Refspecs) == 1 && p.Refspecs[0] == "refs/heads/main:refs/heads/main"
	})).Return(ports.MirrorPushResult{}, nil).Once()
	v, err = f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.NoError(t, err)
	require.False(t, v.NoOp)
}
func TestMirrorsIncidentAndRecovery(t *testing.T) {
	f := newMirrorFixture(t)
	f.push.On("Push", mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, &domain.MirrorError{Class: domain.MirrorClassTransient}).Times(5)
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
	require.Len(t, f.events, 1)
	require.IsType(t, domain.MirrorFailed{}, f.events[0])
	f.advance(24 * time.Hour)
	f.m.ScheduleNow(context.Background())
	f.push.On("Push", mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, nil).Once()
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.NoError(t, err)
	require.Equal(t, domain.MirrorOK, v.Status.State)
	require.Len(t, f.events, 2)
	require.IsType(t, domain.MirrorRecovered{}, f.events[1])
}
func TestMirrorsSingleFlight(t *testing.T) {
	f := newMirrorFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.push.On("Push", mock.Anything, mock.Anything).Run(func(mock.Arguments) { close(entered); <-release }).Return(ports.MirrorPushResult{}, nil).Once()
	done := make(chan error, 1)
	go func() { _, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true); done <- err }()
	<-entered
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
			require.NoError(t, err)
			require.True(t, v.Queued)
			require.Equal(t, f.repo, v.Status.Repo)
		}()
	}
	wg.Wait()
	rerun := make(chan struct{})
	f.push.On("Push", mock.Anything, mock.Anything).Run(func(mock.Arguments) { close(rerun) }).Return(ports.MirrorPushResult{}, nil).Once()
	close(release)
	require.NoError(t, <-done)
	select {
	case <-rerun:
	case <-time.After(time.Second):
		t.Fatal("coalesced force was lost")
	}
}
func TestMirrorsAuthorizationAndMissingSecretsKey(t *testing.T) {
	m := usecase.NewMirrors(usecase.MirrorsConfig{Clock: func() *mocks.Clock { c := mocks.NewClock(t); c.On("Now").Return(now); return c }()})
	_, err := m.ShowKey(context.Background(), domain.Anonymous())
	require.ErrorIs(t, err, domain.ErrUnauthenticated)
	_, err = m.ShowKey(context.Background(), domain.Root())
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	require.Contains(t, err.Error(), "ERMITE_SECRETS_KEY")
	_, err = m.RotateKey(context.Background(), tokenPrincipal(domain.ScopeRepoRead))
	require.True(t, errors.Is(err, domain.ErrForbidden))
}

func mirrorOID(t *testing.T) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(oidA)
	require.NoError(t, err)
	return id
}

func TestMirrorsDivergedPausedAndConfigReset(t *testing.T) {
	f := newMirrorFixture(t)
	f.push.On("Push", mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, &domain.MirrorError{Class: domain.MirrorClassDiverged}).Once()
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, false)
	require.Error(t, err)
	require.Equal(t, domain.MirrorPaused, v.Status.State)
	for i := 0; i < 3; i++ {
		f.advance(time.Hour)
		f.m.ScheduleNow(context.Background())
	}
	f.store.On("PutRepoConfig", mock.Anything, mock.Anything).Return(nil).Once()
	_, err = f.m.SetRepoMirror(context.Background(), domain.Root(), domain.MirrorRepoConfig{Repo: f.repo, URL: "git@example.com:acme/tools", Cron: "*/5 * * * *"})
	require.NoError(t, err)
	f.mu.Lock()
	s := f.status
	events := len(f.events)
	f.mu.Unlock()
	require.Equal(t, domain.MirrorOK, s.State)
	require.Zero(t, s.ConsecutiveFailures)
	require.Equal(t, 1, events)
}

func TestMirrorsKeyLazyGenerationAndRotation(t *testing.T) {
	keys, sealer, clock := mocks.NewMirrorKeyStore(t), mocks.NewSecretSealer(t), mocks.NewClock(t)
	clock.On("Now").Return(now)
	keys.On("GetMirrorKey", mock.Anything).Return([]byte(nil), time.Time{}, domain.ErrNotFound).Once()
	var seeds [][]byte
	sealer.On("Seal", mock.Anything, []byte(domain.MirrorKeyPath)).Return(func(seed, aad []byte) []byte {
		seeds = append(seeds, append([]byte(nil), seed...))
		return append([]byte("sealed:"), seed...)
	}).Twice()
	keys.On("PutMirrorKey", mock.Anything, mock.Anything, now, true).Return(nil).Once()
	keys.On("PutMirrorKey", mock.Anything, mock.Anything, now, false).Return(nil).Once()
	m := usecase.NewMirrors(usecase.MirrorsConfig{Keys: keys, Sealer: sealer, Clock: clock, PublicURL: "https://forge.example.com"})
	first, err := m.ShowKey(context.Background(), domain.Root())
	require.NoError(t, err)
	cached, err := m.ShowKey(context.Background(), domain.Root())
	require.NoError(t, err)
	require.Equal(t, first, cached)
	rotated, err := m.RotateKey(context.Background(), domain.Root())
	require.NoError(t, err)
	require.NotEqual(t, first.Fingerprint, rotated.Fingerprint)
	require.Contains(t, rotated.AuthorizedKey, "ermite-mirror@forge.example.com")
	require.Len(t, seeds, 2)
	require.NotEqual(t, seeds[0], seeds[1])
	require.Len(t, seeds[0], ed25519.SeedSize)
}

func TestMirrorsPushRecordNotification(t *testing.T) {
	f := newPushFixture(t)
	n := name(t, "tools")
	trigger := mocks.NewMirrorTrigger(t)
	updates := []domain.RefUpdate{}
	f.meta.On("Mutate", mock.Anything, n, mock.Anything).Return(domain.Repo{Name: n}, nil)
	f.git.On("DefaultBranch", mock.Anything, n).Return("main", true, nil)
	f.bus.On("Publish", mock.Anything, mock.Anything).Return()
	f.sched.On("Schedule", mock.Anything, n).Return()
	notified := make(chan struct{})
	trigger.On("OnPush", mock.Anything, n, updates).Run(func(mock.Arguments) { close(notified) }).Return().Once()
	require.NoError(t, f.pushes.WithMirror(trigger).Record(context.Background(), domain.Root(), n, updates))
	<-notified
}

func TestMirrorsCronDueAndDirtySkip(t *testing.T) {
	f := newMirrorFixture(t)
	// Establish a successful snapshot; subsequent clean cron ticks need no refs.
	f.push.On("Push", mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, nil).Once()
	_, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.NoError(t, err)
	f.reader.AssertNumberOfCalls(t, "ListRefs", 1)
	f.mu.Lock()
	f.at = f.at.Add(24 * time.Hour).Truncate(time.Hour).Add(time.Minute)
	f.mu.Unlock()
	f.m.ScheduleNow(context.Background())
	f.advance(4 * time.Minute)
	f.m.ScheduleNow(context.Background())
	f.reader.AssertNumberOfCalls(t, "ListRefs", 1)
	f.m.OnPush(context.Background(), f.repo, nil)
	read := make(chan struct{})
	f.reader.ExpectedCalls = nil
	f.reader.On("ListRefs", mock.Anything, f.repo).Run(func(mock.Arguments) { close(read) }).Return([]domain.Ref{{Name: "refs/heads/main", ID: mirrorOID(t)}}, nil).Once()
	// Same minute does not run, even though dirty.
	f.m.ScheduleNow(context.Background())
	select {
	case <-read:
		t.Fatal("same-minute cron reran")
	default:
	}
	f.advance(5 * time.Minute)
	f.m.ScheduleNow(context.Background())
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("due dirty repository not examined")
	}
}

func TestMirrorsAuthorizationMatrix(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	reader := tokenPrincipal(domain.ScopeRepoRead)
	admin := tokenPrincipal(domain.ScopeRepoAdmin)
	_, err := f.m.GetGlobalConfig(ctx, reader)
	require.ErrorIs(t, err, domain.ErrForbidden)
	_, err = f.m.SetGlobalConfig(ctx, reader, ports.MirrorGlobalPatch{})
	require.ErrorIs(t, err, domain.ErrForbidden)
	_, err = f.m.ListStatus(ctx, reader)
	require.ErrorIs(t, err, domain.ErrForbidden)
	_, err = f.m.RotateKey(ctx, admin)
	require.ErrorIs(t, err, domain.ErrForbidden)
	// Key show deliberately requires authentication, not owner rights.
	_, err = f.m.ShowKey(ctx, reader)
	require.NoError(t, err)
	_, err = f.m.ShowKey(ctx, domain.Anonymous())
	require.ErrorIs(t, err, domain.ErrUnauthenticated)
	_, err = f.m.SetRepoMirror(ctx, reader, domain.MirrorRepoConfig{Repo: f.repo})
	require.ErrorIs(t, err, domain.ErrForbidden)
	err = f.m.RemoveRepoMirror(ctx, reader, f.repo)
	require.ErrorIs(t, err, domain.ErrForbidden)
	_, err = f.m.SetRepoExcluded(ctx, reader, f.repo, true)
	require.ErrorIs(t, err, domain.ErrForbidden)
	_, err = f.m.Sync(ctx, reader, f.repo, false)
	require.ErrorIs(t, err, domain.ErrForbidden)
	f.mu.Lock()
	f.status.Detail, f.status.Reason = "admin detail", "admin reason"
	f.mu.Unlock()
	view, err := f.m.GetStatus(ctx, domain.Anonymous(), f.repo)
	require.NoError(t, err)
	require.Empty(t, view.Status.Detail)
	require.Empty(t, view.Status.Reason)
	require.Empty(t, view.Target.URL)
	view, err = f.m.GetStatus(ctx, admin, f.repo)
	require.NoError(t, err)
	require.Equal(t, "admin detail", view.Status.Detail)
	f.store.On("PutRepoConfig", mock.Anything, mock.Anything).Return(nil).Once()
	_, err = f.m.SetRepoMirror(ctx, admin, domain.MirrorRepoConfig{Repo: f.repo})
	require.NoError(t, err)
}

func TestMirrorsPrivateStatusAuthorization(t *testing.T) {
	f := newMirrorFixture(t)
	meta := mocks.NewRepoMetaStore(t)
	meta.On("Get", mock.Anything, f.repo).Return(domain.Repo{Name: f.repo, Visibility: domain.VisibilityPrivate}, nil)
	clock := mocks.NewClock(t)
	clock.On("Now").Return(now)
	m := usecase.NewMirrors(usecase.MirrorsConfig{Meta: meta, Store: f.store, Clock: clock})
	_, err := m.GetStatus(context.Background(), domain.Anonymous(), f.repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = m.GetStatus(context.Background(), tokenPrincipal(domain.ScopeRepoRead, "other"), f.repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = m.GetStatus(context.Background(), tokenPrincipal(domain.ScopeRepoRead, "tools"), f.repo)
	require.NoError(t, err)
}

func TestMirrorsCloseCancelsWorker(t *testing.T) {
	f := newMirrorFixture(t)
	entered, exited := make(chan struct{}), make(chan struct{})
	f.push.On("Push", mock.Anything, mock.Anything).Run(func(a mock.Arguments) { close(entered); <-a.Get(0).(context.Context).Done(); close(exited) }).Return(ports.MirrorPushResult{}, context.Canceled).Once()
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
	f.push.On("Push", mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, &domain.MirrorError{Class: domain.MirrorClassTransient}).Once()
	_, err := f.m.Sync(context.Background(), domain.Root(), f.repo, false)
	require.Error(t, err)
	f.advance(time.Minute)
	f.m.ScheduleNow(context.Background())
	f.push.AssertNumberOfCalls(t, "Push", 1)
	attempted := make(chan struct{})
	f.push.On("Push", mock.Anything, mock.Anything).Run(func(mock.Arguments) { close(attempted) }).Return(ports.MirrorPushResult{}, &domain.MirrorError{Class: domain.MirrorClassTransient}).Once()
	f.advance(4 * time.Minute)
	f.m.ScheduleNow(context.Background())
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("retry did not fire at backoff deadline")
	}
	// Cleanup joins the worker before mock verification.
}

func TestMirrorsRecordDoesNotWaitForTrigger(t *testing.T) {
	f := newPushFixture(t)
	n := name(t, "tools")
	trigger := mocks.NewMirrorTrigger(t)
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	trigger.On("OnPush", mock.Anything, n, mock.Anything).Run(func(mock.Arguments) { close(entered); <-release; close(exited) }).Return().Once()
	f.meta.On("Mutate", mock.Anything, n, mock.Anything).Return(domain.Repo{Name: n}, nil)
	f.git.On("DefaultBranch", mock.Anything, n).Return("main", true, nil)
	f.bus.On("Publish", mock.Anything, mock.Anything).Return()
	f.sched.On("Schedule", mock.Anything, n).Return()
	returned := make(chan error, 1)
	go func() { returned <- f.pushes.WithMirror(trigger).Record(context.Background(), domain.Root(), n, nil) }()
	<-entered
	select {
	case err := <-returned:
		require.NoError(t, err)
	case <-time.After(time.Second):
		close(release)
		<-exited
		t.Fatal("Record blocked on mirror trigger")
	}
	close(release)
	<-exited
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
	f.push.On("Push", mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, errors.New("local key PRIVATE-KEY-MARKER ssh://alice:secret-password@example.com/acme/tools")).Once()
	_, err = f.m.Sync(ctx, domain.Root(), f.repo, true)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "PRIVATE-KEY-MARKER")
	require.NotContains(t, err.Error(), "secret-password")
	require.NotContains(t, logs.String(), "PRIVATE-KEY-MARKER")
	require.NotContains(t, logs.String(), "secret-password")
}

func TestMirrorsFileURLTestOption(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			store, meta, clock := mocks.NewMirrorStore(t), mocks.NewRepoMetaStore(t), mocks.NewClock(t)
			repo := name(t, "tools")
			clock.On("Now").Return(now)
			meta.On("Get", mock.Anything, repo).Return(domain.Repo{Name: repo}, nil)
			global := domain.MirrorGlobalConfig{Enabled: true, URLTemplate: "file:///tmp/remotes/{repo}", Cron: "*/5 * * * *"}
			store.On("GetGlobal", mock.Anything).Return(global, nil).Maybe()
			store.On("GetRepoConfig", mock.Anything, repo).Return(domain.MirrorRepoConfig{}, domain.ErrNotFound).Maybe()
			store.On("GetStatus", mock.Anything, repo).Return(domain.MirrorStatus{Repo: repo}, nil).Maybe()
			store.On("PutRepoConfig", mock.Anything, mock.Anything).Return(nil).Maybe()
			store.On("PutStatus", mock.Anything, mock.Anything).Return(nil).Maybe()
			m := usecase.NewMirrors(usecase.MirrorsConfig{Store: store, Meta: meta, Clock: clock, AllowAnyURL: allowed})
			target, err := m.SetRepoMirror(context.Background(), domain.Root(), domain.MirrorRepoConfig{Repo: repo, URL: "file:///tmp/remotes/tools", Cron: "*/5 * * * *"})
			if allowed {
				require.NoError(t, err)
				require.Equal(t, "file:///tmp/remotes/tools", target.URL)
			} else {
				require.ErrorIs(t, err, domain.ErrInvalid)
			}
		})
	}
}

func TestMirrorsMergeNotifiesMirror(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	trigger := mocks.NewMirrorTrigger(t)
	notified := make(chan []domain.RefUpdate, 1)
	trigger.On("OnPush", mock.Anything, r.repo, mock.Anything).Run(func(a mock.Arguments) { notified <- a.Get(2).([]domain.RefUpdate) }).Return().Once()
	r.mrs.WithMirror(trigger)
	head := r.feature("f0")
	mr := r.open("f0")
	_, err := r.mrs.Merge(r.ctx, root, r.repo, mr.Number, ports.MergeParams{HeadID: head})
	require.NoError(t, err)
	r.mrs.Wait(r.ctx)
	select {
	case u := <-notified:
		require.Equal(t, "refs/heads/main", u[0].Ref)
	case <-time.After(time.Second):
		t.Fatal("merge did not notify mirrors")
	}
}

func TestMirrorsDefaultBranchChangeNotifiesMirror(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	n := name(t, "tools")
	repo := domain.Repo{Name: n, Visibility: domain.VisibilityPrivate}
	trigger := mocks.NewMirrorTrigger(t)
	trigger.On("OnPush", mock.Anything, n, mock.Anything).Return().Once()
	f.repos.WithMirror(trigger)
	f.meta.EXPECT().Get(ctx, n).Return(repo, nil)
	f.git.EXPECT().HasBranch(ctx, n, "dev").Return(true, nil)
	f.git.EXPECT().SetDefaultBranch(ctx, n, "dev").Return(nil)
	f.bus.EXPECT().Publish(ctx, mock.Anything).Return().Once()
	_, err := f.repos.Update(ctx, tokenPrincipal(domain.ScopeRepoAdmin, "tools"), n, ports.UpdateRepoParams{DefaultBranch: ptr("dev")})
	require.NoError(t, err)
}

func TestMirrorsExternalMutationMakesNextDueTickSync(t *testing.T) {
	f := newMirrorFixture(t)
	f.push.On("Push", mock.Anything, mock.Anything).Return(ports.MirrorPushResult{}, nil).Once()
	_, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.NoError(t, err)
	f.mu.Lock()
	f.at = f.at.Add(24 * time.Hour).Truncate(time.Hour).Add(time.Minute)
	f.mu.Unlock()
	f.m.ScheduleNow(context.Background())
	f.advance(4 * time.Minute)
	f.m.ScheduleNow(context.Background()) // due and clean: skipped
	f.reader.AssertNumberOfCalls(t, "ListRefs", 1)
	// A merge and a default-branch change both call OnPush (idempotent dirty mark).
	f.m.OnPush(context.Background(), f.repo, []domain.RefUpdate{{Ref: "refs/heads/main"}})
	f.m.OnPush(context.Background(), f.repo, []domain.RefUpdate{{Ref: "HEAD"}})
	read := make(chan struct{})
	f.reader.ExpectedCalls = nil
	f.reader.On("ListRefs", mock.Anything, f.repo).Run(func(mock.Arguments) { close(read) }).Return([]domain.Ref{{Name: "refs/heads/main", ID: mirrorOID(t)}}, nil).Once()
	f.advance(5 * time.Minute)
	f.m.ScheduleNow(context.Background())
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("dirty repository not synced on the next due tick")
	}
}

func setupRepoOverrideFixture(t *testing.T, global domain.MirrorGlobalConfig) *mirrorFixture {
	f := newMirrorFixture(t)
	f.store.ExpectedCalls = nil
	f.store.On("GetGlobal", mock.Anything).Return(global, nil)
	f.store.On("GetRepoConfig", mock.Anything, f.repo).Return(domain.MirrorRepoConfig{}, domain.ErrNotFound).Maybe()
	f.store.On("GetStatus", mock.Anything, f.repo).Return(domain.MirrorStatus{Repo: f.repo}, nil).Maybe()
	return f
}

func TestMirrorsOverrideWithoutCronRejectedWhenGlobalCronEmpty(t *testing.T) {
	f := setupRepoOverrideFixture(t, domain.MirrorGlobalConfig{})
	_, err := f.m.SetRepoMirror(context.Background(), domain.Root(), domain.MirrorRepoConfig{Repo: f.repo, URL: "git@example.com:acme/tools"})
	var fe *domain.InvalidFieldsError
	require.ErrorAs(t, err, &fe)
	require.Equal(t, "cron", fe.Fields[0].Field)
	// PutRepoConfig/PutStatus unexpected: strict mock fails if called.
}

func TestMirrorsGlobalChangeInvalidatingOverrideRejected(t *testing.T) {
	f := setupRepoOverrideFixture(t, domain.MirrorGlobalConfig{Enabled: true, URLTemplate: "git@example.com:acme/{repo}", Cron: "*/5 * * * *"})
	f.store.ExpectedCalls = nil
	f.store.On("GetGlobal", mock.Anything).Return(domain.MirrorGlobalConfig{Enabled: true, URLTemplate: "git@example.com:acme/{repo}", Cron: "*/5 * * * *"}, nil)
	f.store.On("GetRepoConfig", mock.Anything, f.repo).Return(domain.MirrorRepoConfig{Repo: f.repo, URL: "git@other.example:x/y"}, nil)
	// Disabling the global with an empty cron leaves the inheriting override with no cron.
	_, err := f.m.SetGlobalConfig(context.Background(), domain.Root(), ports.MirrorGlobalPatch{Enabled: ptr(false), Cron: ptr("")})
	var fe *domain.InvalidFieldsError
	require.ErrorAs(t, err, &fe)
}

func TestMirrorsGlobalChangeResetsInheritingURLOverride(t *testing.T) {
	f := newMirrorFixture(t)
	f.store.ExpectedCalls = nil
	f.store.On("GetGlobal", mock.Anything).Return(domain.MirrorGlobalConfig{Enabled: true, URLTemplate: "git@example.com:acme/{repo}", Cron: "*/5 * * * *"}, nil)
	f.store.On("GetRepoConfig", mock.Anything, f.repo).Return(domain.MirrorRepoConfig{Repo: f.repo, URL: "git@other.example:x/y"}, nil)
	f.store.On("PutGlobal", mock.Anything, mock.Anything).Return(nil).Once()
	f.store.On("GetStatus", mock.Anything, f.repo).Return(domain.MirrorStatus{Repo: f.repo, State: domain.MirrorPaused, Class: domain.MirrorClassDiverged}, nil)
	reset := make(chan domain.MirrorStatus, 1)
	f.store.On("PutStatus", mock.Anything, mock.Anything).Run(func(a mock.Arguments) { reset <- a.Get(1).(domain.MirrorStatus) }).Return(nil)
	_, err := f.m.SetGlobalConfig(context.Background(), domain.Root(), ports.MirrorGlobalPatch{Cron: ptr("*/10 * * * *")})
	require.NoError(t, err)
	require.Equal(t, domain.MirrorOK, (<-reset).State)
}

func TestMirrorsSetRepoExcludedConcurrent(t *testing.T) {
	f := newMirrorFixture(t)
	var mu sync.Mutex
	active, maxActive := 0, 0
	f.store.On("PutRepoConfig", mock.Anything, mock.Anything).Run(func(mock.Arguments) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
	}).Return(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(ex bool) {
			defer wg.Done()
			_, err := f.m.SetRepoExcluded(context.Background(), domain.Root(), f.repo, ex)
			require.NoError(t, err)
		}(i%2 == 0)
	}
	wg.Wait()
	require.Equal(t, 1, maxActive, "read-modify-write must be serialized")
}

func TestMirrorsCoalescedManualAppliesThrottle(t *testing.T) {
	f := newMirrorFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.push.On("Push", mock.Anything, mock.Anything).Run(func(mock.Arguments) { close(entered); <-release }).Return(ports.MirrorPushResult{}, nil).Once()
	done := make(chan error, 1)
	go func() { _, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true); done <- err }()
	<-entered
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, false)
	require.NoError(t, err)
	require.True(t, v.Queued)
	close(release)
	require.NoError(t, <-done)
	// The rerun is manual and not forced: the throttle (just attempted) stops it, so no second push.
	time.Sleep(200 * time.Millisecond)
	f.push.AssertNumberOfCalls(t, "Push", 1)
}

func TestMirrorsSlowPushTimestamps(t *testing.T) {
	f := newMirrorFixture(t)
	start := f.at
	f.push.On("Push", mock.Anything, mock.Anything).Run(func(mock.Arguments) { f.advance(10 * time.Minute) }).Return(ports.MirrorPushResult{}, &domain.MirrorError{Class: domain.MirrorClassTransient}).Once()
	v, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.Error(t, err)
	require.True(t, v.Status.LastAttemptAt.Equal(start))
	require.True(t, v.Status.NextRetryAt.Equal(start.Add(10*time.Minute+domain.MirrorBackoff(1))))
}
