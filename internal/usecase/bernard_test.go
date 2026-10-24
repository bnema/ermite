// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"errors"
	"io"
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

type bernardRig struct {
	t      *testing.T
	repo   domain.RepoName
	store  *mocks.DepsStore
	meta   *mocks.RepoMetaStore
	reader *mocks.RepoReader
	eco    *mocks.DepsEcosystem
	reg    *mocks.DepsRegistry
	bus    *mocks.EventBus
	clock  *mocks.Clock
	at     time.Time
	rec    domain.RepoDeps // the stored record
	cands  []domain.DepCandidate
	b      *usecase.Bernard
	regs   int

	changedMu sync.Mutex
	changed   []domain.DependencyUpdatesChanged // the deps.changed events published
}

func isDepsChanged(ev domain.Event) bool {
	_, ok := ev.(domain.DependencyUpdatesChanged)
	return ok
}

func (r *bernardRig) changedCount() int {
	r.changedMu.Lock()
	defer r.changedMu.Unlock()
	return len(r.changed)
}

func newBernardRig(t *testing.T, checkAt string) *bernardRig {
	t.Helper()
	r := &bernardRig{
		t: t, repo: name(t, "proj"),
		store: mocks.NewDepsStore(t), meta: mocks.NewRepoMetaStore(t), reader: mocks.NewRepoReader(t),
		eco: mocks.NewDepsEcosystem(t), reg: mocks.NewDepsRegistry(t), bus: mocks.NewEventBus(t), clock: mocks.NewClock(t),
		at: time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC),
	}
	r.rec = domain.RepoDeps{Repo: r.repo}
	r.bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(isDepsChanged)).Run(func(_ context.Context, ev domain.Event) {
		r.changedMu.Lock()
		r.changed = append(r.changed, ev.(domain.DependencyUpdatesChanged))
		r.changedMu.Unlock()
	}).Maybe()
	r.clock.EXPECT().Now().RunAndReturn(func() time.Time { return r.at }).Maybe()
	r.eco.EXPECT().Name().Return(domain.EcosystemGoMod).Maybe()
	r.store.EXPECT().Mutate(mock.Anything, r.repo, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.RepoName, fn func(*domain.RepoDeps) (bool, error)) (domain.RepoDeps, error) {
			cp := r.rec
			if _, err := fn(&cp); err != nil {
				return domain.RepoDeps{}, err
			}
			r.rec = cp
			return cp, nil
		}).Maybe()
	r.store.EXPECT().Get(mock.Anything, r.repo).RunAndReturn(func(context.Context, domain.RepoName) (domain.RepoDeps, error) { return r.rec, nil }).Maybe()
	b, err := usecase.NewBernard(usecase.BernardConfig{
		Store: r.store, Meta: r.meta, Reader: r.reader, Ecosystems: []ports.DepsEcosystem{r.eco},
		Registry: func() ports.DepsRegistry { r.regs++; return r.reg }, Bus: r.bus, Clock: r.clock,
		CheckAt: checkAt, Cooldown: 72 * time.Hour, Snooze: 7 * 24 * time.Hour, Tick: time.Hour,
	})
	require.NoError(t, err)
	r.b = b
	return r
}

// repoReads sets the repository reads: one go.mod manifest.
func (r *bernardRig) repoReads() {
	tp, err := domain.ParseTreePath("go.mod")
	require.NoError(r.t, err)
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil).Maybe()
	r.reader.EXPECT().ResolveRevision(mock.Anything, r.repo, domain.Revision{}).Return(domain.ResolvedRevision{}, nil)
	r.reader.EXPECT().ListPaths(mock.Anything, r.repo, mock.Anything, domain.MaxPathEntries, domain.MaxPathBytes).Return(domain.PathList{Paths: []string{"go.mod"}}, nil)
	r.reader.EXPECT().OpenBlob(mock.Anything, r.repo, mock.Anything, tp).Return(domain.BlobInfo{Size: 4}, io.NopCloser(bytes.NewReader([]byte("mod\n"))), nil)
	r.eco.EXPECT().Manifests(mock.Anything).Return([]domain.TreePath{tp})
	r.eco.EXPECT().Detect(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(context.Context, map[domain.TreePath][]byte, ports.DepsRegistry) ([]domain.DepCandidate, error) {
			return r.cands, nil
		})
}

func goCand(modName string, versions ...domain.DepVersion) domain.DepCandidate {
	return domain.DepCandidate{Ecosystem: domain.EcosystemGoMod, Name: modName, Current: "v0.20.0", Versions: versions}
}

func (r *bernardRig) old(v string) domain.DepVersion {
	return domain.DepVersion{Version: v, Published: r.at.Add(-10 * 24 * time.Hour)}
}

func depRef(modName, v string) domain.DepRef {
	return domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: modName, Version: v}
}

