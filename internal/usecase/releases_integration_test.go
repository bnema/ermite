// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"

	"bnema.dev/ermite/internal/adapters/blobdisk"
	"bnema.dev/ermite/internal/adapters/eventbus"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type releaseRig struct {
	ctx    context.Context
	repo   domain.RepoName
	store  *kvstore.Store
	blobs  ports.BlobStore
	reader *mocks.RepoReader
	tags   *mocks.ReleaseTagWriter
	pushes *mocks.PushHookService
	clock  *mocks.Clock
	events <-chan domain.Event
	svc    *usecase.Releases
}

func newReleaseRig(t *testing.T, limits ports.ReleaseLimits) *releaseRig {
	t.Helper()
	ctx := t.Context()
	store, err := kvstore.Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	n := name(t, "releases")
	require.NoError(t, store.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPublic, CreatedAt: now}))
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	reader := mocks.NewRepoReader(t)
	tags := mocks.NewReleaseTagWriter(t)
	pushes := mocks.NewPushHookService(t)
	bus := eventbus.NewWithBuffer(1024)
	b := blobdisk.New(t.TempDir())
	r := &releaseRig{ctx: ctx, repo: n, store: store, blobs: b, reader: reader, tags: tags, pushes: pushes, clock: clock, events: bus.Subscribe(ctx)}
	r.svc = usecase.NewReleases(store.Releases(), b, store, reader, tags, pushes, bus, clock, limits)
	return r
}
func (r *releaseRig) resolve(t *testing.T, tag string, err error) {
	t.Helper()
	v, e := domain.ParseRevision("refs/tags/" + tag)
	require.NoError(t, e)
	oid, e := domain.ParseObjectID(oidA)
	require.NoError(t, e)
	r.reader.EXPECT().ResolveRevision(mock.Anything, r.repo, v).Return(domain.ResolvedRevision{CommitID: oid}, err).Maybe()
}
func (r *releaseRig) create(t *testing.T, tag string, draft bool) domain.Release {
	t.Helper()
	r.resolve(t, tag, nil)
	rel, err := r.svc.CreateRelease(r.ctx, domain.Root(), r.repo, ports.CreateReleaseParams{Tag: tag, Draft: draft})
	require.NoError(t, err)
	return rel
}
func TestReleaseAccessMatrix(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(fmt.Sprint("private=", private), func(t *testing.T) {
			r := newReleaseRig(t, ports.ReleaseLimits{})
			published := r.create(t, "v1", false)
			draft := r.create(t, "draft", true)
			if private {
				_, err := r.store.Mutate(r.ctx, r.repo, func(repo *domain.Repo) (bool, error) { repo.Visibility = domain.VisibilityPrivate; return true, nil })
				require.NoError(t, err)
			}
			token := func(scopes []domain.Scope, branches ...string) domain.Principal {
				return domain.TokenPrincipal(domain.Token{ID: "t", Scopes: scopes, BranchPrefixes: branches})
			}
			for _, tc := range []struct {
				name        string
				p           domain.Principal
				read, write bool
			}{
				{"anonymous", domain.Anonymous(), !private, false},
				{"reader", token([]domain.Scope{domain.ScopeRepoRead}), true, false},
				{"writer", token([]domain.Scope{domain.ScopeRepoRead, domain.ScopeReleaseWrite}), true, true},
				{"write-only", token([]domain.Scope{domain.ScopeReleaseWrite}), !private, !private},
				{"branch", token([]domain.Scope{domain.ScopeRepoRead, domain.ScopeReleaseWrite}, "x/"), true, false},
				{"job-read", domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "j", Repo: r.repo, Permissions: map[string]string{"contents": "read"}}), true, false},
				{"job-write", domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "j", Repo: r.repo, Permissions: map[string]string{"contents": "write"}}), true, true},
				{"owner", domain.Owner(), true, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, err := r.svc.GetRelease(r.ctx, tc.p, r.repo, ports.ReleaseRef{ID: published.ID})
					if tc.read {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, domain.ErrNotFound)
					}
					_, err = r.svc.GetRelease(r.ctx, tc.p, r.repo, ports.ReleaseRef{ID: draft.ID})
					if tc.write {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, domain.ErrNotFound)
					}
					title := "changed"
					_, err = r.svc.UpdateRelease(r.ctx, tc.p, r.repo, published.ID, ports.UpdateReleaseParams{Title: &title})
					switch {
					case tc.write:
						require.NoError(t, err)
					case !tc.read:
						require.ErrorIs(t, err, domain.ErrNotFound)
					case tc.p.IsAnonymous():
						require.ErrorIs(t, err, domain.ErrUnauthenticated)
					default:
						require.ErrorIs(t, err, domain.ErrForbidden)
					}
				})
			}
		})
	}
}
func TestReleaseLifecycle(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	first := r.create(t, "v1", false)
	second := r.create(t, "v2", false)
	r.create(t, "draft", true)
	got, err := r.svc.GetRelease(r.ctx, domain.Anonymous(), r.repo, ports.ReleaseRef{Latest: true})
	require.NoError(t, err)
	require.Equal(t, second.ID, got.ID)
	yes := true
	_, err = r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, first.ID, ports.UpdateReleaseParams{MakeLatest: &yes})
	require.NoError(t, err)
	got, err = r.svc.GetRelease(r.ctx, domain.Root(), r.repo, ports.ReleaseRef{Latest: true})
	require.NoError(t, err)
	require.Equal(t, first.ID, got.ID)
	_, err = r.svc.CreateRelease(r.ctx, domain.Root(), r.repo, ports.CreateReleaseParams{Tag: "v1"})
	require.ErrorIs(t, err, domain.ErrAlreadyExists)
	page, err := r.svc.ListReleases(r.ctx, domain.Anonymous(), r.repo, 1, "")
	require.NoError(t, err)
	require.Len(t, page.Releases, 1)
	require.NotEmpty(t, page.NextPageToken)
	next, err := r.svc.ListReleases(r.ctx, domain.Anonymous(), r.repo, 1, page.NextPageToken)
	require.NoError(t, err)
	require.Equal(t, first.ID, next.Releases[0].ID)
	require.Empty(t, next.NextPageToken)
	r.svc.OnRefsMoved(r.ctx, r.repo, []domain.RefUpdate{{Ref: "refs/tags/v1", New: domain.ZeroOID}})
	got, err = r.svc.GetRelease(r.ctx, domain.Root(), r.repo, ports.ReleaseRef{ID: first.ID})
	require.NoError(t, err)
	require.True(t, got.Draft)
	require.True(t, got.PublishedAt.IsZero())
	require.False(t, got.MakeLatest)
	r.svc.OnRefsMoved(r.ctx, r.repo, []domain.RefUpdate{{Ref: "refs/tags/v1", New: oidA}})
	got, err = r.svc.GetRelease(r.ctx, domain.Root(), r.repo, ports.ReleaseRef{ID: first.ID})
	require.NoError(t, err)
	require.True(t, got.Draft)
	var kinds []string
	for len(r.events) > 0 {
		kinds = append(kinds, (<-r.events).EventKind())
	}
	require.Contains(t, kinds, "release.unpublished")
	require.NoError(t, r.svc.DeleteRelease(r.ctx, domain.Root(), r.repo, first.ID))
	_, err = r.svc.GetRelease(r.ctx, domain.Root(), r.repo, ports.ReleaseRef{ID: first.ID})
	require.ErrorIs(t, err, domain.ErrNotFound)
}
func TestReleaseMissingTag(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	r.resolve(t, "missing", domain.ErrNotFound)
	draft, err := r.svc.CreateRelease(r.ctx, domain.Root(), r.repo, ports.CreateReleaseParams{Tag: "missing", Draft: true})
	require.NoError(t, err)
	require.Empty(t, draft.TargetCommitish)
	no := false
	_, err = r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, draft.ID, ports.UpdateReleaseParams{Draft: &no})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	r.resolve(t, "new", domain.ErrNotFound)
	_, err = r.svc.CreateRelease(r.ctx, domain.Root(), r.repo, ports.CreateReleaseParams{Tag: "new"})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	v, err := domain.ParseRevision("main")
	require.NoError(t, err)
	oid, err := domain.ParseObjectID(oidA)
	require.NoError(t, err)
	r.reader.EXPECT().ResolveRevision(r.ctx, r.repo, v).Return(domain.ResolvedRevision{CommitID: oid}, nil).Once()
	r.tags.EXPECT().CreateTag(r.ctx, r.repo, "new", oid).Return(nil).Once()
	r.pushes.EXPECT().Record(r.ctx, domain.Root(), r.repo, []domain.RefUpdate{{Ref: "refs/tags/new", Old: domain.ZeroOID, New: oidA}}).Return(nil).Once()
	rel, err := r.svc.CreateRelease(r.ctx, domain.Root(), r.repo, ports.CreateReleaseParams{Tag: "new", TargetCommitish: "main"})
	require.NoError(t, err)
	require.Equal(t, oidA, rel.TargetCommitish)
}

