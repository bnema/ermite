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
	"golang.org/x/mod/semver"

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
	skips  []ports.DepsSkip
	b      *usecase.Bernard
	regs   int
	// goDirs answers GoDirective by "module@version"; a missing key has no go
	// line. goDirErr fails those lookups.
	goDirs   map[string]string
	goDirErr map[string]error
	goCalls  []string // the "module@version" keys looked up, in order
	onGoDir  func()   // runs on each lookup, when set

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
	r.reg.EXPECT().GoDirective(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, module, version string) (string, error) {
		key := module + "@" + version
		r.goCalls = append(r.goCalls, key)
		if r.onGoDir != nil {
			r.onGoDir()
		}
		return r.goDirs[key], r.goDirErr[key]
	}).Maybe()
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
	r.eco.EXPECT().Manifests(mock.Anything).Return([]ports.DepsInput{{Path: tp}})
	r.eco.EXPECT().Detect(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(context.Context, map[domain.TreePath][]byte, ports.DepsRegistry) ([]domain.DepCandidate, []ports.DepsSkip, error) {
			return r.cands, r.skips, nil
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
	r.rec = domain.RepoDeps{Repo: r.repo, Batches: []domain.DepBatch{{State: domain.DepsFailed, MR: 3, Updates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1"), Current: "v0.20.0", Kind: domain.DepPatch}}}}}
	r.bus.EXPECT().Publish(mock.Anything, domain.DependencyUpdatesFound{Repo: r.repo, Count: 1, At: r.at}).Once()

	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	require.Len(t, got.Base().Updates, 2)
	assert.Equal(t, "a/mod", got.Base().Updates[0].Name)
	assert.False(t, got.Base().Updates[0].New)
	assert.Equal(t, "b/mod", got.Base().Updates[1].Name)
	assert.True(t, got.Base().Updates[1].New)
	assert.Equal(t, domain.DepMinor, got.Base().Updates[1].Kind)
	assert.Equal(t, r.at, got.CheckedAt)
	assert.Equal(t, domain.DepsFailed, got.Base().State, "the check touches only updates and checked-at")
	assert.EqualValues(t, 3, got.Base().MR)
}

func TestBernardCheckNoNewDoesNotPublish(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.20.1"))}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, nil)
	r.rec.Base().Updates = []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1")}}
	_, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err) // the bus mock fails the test on an unexpected Publish
}

// A major batch kept only for its merge request (no proposed updates) is not a
// change on every later check; losing its updates or dropping a batch is.
func TestBernardCheckChangedOnlyWhenBatchesMove(t *testing.T) {
	major := domain.DepBatch{Key: "gomod//x@v2", MR: 4}
	for _, tc := range []struct {
		name    string
		batches []domain.DepBatch
		want    int
	}{
		{"kept MR batch without updates", []domain.DepBatch{{Updates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1"), Current: "v0.20.0"}}}, major}, 0},
		{"kept MR batch loses its updates", []domain.DepBatch{{Updates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1"), Current: "v0.20.0"}}}, {Key: major.Key, MR: 4, Updates: []domain.DepUpdate{{DepRef: depRef("x/v2", "v2.0.0")}}}}, 1},
		{"same version, other current", []domain.DepBatch{{Updates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1"), Current: "v0.19.0"}}}}, 1},
		{"MR snapshot now outdated", []domain.DepBatch{{MR: 2, Updates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1"), Current: "v0.19.0"}}, MRVersions: []domain.DepRef{depRef("a/mod", "v0.20.1")}, MRUpdates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1"), Current: "v0.19.0"}}}}, 1},
		{"unchanged", []domain.DepBatch{{Updates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1"), Current: "v0.20.0"}}}}, 0},
		{"idle batch dropped", []domain.DepBatch{{Updates: []domain.DepUpdate{{DepRef: depRef("a/mod", "v0.20.1"), Current: "v0.20.0"}}}, {Key: "gomod//y@v3", Updates: []domain.DepUpdate{{DepRef: depRef("y/v3", "v3.0.0")}}}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBernardRig(t, "0 6 * * *")
			r.repoReads()
			// The check proposes a/mod v0.20.1 from v0.20.0 (goCand's current).
			r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.20.1"))}
			r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil).Maybe()
			r.reg.EXPECT().Malicious(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, nil).Maybe()
			r.bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(e domain.Event) bool { _, ok := e.(domain.DependencyUpdatesFound); return ok })).Maybe()
			r.rec = domain.RepoDeps{Repo: r.repo, Batches: tc.batches}
			_, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
			require.NoError(t, err)
			assert.Equal(t, tc.want, r.changedCount())
		})
	}
}

func TestBernardCheckSkipsArchived(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo, Archived: true}, nil)
	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Empty(t, got.Base().Updates)
}

func TestBernardCheckEmptyRepository(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	r.reader.EXPECT().ResolveRevision(mock.Anything, r.repo, domain.Revision{}).Return(domain.ResolvedRevision{}, domain.ErrNotFound)
	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Empty(t, got.Base().Updates)
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
	require.Len(t, got.Base().Updates, 1)
	assert.Equal(t, "ok/mod", got.Base().Updates[0].Name)
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
	require.Len(t, got.Base().Updates, 1)
	assert.Equal(t, depRef("mix/mod", "v0.21.0"), got.Base().Updates[0].DepRef)
}

func TestBernardProposesOnlyVersionsFittingTheGoDirective(t *testing.T) {
	for _, tc := range []struct {
		name   string
		repoGo string
		dirs   map[string]string // version → go directive of its go.mod
		errs   map[string]error
		want   string // empty: nothing proposed
		calls  int    // expected go.mod lookups, when not 0
	}{
		{name: "newest fits", repoGo: "1.25.3", dirs: map[string]string{"v0.23.0": "1.25.3"}, want: "v0.23.0", calls: 1},
		{name: "newest too new, older fits", repoGo: "1.25.3", dirs: map[string]string{"v0.23.0": "1.26.0", "v0.22.0": "1.25.0"}, want: "v0.22.0"},
		{name: "release candidate is newer", repoGo: "1.25.3", dirs: map[string]string{"v0.23.0": "1.26rc1", "v0.22.0": "1.24"}, want: "v0.22.0"},
		{name: "none fits", repoGo: "1.25.3", dirs: map[string]string{"v0.23.0": "1.26.0", "v0.22.0": "1.26.0", "v0.21.0": "1.25.4"}},
		{name: "missing go line fits", repoGo: "1.25.3", dirs: map[string]string{"v0.23.0": ""}, want: "v0.23.0"},
		{name: "two-part repo fits its .0", repoGo: "1.25", dirs: map[string]string{"v0.23.0": "1.25.1", "v0.22.0": "1.25.0"}, want: "v0.22.0"},
		{name: "unreadable go.mod skipped", repoGo: "1.25.3", errs: map[string]error{"v0.23.0": errors.New("proxy down")}, want: "v0.22.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBernardRig(t, "0 6 * * *")
			r.repoReads()
			c := goCand("x/mod", r.old("v0.21.0"), r.old("v0.23.0"), r.old("v0.22.0"))
			c.GoDirective = tc.repoGo
			r.cands = []domain.DepCandidate{c}
			r.goDirs, r.goDirErr = map[string]string{}, map[string]error{}
			for v, d := range tc.dirs {
				r.goDirs["x/mod@"+v] = d
			}
			for v, err := range tc.errs {
				r.goDirErr["x/mod@"+v] = err
			}
			r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
			if tc.want != "" {
				r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "x/mod", tc.want).Return(false, nil).Once()
				r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Once()
			}
			got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
			require.NoError(t, err)
			if tc.calls != 0 {
				assert.Len(t, r.goCalls, tc.calls)
			}
			if tc.want == "" {
				assert.Empty(t, got.Base().Updates)
				return
			}
			require.Len(t, got.Base().Updates, 1)
			assert.Equal(t, depRef("x/mod", tc.want), got.Base().Updates[0].DepRef)
		})
	}
}

func TestBernardCancelledDuringGoModLookupProposesNothing(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	c := goCand("x/mod", r.old("v0.22.0"), r.old("v0.23.0"))
	c.GoDirective = "1.25.3"
	r.cands = []domain.DepCandidate{c}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.onGoDir = cancel
	r.goDirErr = map[string]error{"x/mod@v0.23.0": context.Canceled}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	_, err := r.b.Check(ctx, domain.Owner(), r.repo)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"x/mod@v0.23.0"}, r.goCalls)
}

func TestBernardCallbackErrorFailsCheck(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.21.0"))}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, errors.New("disk"))
	_, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.Error(t, err)
	assert.Empty(t, r.rec.Base().Updates)
}

func TestBernardScheduleRunsOncePerDueMinute(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: r.repo}, {Name: name(t, "old"), Archived: true}}, nil).Once()
	r.store.EXPECT().List(mock.Anything).Return(nil, nil).Once() // the due pass settles the applying records first
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
	require.Len(t, r.rec.Base().Updates, 1)
}

