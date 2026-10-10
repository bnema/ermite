// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
)

type fixedClock struct{}

func (fixedClock) Now() time.Time { return now }

// Concurrent Create of one name over the real adapters: one wins, the others
// get ErrAlreadyExists and leave nothing behind.
func TestCreateConcurrentSameNameRealAdapters(t *testing.T) {
	ctx := context.Background()
	bin, err := exec.LookPath("git")
	if err != nil {
		unusable(t, "git not found")
	}
	data := t.TempDir()
	for _, d := range []string{"repos/.staging", "trash", "run/git-home", "kv"} {
		require.NoError(t, os.MkdirAll(filepath.Join(data, d), 0o700))
	}
	git := gitcli.New(gitcli.Config{Bin: bin, DataDir: data, Path: os.Getenv("PATH")})
	if _, err := git.CheckVersion(ctx); err != nil {
		unusable(t, "git is not usable by ermite: %v", err)
	}
	store, err := kvstore.Open(ctx, filepath.Join(data, "kv"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	repos := app.NewRepos(store, git, fixedClock{})
	n := name(t, "tools")

	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repos.Create(ctx, domain.Root(), app.CreateParams{Name: n})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	ok, exists := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrAlreadyExists):
			exists++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, workers-1, exists)

	entries, err := os.ReadDir(filepath.Join(data, "repos"))
	require.NoError(t, err)
	var dirs []string
	for _, e := range entries {
		if e.Name() != ".staging" {
			dirs = append(dirs, e.Name())
		}
	}
	assert.Equal(t, []string{"tools.git"}, dirs)

	all, err := store.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1)

	staging, err := os.ReadDir(filepath.Join(data, "repos", ".staging"))
	require.NoError(t, err)
	assert.Empty(t, staging)
}

// unusable skips the test, or fails it in CI so that CI cannot pass vacuously.
func unusable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}