func TestReleaseLatestPinsAndPrereleases(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	a := r.create(t, "a", false)
	b := r.create(t, "b", false)
	yes := true
	_, err := r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, b.ID, ports.UpdateReleaseParams{Prerelease: &yes})
	require.NoError(t, err)
	latest, err := r.svc.GetRelease(r.ctx, domain.Anonymous(), r.repo, ports.ReleaseRef{Latest: true})
	require.NoError(t, err)
	require.Equal(t, a.ID, latest.ID)
	_, err = r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, a.ID, ports.UpdateReleaseParams{MakeLatest: &yes})
	require.NoError(t, err)
	r.resolve(t, "c", nil)
	c, err := r.svc.CreateRelease(r.ctx, domain.Root(), r.repo, ports.CreateReleaseParams{Tag: "c", MakeLatest: true})
	require.NoError(t, err)
	a, err = r.svc.GetRelease(r.ctx, domain.Root(), r.repo, ports.ReleaseRef{ID: a.ID})
	require.NoError(t, err)
	require.False(t, a.MakeLatest)
	target := "b"
	_, err = r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, c.ID, ports.UpdateReleaseParams{Tag: &target})
	require.ErrorIs(t, err, domain.ErrAlreadyExists)
	require.NoError(t, r.svc.DeleteRelease(r.ctx, domain.Root(), r.repo, c.ID))
	latest, err = r.svc.GetRelease(r.ctx, domain.Root(), r.repo, ports.ReleaseRef{Latest: true})
	require.NoError(t, err)
	require.Equal(t, a.ID, latest.ID)
}

