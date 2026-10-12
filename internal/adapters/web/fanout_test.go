// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func frag(s string) []fragment { return []fragment{{event: "refs-list", html: s}} }

func TestFanoutBuildsOncePerKey(t *testing.T) {
	t.Parallel()
	base := fanKey{repo: "x", event: "b-1"}
	for _, tt := range []struct {
		name  string
		keys  []fanKey
		built int32
	}{
		{"same key", []fanKey{base, base, base}, 1},
		{"owner and anonymous never share", []fanKey{base, {repo: "x", event: "b-1", owner: true}}, 2},
		{"another event", []fanKey{base, {repo: "x", event: "b-2"}}, 2},
		{"another repository", []fanKey{base, {repo: "y", event: "b-1"}}, 2},
		{"no event id: nothing to share by", []fanKey{{repo: "x"}, {repo: "x"}}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var f fanout
			var built atomic.Int32
			for _, k := range tt.keys {
				got, err := f.do(context.Background(), k, func() ([]fragment, error) {
					built.Add(1)
					return frag("for-owner=" + map[bool]string{true: "yes", false: "no"}[k.owner]), nil
				})
				require.NoError(t, err)
				assert.Equal(t, "for-owner="+map[bool]string{true: "yes", false: "no"}[k.owner], got[0].html,
					"a watcher never gets the output built for the other class")
			}
			assert.Equal(t, tt.built, built.Load())
		})
	}
}

// While the first watcher builds, every other one waits for it: in a synctest
// bubble Wait returns once all goroutines are blocked, so the count of the
// watchers that entered and none that returned proves the others wait on the
// build instead of making their own.
func TestFanoutConcurrentWatchersShareOneBuild(t *testing.T) {
	t.Parallel()
	const watchers = 50
	synctest.Test(t, func(t *testing.T) {
		var f fanout
		var built, entered, returned atomic.Int32
		release := make(chan struct{})
		key := fanKey{repo: "x", event: "b-1"}
		var wg sync.WaitGroup
		results := make([][]fragment, watchers)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				entered.Add(1)
				results[i], _ = f.do(context.Background(), key, func() ([]fragment, error) {
					built.Add(1)
					<-release
					return frag("once"), nil
				})
				returned.Add(1)
			}()
		}
		synctest.Wait() // the builder is on release, the others on its entry
		require.Equal(t, int32(watchers), entered.Load())
		require.Equal(t, int32(1), built.Load(), "one builder")
		require.Zero(t, returned.Load(), "the other %d wait for it", watchers-1)

		close(release)
		wg.Wait()
		assert.Equal(t, int32(1), built.Load())
		for _, r := range results {
			assert.Equal(t, frag("once"), r)
		}
	})
}

func TestFanoutFailureIsNotKept(t *testing.T) {
	t.Parallel()
	var f fanout
	key := fanKey{repo: "x", event: "b-1"}
	_, err := f.do(context.Background(), key, func() ([]fragment, error) { return nil, errors.New("boom") })
	require.Error(t, err)
	got, err := f.do(context.Background(), key, func() ([]fragment, error) { return frag("ok"), nil })
	require.NoError(t, err)
	assert.Equal(t, frag("ok"), got, "the next watcher builds again")
}

func TestFanoutWaiterStopsWithItsContext(t *testing.T) {
	t.Parallel()
	var f fanout
	key := fanKey{repo: "x", event: "b-1"}
	started, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = f.do(context.Background(), key, func() ([]fragment, error) {
			close(started)
			<-release
			return frag("late"), nil
		})
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.do(ctx, key, func() ([]fragment, error) { return frag("never"), nil })
	assert.ErrorIs(t, err, context.Canceled)
	close(release)
}

func TestFanoutIsBounded(t *testing.T) {
	t.Parallel()
	var f fanout
	for i := range fanMax + 10 {
		key := fanKey{repo: "x", event: "b-" + string(rune('A'+i%26)) + string(rune('a'+i/26))}
		_, err := f.do(context.Background(), key, func() ([]fragment, error) { return frag("x"), nil })
		require.NoError(t, err)
	}
	assert.LessOrEqual(t, len(f.m), fanMax)
}

