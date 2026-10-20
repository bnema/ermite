// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"crypto/ed25519"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

// mirrorFixture is a Mirrors use case over in-memory state behind the mocks. Every mock
// answers from the fields below under mu, so tests change the world with the setters and
// never mutate a mock after construction.
type mirrorFixture struct {
	m      *usecase.Mirrors
	push   *mocks.MirrorPusher
	keys   *mocks.MirrorKeyStore
	sealer *mocks.SecretSealer
	repo   domain.RepoName

	mu         sync.Mutex
	at         time.Time
	global     domain.MirrorGlobalConfig
	repos      []domain.Repo
	cfgs       map[domain.RepoName]domain.MirrorRepoConfig
	status     map[domain.RepoName]domain.MirrorStatus
	snaps      map[domain.RepoName]domain.MirrorSnapshot
	refs       []domain.Ref
	refsErr    error
	branchErr  error
	listRefs   int
	statusPuts int
	events     []domain.Event
	onPutRepo  func() // runs inside PutRepoConfig, set before concurrency starts
	statusErr  map[domain.RepoName]error
}

func newMirrorFixture(t *testing.T) *mirrorFixture {
	return newMirrorFixtureWith(t, nil)
}

func newMirrorFixtureWith(t *testing.T, tweak func(*usecase.MirrorsConfig)) *mirrorFixture {
	t.Helper()
	f := &mirrorFixture{
		push: mocks.NewMirrorPusher(t), keys: mocks.NewMirrorKeyStore(t), sealer: mocks.NewSecretSealer(t),
		at: now, repo: name(t, "tools"),
		global: domain.MirrorGlobalConfig{Enabled: true, URLTemplate: "git@example.com:acme/{repo}", Cron: "*/5 * * * *"},
		cfgs:   map[domain.RepoName]domain.MirrorRepoConfig{}, status: map[domain.RepoName]domain.MirrorStatus{}, snaps: map[domain.RepoName]domain.MirrorSnapshot{}, statusErr: map[domain.RepoName]error{},
		refs: []domain.Ref{{Name: "refs/heads/main", ID: mirrorOID(t)}},
	}
	f.addRepo(f.repo)
	store, meta, git, reader, bus, clock := mocks.NewMirrorStore(t), mocks.NewRepoMetaStore(t), mocks.NewGitRepos(t), mocks.NewRepoReader(t), mocks.NewEventBus(t), mocks.NewClock(t)
	clock.EXPECT().Now().RunAndReturn(func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.at }).Maybe()
	meta.EXPECT().Get(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, n domain.RepoName) (domain.Repo, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, r := range f.repos {
			if r.Name == n {
				return r, nil
			}
		}
		return domain.Repo{}, domain.ErrNotFound
	}).Maybe()
	meta.EXPECT().List(mock.Anything).RunAndReturn(func(context.Context) ([]domain.Repo, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]domain.Repo(nil), f.repos...), nil
	}).Maybe()
	store.EXPECT().GetGlobal(mock.Anything).RunAndReturn(func(context.Context) (domain.MirrorGlobalConfig, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.global, nil
	}).Maybe()
	store.EXPECT().PutGlobal(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, g domain.MirrorGlobalConfig) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.global = g
		return nil
	}).Maybe()
	store.EXPECT().GetRepoConfig(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, n domain.RepoName) (domain.MirrorRepoConfig, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if c, ok := f.cfgs[n]; ok {
			return c, nil
		}
		return domain.MirrorRepoConfig{}, domain.ErrNotFound
	}).Maybe()
	store.EXPECT().PutRepoConfig(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, c domain.MirrorRepoConfig) error {
		if f.onPutRepo != nil {
			f.onPutRepo()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cfgs[c.Repo] = c
		return nil
	}).Maybe()
	store.EXPECT().DeleteRepoConfig(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, n domain.RepoName) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.cfgs, n)
		return nil
	}).Maybe()
	store.EXPECT().ListRepoConfigs(mock.Anything).RunAndReturn(func(context.Context) ([]domain.MirrorRepoConfig, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []domain.MirrorRepoConfig
		for _, c := range f.cfgs {
			out = append(out, c)
		}
		return out, nil
	}).Maybe()
	store.EXPECT().GetStatus(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, n domain.RepoName) (domain.MirrorStatus, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if s, ok := f.status[n]; ok {
			return s, nil
		}
		return domain.MirrorStatus{}, domain.ErrNotFound
	}).Maybe()
	store.EXPECT().PutStatus(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, s domain.MirrorStatus) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := f.statusErr[s.Repo]; err != nil {
			return err
		}
		f.status[s.Repo] = s
		f.statusPuts++
		return nil
	}).Maybe()
	store.EXPECT().ListStatus(mock.Anything).RunAndReturn(func(context.Context) ([]domain.MirrorStatus, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []domain.MirrorStatus
		for _, s := range f.status {
			out = append(out, s)
		}
		return out, nil
	}).Maybe()
	store.EXPECT().GetSnapshot(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, n domain.RepoName) (domain.MirrorSnapshot, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if s, ok := f.snaps[n]; ok {
			return s, nil
		}
		return domain.MirrorSnapshot{}, domain.ErrNotFound
	}).Maybe()
	store.EXPECT().PutSnapshot(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, n domain.RepoName, s domain.MirrorSnapshot) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.snaps[n] = s
		return nil
	}).Maybe()
	reader.EXPECT().ListRefs(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, domain.RepoName) ([]domain.Ref, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.listRefs++
		return f.refs, f.refsErr
	}).Maybe()
	git.EXPECT().DefaultBranch(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, domain.RepoName) (string, bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return "main", true, f.branchErr
	}).Maybe()
	bus.EXPECT().Publish(mock.Anything, mock.Anything).Run(func(_ context.Context, ev domain.Event) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = append(f.events, ev)
	}).Maybe()
	f.keys.EXPECT().GetMirrorKey(mock.Anything).Return([]byte("sealed"), now, nil).Maybe()
	f.sealer.EXPECT().Open([]byte("sealed"), []byte(domain.MirrorKeyPath)).Return(make([]byte, ed25519.SeedSize), nil).Maybe()
	cfg := usecase.MirrorsConfig{Store: store, Keys: f.keys, Sealer: f.sealer, Pusher: f.push, Meta: meta, Git: git, Reader: reader, Bus: bus, Clock: clock, ScheduleTick: time.Hour}
	if tweak != nil {
		tweak(&cfg)
	}
	f.m = usecase.NewMirrors(cfg)
	f.m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f.m.Close(ctx)
	})
	return f
}

