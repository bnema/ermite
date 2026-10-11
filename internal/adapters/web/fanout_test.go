// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

func frag(s string) []fragment { return []fragment{{event: "refs-list", html: s}} }

func TestFanoutBuildsOncePerKey(t *testing.T) {
	srvA, srvB := &Server{}, &Server{}
	base := fanKey{srv: srvA, repo: "x", event: "b-1"}
	for _, tt := range []struct {
		name  string
		keys  []fanKey
		built int32
	}{
		{"same key", []fanKey{base, base, base}, 1},
		{"owner and anonymous never share", []fanKey{base, {srv: srvA, repo: "x", event: "b-1", owner: true}}, 2},
		{"another event", []fanKey{base, {srv: srvA, repo: "x", event: "b-2"}}, 2},
		{"another repository", []fanKey{base, {srv: srvA, repo: "y", event: "b-1"}}, 2},
		{"another server", []fanKey{base, {srv: srvB, repo: "x", event: "b-1"}}, 2},
		{"no event id: nothing to share by", []fanKey{{srv: srvA, repo: "x"}, {srv: srvA, repo: "x"}}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
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

func TestFanoutConcurrentWatchersShareOneBuild(t *testing.T) {
	var f fanout
	var built atomic.Int32
	release := make(chan struct{})
	key := fanKey{srv: &Server{}, repo: "x", event: "b-1"}
	var wg sync.WaitGroup
	results := make([][]fragment, 50)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = f.do(context.Background(), key, func() ([]fragment, error) {
				built.Add(1)
				<-release
				return frag("once"), nil
			})
		}()
	}
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), built.Load())
	for _, r := range results {
		assert.Equal(t, frag("once"), r)
	}
}

func TestFanoutFailureIsNotKept(t *testing.T) {
	var f fanout
	key := fanKey{srv: &Server{}, repo: "x", event: "b-1"}
	_, err := f.do(context.Background(), key, func() ([]fragment, error) { return nil, errors.New("boom") })
	require.Error(t, err)
	got, err := f.do(context.Background(), key, func() ([]fragment, error) { return frag("ok"), nil })
	require.NoError(t, err)
	assert.Equal(t, frag("ok"), got, "the next watcher builds again")
}

func TestFanoutWaiterStopsWithItsContext(t *testing.T) {
	var f fanout
	key := fanKey{srv: &Server{}, repo: "x", event: "b-1"}
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
	var f fanout
	srv := &Server{}
	for i := range fanMax + 10 {
		key := fanKey{srv: srv, repo: "x", event: "b-" + string(rune('A'+i%26)) + string(rune('a'+i/26))}
		_, err := f.do(context.Background(), key, func() ([]fragment, error) { return frag("x"), nil })
		require.NoError(t, err)
	}
	assert.LessOrEqual(t, len(f.m), fanMax)
}

// refsAPI counts the calls the refs fragment makes to the API.
type refsAPI struct {
	ermiteconnect.UnimplementedRepoServiceHandler
	ermiteconnect.UnimplementedGitServiceHandler
	repo, refs, commits atomic.Int32
	pageSizes           []int32
	mu                  sync.Mutex
}

func (a *refsAPI) GetRepo(_ context.Context, req *connect.Request[pb.GetRepoRequest]) (*connect.Response[pb.GetRepoResponse], error) {
	a.repo.Add(1)
	return connect.NewResponse(&pb.GetRepoResponse{Repo: &pb.Repo{Name: req.Msg.GetRepo(), DefaultBranch: "main"}}), nil
}

func (a *refsAPI) ListRefs(_ context.Context, req *connect.Request[pb.ListRefsRequest]) (*connect.Response[pb.ListRefsResponse], error) {
	a.refs.Add(1)
	a.mu.Lock()
	a.pageSizes = append(a.pageSizes, req.Msg.GetPageSize())
	a.mu.Unlock()
	id := "0123456789abcdef0123456789abcdef01234567"
	return connect.NewResponse(&pb.ListRefsResponse{Refs: []*pb.Ref{
		{Name: "refs/heads/main", ShortName: "main", Kind: pb.RefKind_REF_KIND_BRANCH, CommitId: id},
		{Name: "refs/tags/v1", ShortName: "v1", Kind: pb.RefKind_REF_KIND_TAG, CommitId: id},
	}}), nil
}

func (a *refsAPI) ListCommits(_ context.Context, req *connect.Request[pb.ListCommitsRequest]) (*connect.Response[pb.ListCommitsResponse], error) {
	a.commits.Add(1)
	return connect.NewResponse(&pb.ListCommitsResponse{Commits: []*pb.Commit{{Id: req.Msg.GetRevision(), Summary: "subject"}}}), nil
}

func refsServer(t *testing.T, api *refsAPI) *Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(ermiteconnect.NewRepoServiceHandler(api))
	mux.Handle(ermiteconnect.NewGitServiceHandler(api))
	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)
	s, err := New(Config{
		Repos:     ermiteconnect.NewRepoServiceClient(backend.Client(), backend.URL),
		Git:       ermiteconnect.NewGitServiceClient(backend.Client(), backend.URL),
		PublicURL: "https://forge.example", SourceURL: "https://forge.example/ermite", Version: "v1",
	})
	require.NoError(t, err)
	return s
}

// A push is rendered once for the watchers of the same class, and the owner
// and the anonymous visitors never share what was built.
func TestLiveRefsSharesAPushBetweenWatchers(t *testing.T) {
	api := &refsAPI{}
	s := refsServer(t, api)
	owner := context.WithValue(context.Background(), signedInKey{}, struct{}{})
	anon := context.Background()

	for _, tt := range []struct {
		name                  string
		ctxs                  []context.Context
		wantRepo, wantCommits int32
	}{
		{"five anonymous watchers", []context.Context{anon, anon, anon, anon, anon}, 1, 1},
		{"owner and anonymous", []context.Context{anon, owner, anon, owner}, 2, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api.repo.Store(0)
			api.commits.Store(0)
			ev := &pb.Event{Id: "b-" + tt.name, Kind: &pb.Event_Pushed{Pushed: &pb.Pushed{Repo: "x"}}}
			for _, ctx := range tt.ctxs {
				st := &liveStream{s: s, repo: "x", view: "refs"}
				frags, err := st.render(ctx, ev)
				require.NoError(t, err)
				require.Len(t, frags, 2)
				assert.Equal(t, "refs-list", frags[0].event)
				assert.Contains(t, frags[0].html, `id="ref-rows"`)
			}
			assert.Equal(t, tt.wantRepo, api.repo.Load(), "loadRepo")
			assert.Equal(t, tt.wantCommits, api.commits.Load(), "one ListCommits per distinct commit and class")
		})
	}
}

// The refs are asked in one page of up to maxRefs.
func TestLoadRepoAsksTheRefsInOnePage(t *testing.T) {
	api := &refsAPI{}
	s := refsServer(t, api)
	d, err := s.loadRepo(context.Background(), "x")
	require.NoError(t, err)
	assert.Len(t, d.branches, 1)
	assert.Len(t, d.tags, 1)
	assert.Equal(t, int32(1), api.refs.Load())
	assert.Equal(t, []int32{maxRefs}, api.pageSizes)
}
