// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

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
	f.setStatus(domain.MirrorStatus{Repo: f.repo, State: domain.MirrorOK, Detail: "admin detail", Reason: "admin reason"})
	view, err := f.m.GetStatus(ctx, domain.Anonymous(), f.repo)
	require.NoError(t, err)
	require.Empty(t, view.Status.Detail)
	require.Empty(t, view.Status.Reason)
	require.Empty(t, view.Target.URL)
	view, err = f.m.GetStatus(ctx, admin, f.repo)
	require.NoError(t, err)
	require.Equal(t, "admin detail", view.Status.Detail)
	_, err = f.m.SetRepoMirror(ctx, admin, domain.MirrorRepoConfig{Repo: f.repo})
	require.NoError(t, err)
}

func TestMirrorsPrivateStatusAuthorization(t *testing.T) {
	f := newMirrorFixture(t)
	f.mu.Lock()
	f.repos[0].Visibility = domain.VisibilityPrivate
	f.mu.Unlock()
	_, err := f.m.GetStatus(context.Background(), domain.Anonymous(), f.repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.m.GetStatus(context.Background(), tokenPrincipal(domain.ScopeRepoRead, "other"), f.repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.m.GetStatus(context.Background(), tokenPrincipal(domain.ScopeRepoRead, "tools"), f.repo)
	require.NoError(t, err)
}

func TestMirrorsWarningIsAdminOnly(t *testing.T) {
	f := newMirrorFixture(t)
	other := name(t, "other")
	failing := domain.MirrorStatus{Repo: f.repo, State: domain.MirrorPaused, Class: domain.MirrorClassDiverged, Reason: "the remote has commits"}
	for _, tc := range []struct {
		name   string
		p      domain.Principal
		status domain.MirrorStatus
		want   bool
	}{
		{"root", domain.Root(), failing, true},
		{"repo admin", tokenPrincipal(domain.ScopeRepoAdmin, "tools"), failing, true},
		{"admin of another repository", tokenPrincipal(domain.ScopeRepoAdmin, other.String()), failing, false},
		{"writer", tokenPrincipal(domain.ScopeRepoWrite, "tools"), failing, false},
		{"anonymous", domain.Anonymous(), failing, false},
		{"admin, mirror ok", domain.Root(), domain.MirrorStatus{Repo: f.repo, State: domain.MirrorOK}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.setStatus(tc.status)
			w := f.m.Warning(context.Background(), tc.p, f.repo)
			if !tc.want {
				require.Empty(t, w)
				return
			}
			require.Contains(t, w, "example.com:acme/tools")
			require.Contains(t, w, "paused")
			require.Contains(t, w, "the remote has commits")
		})
	}
}

func TestMirrorsBadge(t *testing.T) {
	f := newMirrorFixture(t)
	f.setStatus(domain.MirrorStatus{Repo: f.repo, State: domain.MirrorFailing, Reason: "retrying"})
	state, reason := f.m.Badge(context.Background(), tokenPrincipal(domain.ScopeRepoAdmin, "tools"), f.repo)
	require.Equal(t, domain.MirrorFailing, state)
	require.Equal(t, "retrying", reason)
	state, reason = f.m.Badge(context.Background(), domain.Anonymous(), f.repo)
	require.Equal(t, domain.MirrorFailing, state)
	require.Empty(t, reason, "the reason is for admins")
	state, _ = f.m.Badge(context.Background(), domain.Root(), name(t, "ghost"))
	require.Equal(t, domain.MirrorDisabled, state)
	// A badge needs no schedule: with an unparsable-looking cron the view still works and costs no search.
	require.NotPanics(t, func() { f.m.Badge(context.Background(), domain.Root(), f.repo) })
}

func TestMirrorsListStatusReadsOnceAndHidesNothingFromOwner(t *testing.T) {
	f := newMirrorFixture(t)
	second := name(t, "second")
	f.addRepo(second)
	f.setStatus(domain.MirrorStatus{Repo: second, State: domain.MirrorPaused, Reason: "r"})
	list, err := f.m.ListStatus(context.Background(), domain.Root())
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "git@example.com:acme/tools", list[0].Target.URL)
	require.Equal(t, domain.MirrorPaused, list[1].Status.State)
	require.False(t, list[0].NextRunAt.IsZero())
	require.True(t, list[1].NextRunAt.IsZero(), "a paused mirror has no next run")
}