func TestBernardOwnerOnly(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	ctx := context.Background()
	for _, p := range []domain.Principal{domain.Anonymous()} {
		_, err := r.b.List(ctx, p, domain.RepoName{})
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = r.b.Check(ctx, p, r.repo)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = r.b.Open(ctx, p, r.repo, domain.DepBatchKey(""), nil)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = r.b.Ignore(ctx, p, r.repo, domain.DepBatchKey(""), nil)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = r.b.Snooze(ctx, p, r.repo)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}
}

func TestBernardOpenUnavailable(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	_, err := r.b.Open(context.Background(), domain.Owner(), r.repo, domain.DepBatchKey(""), nil)
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
}

func TestBernardList(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	other := name(t, "other")
	withUp := domain.RepoDeps{Repo: r.repo, Batches: []domain.DepBatch{{Updates: []domain.DepUpdate{{DepRef: depRef("a", "v1.0.1")}}}}}
	// Batches without updates still show while a merge request or a run holds them.
	withMR := domain.RepoDeps{Repo: name(t, "mr"), Batches: []domain.DepBatch{{}, {Key: "gomod//x@v2", MR: 4}}}
	applying := domain.RepoDeps{Repo: name(t, "run"), Batches: []domain.DepBatch{{State: domain.DepsApplying, RunID: 1}}}
	r.store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{withUp, {Repo: other}, {Repo: name(t, "z"), Batches: withUp.Batches}, withMR, applying}, nil)
	all, err := r.b.List(context.Background(), domain.Owner(), domain.RepoName{})
	require.NoError(t, err)
	assert.Len(t, all, 4)
	one, err := r.b.List(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Len(t, one, 1)
}