// addRepo declares a public repository whose mirror starts OK.
func (f *mirrorFixture) addRepo(n domain.RepoName) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, domain.Repo{Name: n, Visibility: domain.VisibilityPublic, CreatedAt: now})
	f.status[n] = domain.MirrorStatus{Repo: n, State: domain.MirrorOK}
}

func (f *mirrorFixture) advance(d time.Duration) { f.mu.Lock(); f.at = f.at.Add(d); f.mu.Unlock() }
func (f *mirrorFixture) setStatus(s domain.MirrorStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[s.Repo] = s
}
func (f *mirrorFixture) setSnapshot(n domain.RepoName, s domain.MirrorSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[n] = s
}
func (f *mirrorFixture) setRepoConfig(c domain.MirrorRepoConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfgs[c.Repo] = c
}
func (f *mirrorFixture) setGlobal(g domain.MirrorGlobalConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.global = g
}
func (f *mirrorFixture) failStatusWrites(n domain.RepoName, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusErr[n] = err
}
func (f *mirrorFixture) snapshotOf(n domain.RepoName) (domain.MirrorSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.snaps[n]
	return s, ok
}
func (f *mirrorFixture) statusOf(n domain.RepoName) domain.MirrorStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[n]
}
func (f *mirrorFixture) eventList() []domain.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Event(nil), f.events...)
}
func (f *mirrorFixture) refReads() int { f.mu.Lock(); defer f.mu.Unlock(); return f.listRefs }
func (f *mirrorFixture) statusWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusPuts
}

// drain waits until no sync is queued or running: the deterministic barrier before asserting
// that something did not happen.
func (f *mirrorFixture) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.m.Wait(ctx)
	require.NoError(t, ctx.Err(), "syncs did not finish")
}

func mirrorOID(t *testing.T) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(oidA)
	require.NoError(t, err)
	return id
}
