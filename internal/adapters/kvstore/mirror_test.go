// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestMirrorRoundTripsAndDelete(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	repo := name(t, "mirror")
	g, err := s.GetGlobal(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.MirrorGlobalConfig{}, g)
	_, err = s.GetRepoConfig(ctx, repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = s.GetStatus(ctx, repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = s.GetSnapshot(ctx, repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, _, err = s.GetMirrorKey(ctx)
	require.ErrorIs(t, err, domain.ErrNotFound)
	require.NoError(t, s.DeleteRepoConfig(ctx, repo))
	require.ErrorIs(t, s.PutRepoConfig(ctx, domain.MirrorRepoConfig{Repo: repo}), domain.ErrNotFound)
	require.ErrorIs(t, s.PutStatus(ctx, domain.MirrorStatus{Repo: repo}), domain.ErrNotFound)
	require.ErrorIs(t, s.PutSnapshot(ctx, repo, domain.MirrorSnapshot{}), domain.ErrNotFound)
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, Visibility: domain.VisibilityPrivate}))
	g = domain.MirrorGlobalConfig{Enabled: true, URLTemplate: "git@example.org:{repo}", Cron: "*/5 * * * *", AllBranches: true, UpdatedAt: created}
	require.NoError(t, s.PutGlobal(ctx, g))
	gg, err := s.GetGlobal(ctx)
	require.NoError(t, err)
	require.Equal(t, g, gg)
	all := false
	c := domain.MirrorRepoConfig{Repo: repo, URL: "git@example.org:override", Cron: "0 * * * *", AllBranches: &all, Excluded: true, UpdatedAt: created}
	require.NoError(t, s.PutRepoConfig(ctx, c))
	cc, err := s.GetRepoConfig(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, c, cc)
	cs, err := s.ListRepoConfigs(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.MirrorRepoConfig{c}, cs)
	c.AllBranches = nil
	require.NoError(t, s.PutRepoConfig(ctx, c))
	cc, err = s.GetRepoConfig(ctx, repo)
	require.NoError(t, err)
	require.Nil(t, cc.AllBranches)
	st := domain.MirrorStatus{Repo: repo, State: domain.MirrorFailing, Class: domain.MirrorClassTransient, Reason: "retry", Detail: "network", ConsecutiveFailures: 3, LastAttemptAt: created, LastSuccessAt: created, NextRetryAt: created, PausedAt: created, UpdatedAt: created}
	require.NoError(t, s.PutStatus(ctx, st))
	ss, err := s.GetStatus(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, st, ss)
	statuses, err := s.ListStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.MirrorStatus{st}, statuses)
	id, err := domain.ParseObjectID("0123456789012345678901234567890123456789")
	require.NoError(t, err)
	snap := domain.MirrorSnapshot{URL: c.URL, AllBranches: true, At: created, Refs: []domain.MirrorRef{{Name: "refs/heads/main", ID: id}}}
	require.NoError(t, s.PutSnapshot(ctx, repo, snap))
	got, err := s.GetSnapshot(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, snap, got)
	sealed := []byte("opaque sealed seed")
	require.NoError(t, s.PutMirrorKey(ctx, sealed, created, true))
	require.ErrorIs(t, s.PutMirrorKey(ctx, []byte("other"), created, true), domain.ErrAlreadyExists)
	blob, at, err := s.GetMirrorKey(ctx)
	require.NoError(t, err)
	require.Equal(t, sealed, blob)
	require.Equal(t, created, at)
	raw, ok := s.RawMirrorKey(ctx)
	require.True(t, ok)
	require.False(t, bytes.Equal(raw, sealed), "raw record is a protobuf envelope, not bare key material")
	require.NoError(t, s.PutMirrorKey(ctx, []byte("rotated"), created, false))
	blob, _, err = s.GetMirrorKey(ctx)
	require.NoError(t, err)
	require.Equal(t, []byte("rotated"), blob)
	require.NoError(t, s.Delete(ctx, repo))
	_, err = s.GetRepoConfig(ctx, repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = s.GetStatus(ctx, repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = s.GetSnapshot(ctx, repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, _, err = s.GetMirrorKey(ctx)
	require.NoError(t, err)
	gg, err = s.GetGlobal(ctx)
	require.NoError(t, err)
	require.Equal(t, g, gg)
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, Visibility: domain.VisibilityPrivate}))
	_, err = s.GetStatus(ctx, repo)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMirrorStatusInvalidUTF8IsRepairedNotPanicking(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	repo := name(t, "mirror")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, Visibility: domain.VisibilityPrivate}))
	detail := strings.Repeat("é", 2048) + "\xff tail"
	require.NotPanics(t, func() {
		require.NoError(t, s.PutStatus(ctx, domain.MirrorStatus{Repo: repo, State: domain.MirrorFailing, Detail: detail, Reason: "bad \xfe"}))
	})
	got, err := s.GetStatus(ctx, repo)
	require.NoError(t, err)
	require.True(t, utf8.ValidString(got.Detail))
	require.True(t, utf8.ValidString(got.Reason))
	require.Contains(t, got.Detail, "? tail")
	// Other records refuse invalid text instead of panicking.
	require.NotPanics(t, func() {
		err := s.PutGlobal(ctx, domain.MirrorGlobalConfig{URLTemplate: "\xff"})
		require.ErrorIs(t, err, domain.ErrInvalid)
	})
	require.NotPanics(t, func() {
		err := s.PutRepoConfig(ctx, domain.MirrorRepoConfig{Repo: repo, URL: "\xff"})
		require.ErrorIs(t, err, domain.ErrInvalid)
	})
}