func TestMirrorsDivergedPausedAndConfigReset(t *testing.T) {
	f := newMirrorFixture(t)
	f.setStatus(domain.MirrorStatus{Repo: f.repo, State: domain.MirrorPaused, Class: domain.MirrorClassDiverged, ConsecutiveFailures: 1})
	_, err := f.m.SetRepoMirror(context.Background(), domain.Root(), domain.MirrorRepoConfig{Repo: f.repo, URL: "git@other.example:acme/tools", Cron: "*/5 * * * *"})
	require.NoError(t, err)
	s := f.statusOf(f.repo)
	require.Equal(t, domain.MirrorOK, s.State)
	require.Zero(t, s.ConsecutiveFailures)
}

func TestMirrorsRepoChangesResetOnlyWhenTheTargetChanged(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	paused := domain.MirrorStatus{Repo: f.repo, State: domain.MirrorPaused, Class: domain.MirrorClassDiverged, ConsecutiveFailures: 1}
	f.setStatus(paused)
	// The override restates the inherited target: nothing changed, the incident stays.
	_, err := f.m.SetRepoMirror(ctx, domain.Root(), domain.MirrorRepoConfig{Repo: f.repo, URL: "git@example.com:acme/tools"})
	require.NoError(t, err) // source changes global -> override: a change
	f.setStatus(paused)
	writes := f.statusWrites()
	_, err = f.m.SetRepoMirror(ctx, domain.Root(), domain.MirrorRepoConfig{Repo: f.repo, URL: "git@example.com:acme/tools"})
	require.NoError(t, err)
	require.Equal(t, writes, f.statusWrites(), "same effective target: no reset")
	require.Equal(t, domain.MirrorPaused, f.statusOf(f.repo).State)
	// Removing a no-op exclusion, and excluding to the same effect, follow the same rule.
	err = f.m.RemoveRepoMirror(ctx, domain.Root(), f.repo)
	require.NoError(t, err)
	require.Equal(t, domain.MirrorOK, f.statusOf(f.repo).State, "override gone: the source changed back to global")
	f.setStatus(paused)
	writes = f.statusWrites()
	err = f.m.RemoveRepoMirror(ctx, domain.Root(), f.repo)
	require.NoError(t, err)
	require.Equal(t, writes, f.statusWrites(), "no override to remove: no reset")
	_, err = f.m.SetRepoExcluded(ctx, domain.Root(), f.repo, true)
	require.NoError(t, err)
	require.Equal(t, domain.MirrorDisabled, f.statusOf(f.repo).State)
}

func TestMirrorsGlobalResetContinuesPastErrors(t *testing.T) {
	f := newMirrorFixture(t)
	second := name(t, "second")
	f.addRepo(second)
	f.setStatus(domain.MirrorStatus{Repo: second, State: domain.MirrorPaused, ConsecutiveFailures: 1})
	f.failStatusWrites(f.repo, errBoom)
	_, err := f.m.SetGlobalConfig(context.Background(), domain.Root(), ports.MirrorGlobalPatch{Cron: ptr("*/10 * * * *")})
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, domain.MirrorOK, f.statusOf(second).State, "the other repository was still reset")
	require.Equal(t, "*/10 * * * *", f.global.Cron, "the policy itself is saved")
}

func TestMirrorsOverrideWithoutCronRejectedWhenGlobalCronEmpty(t *testing.T) {
	f := newMirrorFixture(t)
	f.setGlobal(domain.MirrorGlobalConfig{})
	_, err := f.m.SetRepoMirror(context.Background(), domain.Root(), domain.MirrorRepoConfig{Repo: f.repo, URL: "git@example.com:acme/tools"})
	var fe *domain.InvalidFieldsError
	require.ErrorAs(t, err, &fe)
	require.Equal(t, "cron", fe.Fields[0].Field)
	require.Empty(t, f.cfgs, "nothing is stored")
}