func TestBernardIgnore(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	r.rec.Base().Updates = []domain.DepUpdate{{DepRef: depRef("a", "v1.0.1")}, {DepRef: depRef("b", "v2.0.1")}}
	ctx := context.Background()

	_, err := r.b.Ignore(ctx, domain.Owner(), r.repo, domain.DepBatchKey(""), []domain.DepRef{depRef("a", "v1.0.0")})
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed, "expected mismatch")

	r.rec.Base().State = domain.DepsApplying
	_, err = r.b.Ignore(ctx, domain.Owner(), r.repo, domain.DepBatchKey(""), r.rec.Base().Refs())
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed, "applying")

	r.rec.Base().State = domain.DepsIdle
	r.store.EXPECT().Ignore(mock.Anything, r.repo, []domain.DepRef{depRef("b", "v2.0.1"), depRef("a", "v1.0.1")}).Return(nil).Once()
	got, err := r.b.Ignore(ctx, domain.Owner(), r.repo, domain.DepBatchKey(""), []domain.DepRef{depRef("b", "v2.0.1"), depRef("a", "v1.0.1")})
	require.NoError(t, err)
	assert.Empty(t, got.Base().Updates)
}

func TestBernardSnooze(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	r.rec.Base().State = domain.DepsApplying // works in any state
	got, err := r.b.Snooze(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Equal(t, r.at.Add(7*24*time.Hour), got.SnoozedUntil)
}

func TestNewBernardRejectsBadConfig(t *testing.T) {
	_, err := usecase.NewBernard(usecase.BernardConfig{})
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

func TestNewBernardRejectsRunsWithoutARunnerLabel(t *testing.T) {
	t.Parallel()
	for _, label := range []string{"", "  "} {
		r := newBernardRig(t, "0 6 * * *")
		_, err := usecase.NewBernard(usecase.BernardConfig{
			Store: r.store, Meta: r.meta, Reader: r.reader, Registry: func() ports.DepsRegistry { return r.reg },
			Bus: r.bus, Clock: r.clock, CheckAt: "0 6 * * *", Runs: mocks.NewDepsRunner(t), RunnerLabel: label,
		})
		assert.ErrorIs(t, err, domain.ErrInvalid, "label %q", label)
	}
}

func TestNewBernardRejectsRunsWithoutRunners(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	_, err := usecase.NewBernard(usecase.BernardConfig{
		Store: r.store, Meta: r.meta, Reader: r.reader, Registry: func() ports.DepsRegistry { return r.reg },
		Bus: r.bus, Clock: r.clock, CheckAt: "0 6 * * *", Runs: mocks.NewDepsRunner(t), RunnerLabel: "ubuntu-latest",
	})
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

// TestBernardIgnoreWaitsForTheRepositoryLock: a transition made while Ignore
// waits for the lock (an Open, say) is seen by Ignore, which then refuses.
func TestBernardIgnoreWaitsForTheRepositoryLock(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil).Maybe()
	ref := depRef("a/mod", "v0.20.1")
	r.rec.Base().Updates = []domain.DepUpdate{{DepRef: ref}}

	holding, release := make(chan struct{}), make(chan struct{})
	locked := make(chan struct{})
	go func() {
		r.b.UnderRepoLock(r.repo, func() {
			close(holding)
			<-release
			r.rec.Base().State, r.rec.Base().RunID = domain.DepsApplying, 5 // Open won
		})
		close(locked)
	}()
	<-holding
	errc := make(chan error, 1)
	go func() {
		_, err := r.b.Ignore(context.Background(), domain.Owner(), r.repo, domain.DepBatchKey(""), []domain.DepRef{ref})
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
			r.rec.Base().MR = 11 // the holder's transition
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
	assert.EqualValues(t, 11, got.Base().MR, "installed on top of the holder's transition")
	assert.Len(t, got.Base().Updates, 1)
}

func TestBernardSnoozeAndIgnorePublishTheChange(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil).Maybe()
	ref := depRef("a/mod", "v0.20.1")
	r.rec.Base().Updates = []domain.DepUpdate{{DepRef: ref}}
	r.store.EXPECT().Ignore(mock.Anything, r.repo, []domain.DepRef{ref}).Return(nil)

	_, err := r.b.Snooze(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Equal(t, 1, r.changedCount())
	_, err = r.b.Ignore(context.Background(), domain.Owner(), r.repo, domain.DepBatchKey(""), []domain.DepRef{ref})
	require.NoError(t, err)
	assert.Equal(t, 2, r.changedCount())
	assert.Equal(t, r.repo, r.changed[1].Repo)
}

func TestBernardIgnoreMajorBatchByKey(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	up := domain.DepUpdate{DepRef: depRef("pkg", "v2.0.0")}
	key := domain.MajorBatchKey(up)
	base := domain.DepBatch{State: domain.DepsApplying, RunID: 99}
	r.rec.Batches = []domain.DepBatch{base, {Key: key, Updates: []domain.DepUpdate{up}}}
	_, err := r.b.Ignore(t.Context(), domain.Owner(), r.repo, "missing", nil)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = r.b.Ignore(t.Context(), domain.Owner(), r.repo, key, nil)
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	r.store.EXPECT().Ignore(mock.Anything, r.repo, []domain.DepRef{up.DepRef}).Return(nil).Once()
	got, err := r.b.Ignore(t.Context(), domain.Owner(), r.repo, key, []domain.DepRef{up.DepRef})
	require.NoError(t, err)
	require.Equal(t, base, got.Batches[0])
	require.Empty(t, got.Batches[1].Updates)
}

func TestBernardCheckMergesMajorBatches(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	c := goCand("pkg", r.old("v0.9.1"), r.old("v1.2.0"))
	c.Current = "v0.9.0"
	r.cands = []domain.DepCandidate{c}
	major := domain.DepUpdate{DepRef: depRef("pkg", "v1.2.0")}
	key := domain.MajorBatchKey(major)
	kept := domain.DepBatch{Key: key, MR: 4, State: domain.DepsApplying, RunID: 10, Error: "keep", RunUpdates: []domain.DepUpdate{major}, MRVersions: []domain.DepRef{major.DepRef}}
	r.rec.Batches = []domain.DepBatch{{}, kept, {Key: "gone"}, {Key: "stale-mr", MR: 9}, {Key: "stale-run", State: domain.DepsApplying, RunID: 11}}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, nil)
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Maybe()
	got, err := r.b.Check(t.Context(), domain.Owner(), r.repo)
	require.NoError(t, err)
	require.Len(t, got.Base().Updates, 1)
	assert.Equal(t, "v0.9.1", got.Base().Updates[0].Version)
	batch, _, ok := got.Batch(key)
	require.True(t, ok)
	updates := batch.Updates
	batch.Updates = nil
	require.Equal(t, kept, batch)
	require.Len(t, updates, 1)
	assert.Equal(t, "v1.2.0", updates[0].Version)
	_, _, ok = got.Batch("gone")
	require.False(t, ok)
	require.Len(t, got.Batches, 4)
	for _, k := range []domain.DepBatchKey{"stale-mr", "stale-run"} {
		b, _, ok := got.Batch(k)
		require.True(t, ok)
		require.Empty(t, b.Updates)
	}
}

func TestBernardIgnoredMajorLeavesMinor(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	c := goCand("pkg", r.old("v0.21.0"), r.old("v1.2.0"))
	r.cands = []domain.DepCandidate{c}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, depRef("pkg", "v1.2.0")).Return(true, nil)
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, nil)
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Maybe()
	got, err := r.b.Check(t.Context(), domain.Owner(), r.repo)
	require.NoError(t, err)
	require.Len(t, got.Batches, 1)
	require.Len(t, got.Base().Updates, 1)
}

