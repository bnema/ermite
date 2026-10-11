// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

// Every test runs in a synctest bubble with a fake clock: the delays are the
// real ones, a run happens exactly when the clock reaches it, and a Close that
// hangs is reported as a bubble deadlock instead of a timeout.

func TestMaintainerDebounces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		n := name(t, "tools")
		git := mocks.NewGitRepos(t)
		var runs atomic.Int32
		git.EXPECT().Maintain(mockAny, n).RunAndReturn(func(context.Context, domain.RepoName) error {
			runs.Add(1)
			return nil
		})
		m := app.NewMaintainer(ctx, git, app.MaintenanceDelay)
		defer m.Close(ctx)

		for range 10 {
			m.Schedule(ctx, n)
		}
		synctest.Sleep(app.MaintenanceDelay - time.Nanosecond)
		assert.Equal(t, int32(0), runs.Load(), "waits for the full delay")
		synctest.Sleep(time.Nanosecond)
		assert.Equal(t, int32(1), runs.Load(), "a burst makes one run")
		synctest.Sleep(time.Hour)
		assert.Equal(t, int32(1), runs.Load(), "no second run is coming")

		m.Schedule(ctx, n)
		synctest.Sleep(app.MaintenanceDelay - time.Nanosecond)
		assert.Equal(t, int32(1), runs.Load(), "a second burst waits for its own delay")
		synctest.Sleep(time.Nanosecond)
		assert.Equal(t, int32(2), runs.Load(), "a second burst, a second run")
	})
}

func TestMaintainerOneRunAtATimePerRepo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		const d = app.MaintenanceDelay
		m := app.NewMaintainer(ctx, git, d)
		defer m.Close(ctx)

		m.Schedule(ctx, n)
		synctest.Sleep(d)
		require.Equal(t, int32(1), runs.Load(), "first run started")
		m.Schedule(ctx, n) // during the run: queues exactly one more
		m.Schedule(ctx, n)
		m.Schedule(ctx, other)
		synctest.Sleep(d)
		assert.True(t, otherRan.Load(), "another repository is not blocked")
		assert.Equal(t, int32(1), runs.Load(), "the second run waits for the first")

		release <- struct{}{} // finish the first run: the queued one is armed
		synctest.Sleep(d - time.Nanosecond)
		assert.Equal(t, int32(1), runs.Load(), "the queued run waits for its delay")
		synctest.Sleep(time.Nanosecond)
		assert.Equal(t, int32(2), runs.Load(), "queued run")
		release <- struct{}{}
		synctest.Sleep(time.Hour)
		assert.Equal(t, int32(2), runs.Load(), "exactly one queued run")
		assert.Equal(t, int32(1), maxConcurrent.Load())
	})
}

func TestMaintainerTellsTheReaderAfterARun(t *testing.T) {
	for label, failure := range map[string]error{"success": nil, "failure": errors.New("boom")} {
		t.Run(label, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				n := name(t, "tools")
				git := mocks.NewGitRepos(t)
				git.EXPECT().Maintain(mockAny, n).Return(failure)
				told := make(chan struct{}, 2)
				fresh := mocks.NewRepoFreshener(t)
				fresh.EXPECT().Refresh(mockAny, n).Run(func(context.Context, domain.RepoName) { told <- struct{}{} }).Return().Once()
				m := app.NewMaintainer(ctx, git, app.MaintenanceDelay).WithFreshener(fresh)
				m.Schedule(ctx, n)
				synctest.Sleep(app.MaintenanceDelay - time.Nanosecond)
				assert.Empty(t, told, "not before the run")
				synctest.Sleep(time.Nanosecond)
				assert.Len(t, told, 1, "the reader is told when the run ends")
				m.Close(ctx)
				synctest.Sleep(time.Hour)
				assert.Len(t, told, 1, "told exactly once")
			})
		})
	}
}

func TestMaintainerFailureIsLoggedNotFatal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		n := name(t, "tools")
		git := mocks.NewGitRepos(t)
		var runs atomic.Int32
		git.EXPECT().Maintain(mockAny, n).RunAndReturn(func(context.Context, domain.RepoName) error {
			runs.Add(1)
			return errors.New("boom")
		})
		m := app.NewMaintainer(ctx, git, app.MaintenanceDelay)
		defer m.Close(ctx)
		m.Schedule(ctx, n)
		synctest.Sleep(app.MaintenanceDelay)
		assert.Equal(t, int32(1), runs.Load())
		m.Schedule(ctx, n)
		synctest.Sleep(app.MaintenanceDelay)
		assert.Equal(t, int32(2), runs.Load(), "later runs still happen")
	})
}

func TestMaintainerClose(t *testing.T) {
	ctx := context.Background()
	n := name(t, "tools")

	t.Run("pending runs are dropped", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			git := mocks.NewGitRepos(t) // no Maintain expected
			m := app.NewMaintainer(ctx, git, app.MaintenanceDelay)
			m.Schedule(ctx, n)
			m.Close(ctx)
			m.Schedule(ctx, n) // after Close: ignored
			m.Close(ctx)       // twice is fine
			synctest.Sleep(time.Hour)
		})
	})

	t.Run("a run in progress is cancelled and awaited", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			git := mocks.NewGitRepos(t)
			var cancelled atomic.Bool
			git.EXPECT().Maintain(mockAny, n).RunAndReturn(func(ctx context.Context, _ domain.RepoName) error {
				<-ctx.Done()
				cancelled.Store(true)
				return ctx.Err()
			})
			m := app.NewMaintainer(ctx, git, app.MaintenanceDelay)
			m.Schedule(ctx, n)
			synctest.Sleep(app.MaintenanceDelay) // the run is blocked in Maintain
			assert.False(t, cancelled.Load())
			m.Close(ctx) // a hang is a bubble deadlock: reported, no timeout needed
			assert.True(t, cancelled.Load())
		})
	})
}

func TestMaintainerConcurrentSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		git := mocks.NewGitRepos(t)
		git.EXPECT().Maintain(mockAny, mockAny).Return(nil).Maybe()
		m := app.NewMaintainer(ctx, git, app.MaintenanceDelay)
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 50 {
					m.Schedule(ctx, name(t, string(rune('a'+i%3))))
					runtime.Gosched()
				}
			}()
		}
		wg.Wait()
		m.Close(ctx)
	})
}