func TestBernardCheckStoresPendingAndMarksNew(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.cands = []domain.DepCandidate{goCand("b/mod", r.old("v0.21.0")), goCand("a/mod", r.old("v0.20.1"))}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, mock.Anything, mock.Anything).Return(false, nil)
	// a/mod was already pending, b/mod is new.
	r.rec = domain.RepoDeps{Repo: r.repo, State: domain.DepsFailed, MR: 3, Updates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1"), Current: "v0.20.0", Kind: domain.DepPatch}}}
	r.bus.EXPECT().Publish(mock.Anything, domain.DependencyUpdatesFound{Repo: r.repo, Count: 1, At: r.at}).Once()

	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	require.Len(t, got.Updates, 2)
	assert.Equal(t, "a/mod", got.Updates[0].Name)
	assert.False(t, got.Updates[0].New)
	assert.Equal(t, "b/mod", got.Updates[1].Name)
	assert.True(t, got.Updates[1].New)
	assert.Equal(t, domain.DepMinor, got.Updates[1].Kind)
	assert.Equal(t, r.at, got.CheckedAt)
	assert.Equal(t, domain.DepsFailed, got.State, "the check touches only updates and checked-at")
	assert.EqualValues(t, 3, got.MR)
}

func TestBernardCheckNoNewDoesNotPublish(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.20.1"))}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, nil)
	r.rec.Updates = []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1")}}
	_, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err) // the bus mock fails the test on an unexpected Publish
}

func TestBernardCheckSkipsArchived(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo, Archived: true}, nil)
	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Empty(t, got.Updates)
}

func TestBernardCheckEmptyRepository(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	r.reader.EXPECT().ResolveRevision(mock.Anything, r.repo, domain.Revision{}).Return(domain.ResolvedRevision{}, domain.ErrNotFound)
	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Empty(t, got.Updates)
	assert.Equal(t, r.at, got.CheckedAt)
}

func TestBernardOSVErrorAndMaliciousSkip(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.cands = []domain.DepCandidate{goCand("err/mod", r.old("v0.21.0")), goCand("mal/mod", r.old("v0.21.0")), goCand("ok/mod", r.old("v0.21.0"))}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "err/mod", "v0.21.0").Return(false, errors.New("osv down"))
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "mal/mod", "v0.21.0").Return(true, nil)
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "ok/mod", "v0.21.0").Return(false, nil)
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Once()
	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	require.Len(t, got.Updates, 1)
	assert.Equal(t, "ok/mod", got.Updates[0].Name)
}

func TestBernardCooldownAndIgnored(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	young := domain.DepVersion{Version: "v0.22.0", Published: r.at.Add(-time.Hour)}
	r.cands = []domain.DepCandidate{
		goCand("young/mod", young),                 // only a young version: nothing
		goCand("mix/mod", r.old("v0.21.0"), young), // the old one is proposed
		goCand("ign/mod", r.old("v0.21.0")),        // ignored: nothing
	}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, depRef("ign/mod", "v0.21.0")).Return(true, nil)
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "mix/mod", "v0.21.0").Return(false, nil)
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Once()
	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	require.Len(t, got.Updates, 1)
	assert.Equal(t, depRef("mix/mod", "v0.21.0"), got.Updates[0].DepRef)
}

func TestBernardCallbackErrorFailsCheck(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.21.0"))}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, errors.New("disk"))
	_, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.Error(t, err)
	assert.Empty(t, r.rec.Updates)
}

func TestBernardScheduleRunsOncePerDueMinute(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: r.repo}, {Name: name(t, "old"), Archived: true}}, nil).Once()
	r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.21.0"))}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, nil)
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Once()

	r.at = time.Date(2026, 10, 24, 5, 59, 0, 0, time.UTC)
	r.b.ScheduleNow(context.Background()) // not due
	assert.Zero(t, r.regs)
	r.at = time.Date(2026, 10, 24, 6, 0, 10, 0, time.UTC)
	r.b.ScheduleNow(context.Background())
	r.at = r.at.Add(20 * time.Second) // same minute
	r.b.ScheduleNow(context.Background())
	assert.Equal(t, 1, r.regs)
	require.Len(t, r.rec.Updates, 1)
}

func TestBernardOwnerOnly(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	ctx := context.Background()
	for _, p := range []domain.Principal{domain.Anonymous()} {
		_, err := r.b.List(ctx, p, domain.RepoName{})
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = r.b.Check(ctx, p, r.repo)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = r.b.Open(ctx, p, r.repo, nil)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = r.b.Ignore(ctx, p, r.repo, nil)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = r.b.Snooze(ctx, p, r.repo)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}
}

func TestBernardOpenUnavailable(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	_, err := r.b.Open(context.Background(), domain.Owner(), r.repo, nil)
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
}

