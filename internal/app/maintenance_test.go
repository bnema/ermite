// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestMaintainerDebounces(t *testing.T) {
	ctx := context.Background()
	n := name(t, "tools")
	git := mocks.NewGitRepos(t)
	var runs atomic.Int32
	git.EXPECT().Maintain(mockAny, n).RunAndReturn(func(context.Context, domain.RepoName) error {
		runs.Add(1)
		return nil
	})
	m := app.NewMaintainer(ctx, git, 50*time.Millisecond)
	defer m.Close(ctx)

	for range 10 {
		m.Schedule(n)
	}
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, int32(0), runs.Load(), "waits for the delay")
	waitFor(t, func() bool { return runs.Load() == 1 }, "one run")
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, int32(1), runs.Load(), "a burst makes one run")

	m.Schedule(n)
	waitFor(t, func() bool { return runs.Load() == 2 }, "a second burst, a second run")
}

func TestMaintainerOneRunAtATimePerRepo(t *testing.T) {
	ctx := context.Background()
	n, other := name(t, "tools"), name(t, "other")
	git := mocks.NewGitRepos(t)
	var concurrent, maxConcurrent, runs atomic.Int32
	release := make(chan struct{})
	var otherRan atomic.Bool
	git.EXPECT().Maintain(mockAny, n).RunAndReturn(func(context.Context, domain.RepoName) error {
		c := concurrent.Add(1)
		for {
			m := maxConcurrent.Load()
			if c <= m || maxConcurrent.CompareAndSwap(m, c) {
				break
			}
		}
		runs.Add(1)
		<-release
		concurrent.Add(-1)
		return nil
	})
	git.EXPECT().Maintain(mockAny, other).RunAndReturn(func(context.Context, domain.RepoName) error {
		otherRan.Store(true)
		return nil
	})
	m := app.NewMaintainer(ctx, git, 10*time.Millisecond)
	defer m.Close(ctx)

	m.Schedule(n)
	waitFor(t, func() bool { return runs.Load() == 1 }, "first run to start")
	m.Schedule(n) // during the run: queues exactly one more
	m.Schedule(n)
	m.Schedule(other)
	waitFor(t, otherRan.Load, "another repository is not blocked")
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, int32(1), runs.Load(), "the second run waits for the first")

	release <- struct{}{} // finish first run
	waitFor(t, func() bool { return runs.Load() == 2 }, "queued run")
	release <- struct{}{}
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, int32(2), runs.Load(), "exactly one queued run")
	assert.Equal(t, int32(1), maxConcurrent.Load())
}

func TestMaintainerFailureIsLoggedNotFatal(t *testing.T) {
	ctx := context.Background()
	n := name(t, "tools")
	git := mocks.NewGitRepos(t)
	var runs atomic.Int32
	git.EXPECT().Maintain(mockAny, n).RunAndReturn(func(context.Context, domain.RepoName) error {
		runs.Add(1)
		return errors.New("boom")
	})
	m := app.NewMaintainer(ctx, git, 5*time.Millisecond)
	defer m.Close(ctx)
	m.Schedule(n)
	waitFor(t, func() bool { return runs.Load() == 1 }, "run")
	m.Schedule(n)
	waitFor(t, func() bool { return runs.Load() == 2 }, "later runs still happen")
}

func TestMaintainerClose(t *testing.T) {
	ctx := context.Background()
	n := name(t, "tools")

	t.Run("pending runs are dropped", func(t *testing.T) {
		git := mocks.NewGitRepos(t) // no Maintain expected
		m := app.NewMaintainer(ctx, git, time.Hour)
		m.Schedule(n)
		m.Close(ctx)
		m.Schedule(n) // after Close: ignored
		m.Close(ctx)  // twice is fine
	})

	t.Run("a run in progress is cancelled and awaited", func(t *testing.T) {
		git := mocks.NewGitRepos(t)
		started := make(chan struct{})
		var cancelled atomic.Bool
		git.EXPECT().Maintain(mockAny, n).RunAndReturn(func(ctx context.Context, _ domain.RepoName) error {
			close(started)
			<-ctx.Done()
			cancelled.Store(true)
			return ctx.Err()
		})
		m := app.NewMaintainer(ctx, git, time.Millisecond)
		m.Schedule(n)
		<-started
		done := make(chan struct{})
		go func() { m.Close(ctx); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not return")
		}
		assert.True(t, cancelled.Load())
	})
}

func TestMaintainerConcurrentSchedule(t *testing.T) {
	ctx := context.Background()
	git := mocks.NewGitRepos(t)
	git.EXPECT().Maintain(mockAny, mockAny).Return(nil).Maybe()
	m := app.NewMaintainer(ctx, git, time.Millisecond)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				m.Schedule(name(t, string(rune('a'+i%3))))
				time.Sleep(100 * time.Microsecond)
			}
		}()
	}
	wg.Wait()
	m.Close(ctx)
	require.True(t, true)
}