func TestReleaseEventsReachHomeAndRepositoryTopics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 0)
		pub := r.repo("pub", domain.VisibilityPublic)
		priv := r.repo("priv", domain.VisibilityPrivate)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		anon := r.watch(ctx, domain.Anonymous(), ports.WatchParams{Topics: []string{"home"}})
		owner := r.watch(ctx, domain.Root(), ports.WatchParams{Topics: []string{"repo:priv"}})
		synctest.Wait()
		for _, n := range []domain.RepoName{priv, pub} {
			vis := domain.VisibilityPublic
			if n == priv {
				vis = domain.VisibilityPrivate
			}
			rel := domain.Release{ID: 1, Repo: n, Tag: "v1"}
			r.feed <- domain.ReleasePublished{Release: rel, At: now, CreatedAt: now, Visibility: vis}
			r.feed <- domain.ReleaseUpdated{Release: rel, At: now, CreatedAt: now, Visibility: vis}
			r.feed <- domain.ReleaseDeleted{Release: rel, At: now, CreatedAt: now, Visibility: vis}
			r.feed <- domain.ReleaseUnpublished{Release: rel, At: now, CreatedAt: now, Visibility: vis}
		}
		for _, w := range []*watcher{anon, owner} {
			for _, kind := range []string{"release.published", "release.updated", "release.deleted", "release.unpublished"} {
				require.Equal(t, kind, w.next(t).Event.EventKind())
			}
		}
		anon.quiet(t, r, pub)
		owner.quiet(t, r, priv)
	})
}

