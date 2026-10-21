// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/blobdisk"
	"bnema.dev/ermite/internal/adapters/rpc"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

func TestReleaseCLIEndToEnd(t *testing.T) {
	c := newCLIHarness(t)
	c.cli(t, adminToken, "repo", "create", "proj", "--visibility", "public").ok(t)
	wt := c.workTree("main", "README.md")
	c.mustGit(wt, "tag", "v1")
	c.mustGit(wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "main", "refs/tags/v1")

	// P8 owns production wiring. Compose the real release service and RPC here,
	// sharing the forge's database, auth, git reader, pushes and event bus.
	f := c.forge
	releases := usecase.NewReleases(f.store.Releases(), blobdisk.New(filepath.Join(c.data, "release-test-blobs")), f.store, f.git, f.git, f.Pushes, f.Bus, systemClock{}, ports.ReleaseLimits{})
	api := rpc.New(rpc.Config{Auth: f.Auth, Releases: releases, SigningKey: f.SigningKey, Sessions: f.Sessions})
	mux := http.NewServeMux()
	mux.Handle("/-/api/", http.StripPrefix("/-/api", api.Handler()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	oldURL := c.url
	c.url = srv.URL
	bin := func(args ...string) string {
		t.Helper()
		out, stderr, code := c.bin(t, adminToken, args...)
		require.Equal(t, 0, code, stderr)
		return out
	}
	asset := filepath.Join(t.TempDir(), "binary.dat")
	data := []byte("release payload\x00\xff")
	require.NoError(t, os.WriteFile(asset, data, 0o600))
	created := decode[jsonRelease](t, bin("release", "create", "proj", "v1", "--title", "Version one", "--notes", "# shipped", "--asset", asset+"#Binary", "--json"))
	require.Positive(t, created.ID)
	require.Len(t, created.Assets, 1)
	assert.Equal(t, "Binary", created.Assets[0].Label)
	viewed := decode[jsonRelease](t, bin("release", "view", "proj", "v1", "--json"))
	assert.Equal(t, created.ID, viewed.ID)
	assert.Equal(t, "# shipped", viewed.Notes)
	assert.Contains(t, bin("release", "list", "proj"), "Version one")
	bin("release", "edit", "proj", strconv.FormatInt(created.ID, 10), "--title", "Edited", "--no-prerelease")
	assert.Equal(t, "Edited", decode[jsonRelease](t, bin("release", "view", "proj", "latest", "--json")).Title)
	dir := t.TempDir()
	bin("release", "download", "proj", "latest", "--dir", dir, "--pattern", "*.dat")
	downloaded, err := os.ReadFile(filepath.Join(dir, "binary.dat"))
	require.NoError(t, err)
	assert.Equal(t, data, downloaded)
	_, stderr, code := c.bin(t, adminToken, "release", "upload", "proj", "v1", asset)
	requireOneLineError(t, stderr, code, "binary.dat", "--clobber")
	bin("release", "upload", "proj", "v1", asset, "--clobber")
	bin("release", "delete", "proj", "v1", "--yes")
	_, stderr, code = c.bin(t, adminToken, "release", "view", "proj", "v1")
	requireOneLineError(t, stderr, code, "not found")
	// The unmodified forge intentionally has no release wiring yet.
	c.url = oldURL
	_, stderr, code = c.bin(t, adminToken, "release", "list", "proj")
	requireOneLineError(t, stderr, code, "the server has no releases (it lacks the releases feature)")
}