func TestBernardList(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	other := name(t, "other")
	withUp := domain.RepoDeps{Repo: r.repo, Updates: []domain.DepUpdate{{DepRef: depRef("a", "v1.0.1")}}}
	r.store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{withUp, {Repo: other}, {Repo: name(t, "z"), Updates: withUp.Updates}}, nil)
	all, err := r.b.List(context.Background(), domain.Owner(), domain.RepoName{})
	require.NoError(t, err)
	assert.Len(t, all, 2)
	one, err := r.b.List(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Len(t, one, 1)
}

func TestBernardIgnore(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	r.rec.Updates = []domain.DepUpdate{{DepRef: depRef("a", "v1.0.1")}, {DepRef: depRef("b", "v2.0.1")}}
	ctx := context.Background()

	_, err := r.b.Ignore(ctx, domain.Owner(), r.repo, []domain.DepRef{depRef("a", "v1.0.0")})
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed, "expected mismatch")

	r.rec.State = domain.DepsApplying
	_, err = r.b.Ignore(ctx, domain.Owner(), r.repo, r.rec.Refs())
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed, "applying")

	r.rec.State = domain.DepsIdle
	r.store.EXPECT().Ignore(mock.Anything, r.repo, []domain.DepRef{depRef("b", "v2.0.1"), depRef("a", "v1.0.1")}).Return(nil).Once()
	got, err := r.b.Ignore(ctx, domain.Owner(), r.repo, []domain.DepRef{depRef("b", "v2.0.1"), depRef("a", "v1.0.1")})
	require.NoError(t, err)
	assert.Empty(t, got.Updates)
}

func TestBernardSnooze(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	r.rec.State = domain.DepsApplying // works in any state
	got, err := r.b.Snooze(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Equal(t, r.at.Add(7*24*time.Hour), got.SnoozedUntil)
}

func TestNewBernardRejectsBadConfig(t *testing.T) {
	_, err := usecase.NewBernard(usecase.BernardConfig{})
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

// TestBernardIgnoreWaitsForTheRepositoryLock: a transition made while Ignore
// waits for the lock (an Open, say) is seen by Ignore, which then refuses.
func TestBernardIgnoreWaitsForTheRepositoryLock(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil).Maybe()
	ref := depRef("a/mod", "v0.20.1")
	r.rec.Updates = []domain.DepUpdate{{DepRef: ref}}

	holding, release := make(chan struct{}), make(chan struct{})
	locked := make(chan struct{})
	go func() {
		r.b.UnderRepoLock(r.repo, func() {
			close(holding)
			<-release
			r.rec.State, r.rec.JobID = domain.DepsApplying, "deps-x" // Open won
		})
		close(locked)
	}()
	<-holding
	errc := make(chan error, 1)
	go func() {
		_, err := r.b.Ignore(context.Background(), domain.Owner(), r.repo, []domain.DepRef{ref})
		errc <- err
	}()
	close(release)
	<-locked
	require.ErrorIs(t, <-errc, domain.ErrPreconditionFailed) // no Store.Ignore call: the mock would fail
}

// TestBernardCheckWaitsForTheRepositoryLock: the check installs its result
// only once the holder of the lock is done.
func TestBernardCheckWaitsForTheRepositoryLock(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.20.1"))}
	detected := make(chan struct{}, 2)
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).RunAndReturn(
		func(context.Context, domain.RepoName, domain.DepRef) (bool, error) {
			detected <- struct{}{}
			return false, nil
		})
	r.reg.EXPECT().Malicious(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, nil)
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Maybe()

	holding, release, locked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		r.b.UnderRepoLock(r.repo, func() {
			close(holding)
			<-release
			r.rec.MR = 11 // the holder's transition
		})
		close(locked)
	}()
	<-holding
	done := make(chan domain.RepoDeps, 1)
	go func() {
		got, _ := r.b.Check(context.Background(), domain.Owner(), r.repo)
		done <- got
	}()
	<-detected // the detection ran; the installation is behind the lock
	select {
	case <-detected:
		t.Fatal("the check went past the lock")
	case <-done:
		t.Fatal("the check ended under the lock")
	default:
	}
	close(release)
	<-locked
	got := <-done
	assert.EqualValues(t, 11, got.MR, "installed on top of the holder's transition")
	assert.Len(t, got.Updates, 1)
}

func TestBernardSnoozeAndIgnorePublishTheChange(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil).Maybe()
	ref := depRef("a/mod", "v0.20.1")
	r.rec.Updates = []domain.DepUpdate{{DepRef: ref}}
	r.store.EXPECT().Ignore(mock.Anything, r.repo, []domain.DepRef{ref}).Return(nil)

	_, err := r.b.Snooze(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Equal(t, 1, r.changedCount())
	_, err = r.b.Ignore(context.Background(), domain.Owner(), r.repo, []domain.DepRef{ref})
	require.NoError(t, err)
	assert.Equal(t, 2, r.changedCount())
	assert.Equal(t, r.repo, r.changed[1].Repo)
}