// refsMocks is a web server over mocks that expect the refs fragment to load
// the repository and its refs once per build of each class of watcher
// (anonymous, owner), asking the use cases as that class's principal, and to
// ask for a commit once per build when commits is set.
func refsMocks(t *testing.T, anonymous, owner int, commits bool) *Server {
	t.Helper()
	const id = "0123456789abcdef0123456789abcdef01234567"
	repos := mocks.NewRepoService(t)
	browse := mocks.NewBrowseService(t)
	for _, class := range []struct {
		p      domain.Principal
		builds int
	}{{domain.Anonymous(), anonymous}, {domain.Owner(), owner}} {
		p, builds := class.p, class.builds
		if builds == 0 {
			continue
		}
		repos.EXPECT().Get(mock.Anything, p, repoName(t, "x")).Return(publicRepo(t, "x").Repo, nil).Times(builds)
		browse.EXPECT().ListRefs(mock.Anything, p, repoName(t, "x"), mock.MatchedBy(func(params ports.ListRefsParams) bool {
			return params.PageSize == maxRefs // the refs are asked in one page
		})).Return(ports.RefPage{Refs: []domain.Ref{
			{Name: "refs/heads/main", ShortName: "main", Kind: domain.RefKindBranch, CommitID: objectID(t, id)},
			{Name: "refs/tags/v1", ShortName: "v1", Kind: domain.RefKindTag, CommitID: objectID(t, id)},
		}}, nil).Times(builds)
		if commits {
			browse.EXPECT().ListCommits(mock.Anything, p, repoName(t, "x"), mock.Anything).RunAndReturn(
				func(_ context.Context, _ domain.Principal, _ domain.RepoName, params ports.ListCommitsParams) (ports.HistoryPage, error) {
					return ports.HistoryPage{Commits: []domain.Commit{{ID: objectID(t, params.Revision.String()), Summary: "subject"}}}, nil
				}).Times(builds)
		}
	}
	repos.EXPECT().View(mock.Anything, mock.Anything).Return(publicRepo(t, "x"), nil).Times(anonymous + owner)
	return newTestServer(t, Config{Repos: repos, Browse: browse})
}

// A push is rendered once for the watchers of the same class, and the owner
// and the anonymous visitors never share what was built.
func TestLiveRefsSharesAPushBetweenWatchers(t *testing.T) {
	t.Parallel()
	owner := domain.ContextWithPrincipal(context.WithValue(context.Background(), signedInKey{}, struct{}{}), domain.Owner())
	anon := context.Background()

	for _, tt := range []struct {
		name            string
		ctxs            []context.Context
		anonymous, mine int // builds: one per class
	}{
		{"five anonymous watchers", []context.Context{anon, anon, anon, anon, anon}, 1, 0},
		{"owner and anonymous", []context.Context{anon, owner, anon, owner}, 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := refsMocks(t, tt.anonymous, tt.mine, true)
			ev := ports.WatchEvent{ID: "b-1", Event: domain.PushEvent{Repo: repoName(t, "x")}}
			for _, ctx := range tt.ctxs {
				st := &liveStream{s: s, repo: "x", view: "refs"}
				frags, err := st.render(ctx, ev)
				require.NoError(t, err)
				require.Len(t, frags, 2)
				assert.Equal(t, "refs-list", frags[0].event)
				assert.Contains(t, frags[0].html, `id="ref-rows"`)
			}
		})
	}
}

// The refs are asked in one page of up to maxRefs (the mock only answers that).
func TestLoadRepoAsksTheRefsInOnePage(t *testing.T) {
	t.Parallel()
	s := refsMocks(t, 1, 0, false)
	d, err := s.loadRepo(context.Background(), "x")
	require.NoError(t, err)
	assert.Len(t, d.branches, 1)
	assert.Len(t, d.tags, 1)
}
