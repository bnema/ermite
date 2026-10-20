// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestMirrorsPushRecordNotification(t *testing.T) {
	f := newPushFixture(t)
	n := name(t, "tools")
	trigger := mocks.NewMirrorTrigger(t)
	updates := []domain.RefUpdate{}
	f.meta.On("Mutate", mock.Anything, n, mock.Anything).Return(domain.Repo{Name: n}, nil)
	f.git.On("DefaultBranch", mock.Anything, n).Return("main", true, nil)
	f.bus.On("Publish", mock.Anything, mock.Anything).Return()
	f.sched.On("Schedule", mock.Anything, n).Return()
	trigger.EXPECT().OnPush(mock.Anything, n, updates).Return().Once() // synchronous: verified when Record returns
	require.NoError(t, f.pushes.WithMirror(trigger).Record(context.Background(), domain.Root(), n, updates))
}

func TestMirrorsMergeNotifiesMirror(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	trigger := mocks.NewMirrorTrigger(t)
	notified := make(chan []domain.RefUpdate, 1)
	trigger.EXPECT().OnPush(mock.Anything, r.repo, mock.Anything).Run(func(_ context.Context, _ domain.RepoName, u []domain.RefUpdate) { notified <- u }).Return().Once()
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
	trigger.EXPECT().OnPush(mock.Anything, n, mock.Anything).Return().Once()
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
	f.pushes(t, nil)
	_, err := f.m.Sync(context.Background(), domain.Root(), f.repo, true)
	require.NoError(t, err)
	f.advance(5 * time.Minute)
	f.m.ScheduleNow(context.Background())
	f.drain(t)
	require.Equal(t, 1, f.refReads(), "due and clean: skipped")
	f.m.OnPush(context.Background(), f.repo, []domain.RefUpdate{{Ref: "refs/heads/main"}})
	f.advance(5 * time.Minute)
	f.m.ScheduleNow(context.Background())
	f.drain(t)
	require.Equal(t, 2, f.refReads(), "dirty repository synced on the next due tick")
}

func TestMirrorsEventsAreDeliveredByVisibility(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 0)
		pub := r.repo("pub", domain.VisibilityPublic)
		priv := r.repo("priv", domain.VisibilityPrivate)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		anon := r.watch(ctx, domain.Anonymous(), ports.WatchParams{})
		root := r.watch(ctx, domain.Root(), ports.WatchParams{})
		synctest.Wait()
		for _, n := range []domain.RepoName{priv, pub} {
			vis := domain.VisibilityPublic
			if n == priv {
				vis = domain.VisibilityPrivate
			}
			r.feed <- domain.MirrorFailed{Repo: n, State: domain.MirrorPaused, Class: domain.MirrorClassDiverged, At: now, Visibility: vis, CreatedAt: now}
			r.feed <- domain.MirrorRecovered{Repo: n, At: now, Visibility: vis, CreatedAt: now}
		}
		for _, n := range []domain.RepoName{priv, pub} {
			failed := root.next(t).Event.(domain.MirrorFailed)
			require.Equal(t, n, failed.Repo)
			require.Equal(t, n, root.next(t).Event.(domain.MirrorRecovered).Repo)
		}
		failed := anon.next(t).Event.(domain.MirrorFailed)
		require.Equal(t, pub, failed.Repo, "a private repository's mirror events stay private")
		require.Equal(t, pub, anon.next(t).Event.(domain.MirrorRecovered).Repo)
		anon.quiet(t, r, pub)
	})
}