// A dependency Detect skipped (registry failure, spent npm budget) keeps its
// stored updates: in the base batch and in its major batch without a merge
// request, not new, and nothing is announced. A stored update from another
// current version is stale and dropped.
func TestBernardCheckKeepsSkippedDependencies(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.cands = []domain.DepCandidate{goCand("a/mod", r.old("v0.20.1"))}
	r.skips = []ports.DepsSkip{{Name: "x/mod", Current: "v1.0.0"}, {Name: "stale/mod", Current: "v2.0.0"}}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, nil)
	known := domain.DepUpdate{DepRef: depRef("a/mod", "v0.20.1"), Current: "v0.20.0", Kind: domain.DepPatch}
	minor := domain.DepUpdate{DepRef: depRef("x/mod", "v1.2.0"), Current: "v1.0.0", Kind: domain.DepMinor, New: true}
	major := domain.DepUpdate{DepRef: depRef("x/mod", "v2.0.0"), Current: "v1.0.0", Kind: domain.DepMajor}
	stale := domain.DepUpdate{DepRef: depRef("stale/mod", "v1.1.0"), Current: "v1.0.0", Kind: domain.DepMinor}
	r.rec = domain.RepoDeps{Repo: r.repo, Batches: []domain.DepBatch{
		{Updates: []domain.DepUpdate{known, minor, stale}},
		{Key: domain.MajorBatchKey(major), Updates: []domain.DepUpdate{major}},
	}}
	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo) // the bus mock refuses DependencyUpdatesFound
	require.NoError(t, err)
	notNew := minor
	notNew.New = false
	assert.Equal(t, []domain.DepUpdate{known, notNew}, got.Base().Updates)
	require.Len(t, got.Batches, 2, "the major batch without a merge request stays")
	assert.Equal(t, []domain.DepUpdate{major}, got.Batches[1].Updates)
	assert.Equal(t, 1, r.changedCount(), "only the stale update left")
}