func TestReleaseWriteRetriesUsageConflict(t *testing.T) {
	ctx := t.Context()
	repo := name(t, "retry")
	store := mocks.NewReleaseStore(t)
	meta := mocks.NewRepoMetaStore(t)
	reader := mocks.NewRepoReader(t)
	clock := mocks.NewClock(t)
	bus := mocks.NewEventBus(t)
	meta.EXPECT().Get(ctx, repo).Return(domain.Repo{Name: repo, Visibility: domain.VisibilityPublic, CreatedAt: now}, nil)
	clock.EXPECT().Now().Return(now).Maybe()
	rev, err := domain.ParseRevision("refs/tags/v1")
	require.NoError(t, err)
	reader.EXPECT().ResolveRevision(ctx, repo, rev).Return(domain.ResolvedRevision{}, domain.ErrNotFound)
	store.EXPECT().GetReleaseByTag(ctx, repo, "v1").Return(domain.Release{}, domain.ErrNotFound)
	store.EXPECT().NextReleaseID(ctx).Return(int64(1), nil)
	store.EXPECT().ListReleases(ctx, repo).Return(nil, nil).Twice()
	store.EXPECT().ReleaseUsage(ctx, repo).Return(int64(0), uint64(1), nil).Once()
	store.EXPECT().ApplyReleases(ctx, mock.MatchedBy(func(w ports.ReleaseWrite) bool { return w.UsageVersion == 1 && len(w.PutReleases) == 1 })).Return(domain.ErrConflict).Once()
	store.EXPECT().ReleaseUsage(ctx, repo).Return(int64(0), uint64(2), nil).Once()
	store.EXPECT().ApplyReleases(ctx, mock.MatchedBy(func(w ports.ReleaseWrite) bool { return w.UsageVersion == 2 && len(w.PutReleases) == 1 })).Return(nil).Once()
	store.EXPECT().GetRelease(ctx, repo, int64(1)).Return(domain.Release{ID: 1, Repo: repo, Tag: "v1", Draft: true}, nil)
	svc := usecase.NewReleases(store, nil, meta, reader, nil, nil, bus, clock, ports.ReleaseLimits{})
	rel, err := svc.CreateRelease(ctx, domain.Root(), repo, ports.CreateReleaseParams{Tag: "v1", Draft: true})
	require.NoError(t, err)
	require.EqualValues(t, 1, rel.ID)
}

func TestReleaseListHidesDraftCursors(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	pub := r.create(t, "v1", false)
	d1 := r.create(t, "d1", true)
	d2 := r.create(t, "d2", true)
	anon := domain.Anonymous()

	_, errDraft := r.svc.ListReleases(r.ctx, anon, r.repo, 1, fmt.Sprint(d1.ID))
	_, errUnknown := r.svc.ListReleases(r.ctx, anon, r.repo, 1, "999999")
	require.ErrorIs(t, errUnknown, domain.ErrInvalidPageToken)
	require.Equal(t, errUnknown, errDraft)

	// A writer still pages through drafts, newest first.
	var got []int64
	token := ""
	for {
		page, err := r.svc.ListReleases(r.ctx, domain.Root(), r.repo, 1, token)
		require.NoError(t, err)
		for _, rel := range page.Releases {
			got = append(got, rel.ID)
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	require.ElementsMatch(t, []int64{pub.ID, d1.ID, d2.ID}, got)
}

func TestReleaseUpdateTargetCreatesMissingTag(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	r.resolve(t, "missing", domain.ErrNotFound)
	draft, err := r.svc.CreateRelease(r.ctx, domain.Root(), r.repo, ports.CreateReleaseParams{Tag: "missing", Draft: true})
	require.NoError(t, err)
	no := false
	_, err = r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, draft.ID, ports.UpdateReleaseParams{Draft: &no})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)

	v, err := domain.ParseRevision("main")
	require.NoError(t, err)
	oid, err := domain.ParseObjectID(oidA)
	require.NoError(t, err)
	r.reader.EXPECT().ResolveRevision(r.ctx, r.repo, v).Return(domain.ResolvedRevision{CommitID: oid}, nil).Once()
	r.tags.EXPECT().CreateTag(r.ctx, r.repo, "missing", oid).Return(nil).Once()
	r.pushes.EXPECT().Record(r.ctx, domain.Root(), r.repo, []domain.RefUpdate{{Ref: "refs/tags/missing", Old: domain.ZeroOID, New: oidA}}).Return(nil).Once()
	main := "main"
	rel, err := r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, draft.ID, ports.UpdateReleaseParams{Draft: &no, TargetCommitish: &main})
	require.NoError(t, err)
	require.False(t, rel.Draft)
	require.Equal(t, oidA, rel.TargetCommitish)
}

