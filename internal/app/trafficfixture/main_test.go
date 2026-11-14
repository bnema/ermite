// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
)

func TestFixture(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	dir := filepath.Join(root, "kv")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := kvstore.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := domain.ParseRepoName("alpha")
	b, _ := domain.ParseRepoName("beta")
	now := time.Now().UTC()
	for _, name := range []domain.RepoName{a, b} {
		if err := s.Create(ctx, domain.Repo{Name: name, CreatedAt: now, Visibility: domain.VisibilityPrivate}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	o := options{days: 90, scale: 1, repos: "alpha", dry: true}
	if err := run(ctx, root, o, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Fatal("missing summary")
	}
	s, err = kvstore.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	daily, err := s.Traffic().Daily(ctx, a, now.Add(-91*24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, bucket := range daily {
		if bucket.Views != 0 {
			t.Fatal("dry run wrote traffic")
		}
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	o.dry = false
	if err := run(ctx, root, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, err = kvstore.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	daily, err = s.Traffic().Daily(ctx, a, now.Add(-91*24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var views, uniques, clones, bots uint64
	for _, bucket := range daily {
		views += bucket.Views
		uniques += bucket.ViewUniques
		clones += bucket.Clones
		bots += bucket.Bots
		if bucket.Clones > 0 && bucket.CloneUniques == 0 {
			t.Fatalf("clones without unique visitors on %s", bucket.Start)
		}
		if bucket.CloneUniques > clonePool {
			t.Fatalf("%d clone uniques on %s, pool is %d", bucket.CloneUniques, bucket.Start, clonePool)
		}
		if bucket.Start.Before(now.Add(-91 * 24 * time.Hour)) {
			t.Fatal("outside retention")
		}
	}
	if views == 0 || uniques >= views || clones == 0 || clones >= views || bots == 0 {
		t.Fatalf("bad totals: %d %d %d %d", views, uniques, clones, bots)
	}
	hourly, err := s.Traffic().Hourly(ctx, a, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	populated := 0
	for _, bucket := range hourly {
		if bucket.Views > 0 {
			populated++
		}
	}
	if populated < 12 {
		t.Fatalf("only %d populated hours", populated)
	}
	other, err := s.Traffic().Daily(ctx, b, now.Add(-91*24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, bucket := range other {
		if bucket.Views != 0 {
			t.Fatal("filter ignored")
		}
	}
}