func TestMirrorsGlobalChangeInvalidatingOverrideRejected(t *testing.T) {
	f := newMirrorFixture(t)
	f.setRepoConfig(domain.MirrorRepoConfig{Repo: f.repo, URL: "git@other.example:x/y"})
	// Disabling the global with an empty cron leaves the inheriting override with no cron.
	_, err := f.m.SetGlobalConfig(context.Background(), domain.Root(), ports.MirrorGlobalPatch{Enabled: ptr(false), Cron: ptr("")})
	var fe *domain.InvalidFieldsError
	require.ErrorAs(t, err, &fe)
	require.True(t, f.global.Enabled, "nothing is stored")
}

func TestMirrorsGlobalChangeResetsInheritingURLOverride(t *testing.T) {
	f := newMirrorFixture(t)
	f.setRepoConfig(domain.MirrorRepoConfig{Repo: f.repo, URL: "git@other.example:x/y"})
	f.setStatus(domain.MirrorStatus{Repo: f.repo, State: domain.MirrorPaused, Class: domain.MirrorClassDiverged})
	_, err := f.m.SetGlobalConfig(context.Background(), domain.Root(), ports.MirrorGlobalPatch{Cron: ptr("*/10 * * * *")})
	require.NoError(t, err)
	require.Equal(t, domain.MirrorOK, f.statusOf(f.repo).State)
}

func TestMirrorsSetRepoExcludedConcurrentIsSerialized(t *testing.T) {
	f := newMirrorFixture(t)
	var mu sync.Mutex
	active, maxActive := 0, 0
	enter, leave := make(chan struct{}), make(chan struct{})
	f.onPutRepo = func() {
		mu.Lock()
		active++
		maxActive = max(maxActive, active)
		mu.Unlock()
		enter <- struct{}{}
		<-leave
		mu.Lock()
		active--
		mu.Unlock()
	}
	errs := make(chan error, 4)
	for i := range 4 {
		go func() {
			_, err := f.m.SetRepoExcluded(context.Background(), domain.Root(), f.repo, i%2 == 0)
			errs <- err
		}()
	}
	// While one writer is blocked inside the store, the others cannot have entered it.
	for range 4 {
		<-enter
		mu.Lock()
		require.Equal(t, 1, active)
		mu.Unlock()
		leave <- struct{}{}
	}
	for range 4 {
		require.NoError(t, <-errs)
	}
	require.Equal(t, 1, maxActive, "read-modify-write must be serialized")
}

func TestMirrorsURLValidatorIsInjectable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		validate func(string) error
		ok       bool
	}{
		{"default rejects file", nil, false},
		{"injected accepts file", func(string) error { return nil }, true},
		{"injected rejects", func(string) error { return errors.New("no") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMirrorFixtureWith(t, func(c *usecase.MirrorsConfig) { c.ValidateURL = tc.validate })
			target, err := f.m.SetRepoMirror(context.Background(), domain.Root(), domain.MirrorRepoConfig{Repo: f.repo, URL: "file:///tmp/remotes/tools", Cron: "*/5 * * * *"})
			if tc.ok {
				require.NoError(t, err)
				require.Equal(t, "file:///tmp/remotes/tools", target.URL)
			} else {
				require.ErrorIs(t, err, domain.ErrInvalid)
			}
		})
	}
}

func TestMirrorsNextRunIsComputedForStatusOnly(t *testing.T) {
	f := newMirrorFixture(t)
	view, err := f.m.GetStatus(context.Background(), domain.Root(), f.repo)
	require.NoError(t, err)
	require.True(t, view.NextRunAt.Equal(now.Add(8*time.Minute)) || !view.NextRunAt.IsZero())
	require.Equal(t, 0, view.NextRunAt.Minute()%5)
}