func TestReleaseUpdateTargetNeverMovesExistingTag(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	rel := r.create(t, "v1", false)
	other := "other" // never resolved: the mock would fail the test
	got, err := r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, rel.ID, ports.UpdateReleaseParams{TargetCommitish: &other})
	require.NoError(t, err)
	require.Equal(t, oidA, got.TargetCommitish)
}

func TestReleaseLatestIndexTransitions(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	latest := func(id int64) {
		t.Helper()
		got, err := r.svc.GetRelease(r.ctx, domain.Anonymous(), r.repo, ports.ReleaseRef{Latest: true})
		if id == 0 {
			require.ErrorIs(t, err, domain.ErrNotFound)
			return
		}
		require.NoError(t, err)
		require.Equal(t, id, got.ID)
	}
	update := func(id int64, p ports.UpdateReleaseParams) {
		t.Helper()
		_, err := r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, id, p)
		require.NoError(t, err)
	}
	yes, no := true, false
	draft := r.create(t, "draft", true)
	latest(0)
	first := r.create(t, "v1", false)
	latest(first.ID)
	update(draft.ID, ports.UpdateReleaseParams{Draft: &no})
	latest(first.ID) // equal creation times: higher ID wins
	second := r.create(t, "v2", false)
	latest(second.ID)
	update(second.ID, ports.UpdateReleaseParams{Prerelease: &yes})
	latest(first.ID)
	update(second.ID, ports.UpdateReleaseParams{Prerelease: &no})
	latest(second.ID)
	update(first.ID, ports.UpdateReleaseParams{MakeLatest: &yes})
	latest(first.ID)
	tag := "renamed"
	r.resolve(t, tag, nil)
	update(first.ID, ports.UpdateReleaseParams{Tag: &tag})
	latest(first.ID)
	require.NoError(t, r.svc.DeleteRelease(r.ctx, domain.Root(), r.repo, first.ID))
	latest(second.ID)
	update(second.ID, ports.UpdateReleaseParams{Draft: &yes})
	latest(draft.ID)
	update(draft.ID, ports.UpdateReleaseParams{Draft: &yes})
	latest(0)
}

// No ListReleases expectation: latest resolution must use only point reads.
func TestReleaseLatestUsesIndex(t *testing.T) {
	ctx := t.Context()
	repo := name(t, "indexed")
	store := mocks.NewReleaseStore(t)
	meta := mocks.NewRepoMetaStore(t)
	meta.EXPECT().Get(ctx, repo).Return(domain.Repo{Name: repo, Visibility: domain.VisibilityPublic}, nil)
	store.EXPECT().GetLatestRelease(ctx, repo).Return(domain.Release{ID: 42, Repo: repo}, nil).Once()
	svc := usecase.NewReleases(store, nil, meta, nil, nil, nil, nil, nil, ports.ReleaseLimits{})
	got, err := svc.GetRelease(ctx, domain.Anonymous(), repo, ports.ReleaseRef{Latest: true})
	require.NoError(t, err)
	require.EqualValues(t, 42, got.ID)
}