// laneCand is a Go candidate of x/mod from v1.0.0 offering versions.
func (r *bernardRig) laneCand(versions ...string) domain.DepCandidate {
	c := domain.DepCandidate{Ecosystem: domain.EcosystemGoMod, Name: "x/mod", Current: "v1.0.0"}
	for _, v := range versions {
		c.Versions = append(c.Versions, r.old(v))
	}
	return c
}

func xUpdate(version string, kind domain.DepKind) domain.DepUpdate {
	return domain.DepUpdate{DepRef: depRef("x/mod", version), Current: "v1.0.0", Kind: kind}
}

func (r *bernardRig) skipRecord(ups ...domain.DepUpdate) {
	r.rec = domain.RepoDeps{Repo: r.repo}
	var base []domain.DepUpdate
	for _, u := range ups {
		if semver.Major(u.Version) != semver.Major(u.Current) {
			r.rec.Batches = append(r.rec.Batches, domain.DepBatch{Key: domain.MajorBatchKey(u), Updates: []domain.DepUpdate{u}})
		} else {
			base = append(base, u)
		}
	}
	r.rec.Batches = append([]domain.DepBatch{{Updates: base}}, r.rec.Batches...)
}

// A malicious result in one lane is not undone by an advisory failure in the
// other: skips are per lane, so the stored malicious version is not carried.
func TestBernardCheckSkipsAreLaneScoped(t *testing.T) {
	boom := errors.New("osv down")
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.cands = []domain.DepCandidate{r.laneCand("v1.3.0", "v2.0.0")}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "x/mod", "v1.3.0").Return(true, nil)
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "x/mod", "v2.0.0").Return(false, boom)
	major := xUpdate("v2.0.0", domain.DepMajor)
	r.skipRecord(xUpdate("v1.3.0", domain.DepMinor), major)
	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Empty(t, got.Base().Updates, "the malicious v1.3.0 is not carried")
	require.Len(t, got.Batches, 2)
	assert.Equal(t, []domain.DepUpdate{major}, got.Batches[1].Updates, "the major lane whose advisory failed keeps its update")
}

// An advisory failure in the base lane carries only that lane; a lane with no
// fitting version this time is a deliberate miss and drops its batch.
func TestBernardCheckOSVErrorCarriesOneLane(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	r.cands = []domain.DepCandidate{r.laneCand("v1.3.0")}
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "x/mod", "v1.3.0").Return(false, errors.New("osv down")).Once()
	stored := xUpdate("v1.2.0", domain.DepMinor)
	r.skipRecord(stored, xUpdate("v2.0.0", domain.DepMajor))
	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo) // no Malicious(v1.2.0): nothing new is known
	require.NoError(t, err)
	assert.Equal(t, []domain.DepUpdate{stored}, got.Base().Updates)
	assert.Len(t, got.Batches, 1, "the major batch without a merge request is dropped")
}

// A dependency the ecosystem did not look up keeps only stored updates that
// are still not malicious and not ignored; a failed advisory drops them.
func TestBernardCheckRechecksRegistrySkips(t *testing.T) {
	r := newBernardRig(t, "0 6 * * *")
	r.repoReads()
	mod := func(n string) domain.DepUpdate {
		return domain.DepUpdate{DepRef: depRef(n, "v1.1.0"), Current: "v1.0.0", Kind: domain.DepMinor}
	}
	r.skips = []ports.DepsSkip{{Name: "mal", Current: "v1.0.0"}, {Name: "osv", Current: "v1.0.0"}, {Name: "ignored", Current: "v1.0.0"}, {Name: "ok", Current: "v1.0.0"}}
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "mal", "v1.1.0").Return(true, nil).Once()
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "osv", "v1.1.0").Return(false, errors.New("osv down")).Once()
	r.reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "ok", "v1.1.0").Return(false, nil).Once()
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mod("ignored").DepRef).Return(true, nil).Once()
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil)
	r.skipRecord(mod("ignored"), mod("mal"), mod("ok"), mod("osv"))
	got, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	assert.Equal(t, []domain.DepUpdate{mod("ok")}, got.Base().Updates)
}
