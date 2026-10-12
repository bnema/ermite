// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/rpc"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// watchWait is how long a test waits for an event that must arrive. Only
// TestAC5PushEventWithinASecond holds the server to a second.
const watchWait = 10 * time.Second

// AC5: a push shows up in Watch within a second, with the ref updates.
func TestAC5PushEventWithinASecond(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic)
	w := h.watch(h.api(""), "", "home")

	wt := h.workTree("main", "a.txt")
	start := time.Now()
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
	ev := w.next(t, time.Second)
	assert.Less(t, time.Since(start), 2*time.Second)

	pushed := ev.GetPushed()
	require.NotNil(t, pushed, "got %v", ev)
	assert.Equal(t, "pub", pushed.GetRepo())
	require.Len(t, pushed.GetRefUpdates(), 1)
	up := pushed.GetRefUpdates()[0]
	assert.Equal(t, "refs/heads/main", up.GetRef())
	assert.Empty(t, up.GetOldId(), "a created ref has no old id")
	assert.Equal(t, h.bareGit("pub", "rev-parse", "main"), up.GetNewId())
	assert.NotEmpty(t, ev.GetId())
	assert.NotNil(t, ev.GetTime())

	// A second push to the same ref carries both ids; a deletion has no new id.
	old := up.GetNewId()
	h.writeFile(wt, "b.txt", []byte("b\n"))
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
	up2 := w.next(t, watchWait).GetPushed().GetRefUpdates()[0]
	assert.Equal(t, old, up2.GetOldId())
	assert.NotEqual(t, old, up2.GetNewId())

	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main:refs/heads/tmp")
	w.next(t, watchWait)
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), ":refs/heads/tmp")
	del := w.next(t, watchWait).GetPushed().GetRefUpdates()[0]
	assert.Equal(t, "refs/heads/tmp", del.GetRef())
	assert.NotEmpty(t, del.GetOldId())
	assert.Empty(t, del.GetNewId())
}

// AC5: creating, updating and deleting repositories through the API publish
// RepoCreated, RepoUpdated and RepoDeleted; so does a create-on-push.
func TestAC5RepoLifecycleEvents(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	root := h.api(adminToken)
	w := h.watch(root, "")

	_, err := root.repo.CreateRepo(h.ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "life", Description: "d", Visibility: pb.Visibility_VISIBILITY_PUBLIC}))
	require.NoError(t, err)
	created := w.next(t, watchWait).GetRepoCreated().GetRepo()
	require.NotNil(t, created)
	assert.Equal(t, "life", created.GetName())
	assert.Equal(t, pb.Visibility_VISIBILITY_PUBLIC, created.GetVisibility())
	assert.Equal(t, "main", created.GetDefaultBranch())

	_, err = root.repo.UpdateRepo(h.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "life", Description: "new", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
	}))
	require.NoError(t, err)
	updated := w.next(t, watchWait).GetRepoUpdated().GetRepo()
	require.NotNil(t, updated)
	assert.Equal(t, "new", updated.GetDescription())

	_, err = root.repo.DeleteRepo(h.ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: "life"}))
	require.NoError(t, err)
	assert.Equal(t, "life", w.next(t, watchWait).GetRepoDeleted().GetRepo())

	// Create on first push publishes RepoCreated once, then the push.
	wt := h.workTree("main", "a.txt")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/fresh.git"), "main")
	assert.Equal(t, "fresh", w.next(t, watchWait).GetRepoCreated().GetRepo().GetName())
	assert.Equal(t, "fresh", w.next(t, watchWait).GetPushed().GetRepo())
	// Nothing else came in between: the next event is the sentinel.
	h.sentinel(t, w, "s-life")
}

// AC5: resume from an id replays what came after, then goes live.
func TestAC5Resume(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic)
	api := h.api("")
	w := h.watch(api, "")

	wt := h.workTree("main", "a.txt")
	var ids []string
	for i := range 4 {
		if i > 0 {
			h.writeFile(wt, fmt.Sprintf("f%d.txt", i), []byte("x\n"))
		}
		h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
		ids = append(ids, w.next(t, watchWait).GetId())
	}
	w.cancel()

	// The client lost its connection after ids[1]: it gets 2 and 3, then live.
	r := h.watch(api, ids[1])
	for _, want := range ids[2:] {
		ev := r.next(t, watchWait)
		assert.Equal(t, want, ev.GetId())
		assert.NotNil(t, ev.GetPushed())
	}
	// Nothing was replayed twice: the next event is the live one, below.
	h.writeFile(wt, "live.txt", []byte("x\n"))
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
	live := r.next(t, watchWait)
	assert.NotNil(t, live.GetPushed())
	assert.NotContains(t, ids, live.GetId())

	// An id nobody issued is a gap, followed by the oldest buffered event: the
	// creation of the repository, which came before the first push.
	g := h.watch(api, "not-an-id")
	assert.NotNil(t, g.next(t, watchWait).GetGap())
	assert.Equal(t, "pub", g.next(t, watchWait).GetRepoCreated().GetRepo().GetName())
	assert.Equal(t, ids[0], g.next(t, watchWait).GetId())
}

// AC5: a gap when the id fell out of a small ring.
func TestAC5GapAfterOverflow(t *testing.T) {
	h := newHarness(t, harnessOpts{eventBuffer: 4})
	h.seed("pub", domain.VisibilityPublic)
	api := h.api(adminToken)
	w := h.watch(api, "")
	wt := h.workTree("main", "a.txt")

	var ids []string
	for i := range 8 {
		if i > 0 {
			h.writeFile(wt, fmt.Sprintf("f%d.txt", i), []byte("x\n"))
		}
		h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
		ids = append(ids, w.next(t, watchWait).GetId())
	}

	// ids[2] is out of the ring (the last four are ids[4:]).
	r := h.watch(api, ids[2])
	gap := r.next(t, watchWait)
	assert.NotNil(t, gap.GetGap())
	assert.Empty(t, gap.GetId())
	for _, want := range ids[4:] {
		assert.Equal(t, want, r.next(t, watchWait).GetId())
	}

	// The last buffered id still resumes without a gap.
	ok := h.watch(api, ids[4])
	assert.Equal(t, ids[5], ok.next(t, watchWait).GetId())
}

// AC5: private events never reach anonymous callers, nor tokens that may not
// read the repository; repo topics follow the same rule.
func TestAC5NoPrivateEventsToAnonymous(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic)
	h.seed("priv", domain.VisibilityPrivate)
	other := h.token(scopes(domain.ScopeRepoRead), "pub")
	reader := h.token(scopes(domain.ScopeRepoRead), "priv")

	anon := h.watch(h.api(""), "")
	scoped := h.watch(h.api(other), "")
	rd := h.watch(h.api(reader), "")
	root := h.watch(h.api(adminToken), "")

	// A private repository's topic is not found for those who cannot read it.
	assert.Equal(t, "not_found", codeName(watchErr(h.ctx, h.api(""), "repo:priv")))
	assert.Equal(t, "not_found", codeName(watchErr(h.ctx, h.api(other), "repo:priv")))
	assert.Equal(t, codeName(watchErr(h.ctx, h.api(""), "repo:nothing")), codeName(watchErr(h.ctx, h.api(""), "repo:priv")))

	wt := h.workTree("main", "a.txt")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/priv.git"), "main")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
	// Metadata events too.
	_, err := h.api(adminToken).repo.UpdateRepo(h.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "priv", Description: "secret plans", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
	}))
	require.NoError(t, err)

	// The reader of priv sees its push and its update, and pub's push.
	assert.Equal(t, "priv", rd.next(t, watchWait).GetPushed().GetRepo())
	assert.Equal(t, "pub", rd.next(t, watchWait).GetPushed().GetRepo(), "pub is public")
	assert.Equal(t, "secret plans", rd.next(t, watchWait).GetRepoUpdated().GetRepo().GetDescription())
	// Root sees everything in order.
	assert.Equal(t, "priv", root.next(t, watchWait).GetPushed().GetRepo())
	assert.Equal(t, "pub", root.next(t, watchWait).GetPushed().GetRepo())
	assert.Equal(t, "priv", root.next(t, watchWait).GetRepoUpdated().GetRepo().GetName())

	// Deleting reaches those who could read the repository before, not others.
	_, err = h.api(adminToken).repo.DeleteRepo(h.ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: "priv"}))
	require.NoError(t, err)
	assert.Equal(t, "priv", rd.next(t, watchWait).GetRepoDeleted().GetRepo())
	assert.Equal(t, "priv", root.next(t, watchWait).GetRepoDeleted().GetRepo())

	for name, w := range map[string]*stream{"anonymous": anon, "scoped": scoped} {
		ev := w.next(t, watchWait)
		assert.Equal(t, "pub", ev.GetPushed().GetRepo(), name)
	}
	// Nothing about priv came before the sentinel, for either of them.
	_, err = h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name("sentinel"), Visibility: domain.VisibilityPublic})
	require.NoError(t, err)
	for name, w := range map[string]*stream{"anonymous": anon, "scoped": scoped} {
		assert.Equal(t, "sentinel", w.next(t, watchWait).GetRepoCreated().GetRepo().GetName(), name)
	}

	// Resuming does not replay private events to anonymous either: after a
	// gap, the creation and the push of pub, the sentinels the watchers above
	// saw, and nothing about priv.
	r := h.watch(h.api(""), "bogus")
	assert.NotNil(t, r.next(t, watchWait).GetGap())
	assert.Equal(t, "pub", r.next(t, watchWait).GetRepoCreated().GetRepo().GetName())
	assert.Equal(t, "pub", r.next(t, watchWait).GetPushed().GetRepo())
	assert.Equal(t, "sentinel", r.next(t, watchWait).GetRepoCreated().GetRepo().GetName())
	h.sentinel(t, r, "s-resumed")
}

// AC5: a repo topic only carries that repository; a revoked token cannot
// start a new watch.
func TestAC5RepoTopic(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("a", domain.VisibilityPublic)
	h.seed("b", domain.VisibilityPublic)
	w := h.watch(h.api(""), "", "repo:a")

	wt := h.workTree("main", "x.txt")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/b.git"), "main")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/a.git"), "main")
	assert.Equal(t, "a", w.next(t, watchWait).GetPushed().GetRepo())
	// The push to b was not delivered: the next event is the next push to a.
	h.writeFile(wt, "y.txt", []byte("y\n"))
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/a.git"), "main")
	assert.Equal(t, h.bareGit("a", "rev-parse", "main"), w.next(t, watchWait).GetPushed().GetRefUpdates()[0].GetNewId())
}

// The stream ends cleanly when the client cancels, and Watch counts go back to zero.
func TestWatchEndsOnCancel(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	w := h.watch(h.api(""), "")
	require.Eventually(t, func() bool { return h.forge.EventLog.Watchers() == 1 }, 5*time.Second, time.Millisecond)
	w.cancel()
	select {
	case <-w.done:
	case <-time.After(5 * watchWait):
		t.Fatal("the stream did not end")
	}
	require.Eventually(t, func() bool { return h.forge.EventLog.Watchers() == 0 }, 5*time.Second, time.Millisecond)
}

// Shutdown ends the streams cleanly: the client sees the stream end without
// an error and the server exits 0 well before the shutdown timeout.
func TestServeShutdownEndsWatchStreams(t *testing.T) {
	requireGit(t)
	data := t.TempDir()
	addr := freeAddr(t)
	cmd := exec.Command(binary(t), "serve")
	cmd.Env = append(baseEnv(),
		"ERMITE_ADDR="+addr, "ERMITE_DATA_DIR="+data, "ERMITE_PUBLIC_URL=http://"+addr,
		"ERMITE_ADMIN_TOKEN="+adminToken, "ERMITE_LOG_LEVEL=debug", "ERMITE_LOG_FORMAT=json",
	)
	var logs syncBuffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	base := "http://" + addr
	require.Eventually(t, func() bool {
		resp, err := http.Get(base + "/-/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	}, 15*time.Second, 50*time.Millisecond, "%s", logs.String())

	c := newClients(base+"/-/api", adminToken)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := c.event.Watch(ctx, connect.NewRequest(&pb.WatchRequest{}))
	require.NoError(t, err)
	ended := make(chan error, 1)
	go func() {
		for st.Receive() {
		}
		ended <- st.Err()
	}()

	// The server version comes from the real binary.
	v, err := c.server.GetVersion(ctx, connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err)
	assert.NotEmpty(t, v.Msg.GetVersion())

	start := time.Now()
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-ended:
		assert.NoError(t, err, "the stream ends cleanly")
	case <-time.After(15 * watchWait):
		t.Fatalf("the stream did not end\n%s", logs.String())
	}
	select {
	case err := <-exited:
		require.NoError(t, err, "%s", logs.String())
	case <-time.After(15 * watchWait):
		t.Fatalf("the server did not exit\n%s", logs.String())
	}
	assert.Less(t, time.Since(start), 8*time.Second, "shutdown did not wait for its timeout")
	assert.NotContains(t, logs.String(), `"level":"error"`, "no error logged:\n%s", logs.String())
	assert.False(t, errors.Is(err, context.Canceled))
}

// A token that is revoked while its Watch is open ends the stream with
// unauthenticated, and the private events that follow never reach it.
func TestWatchEndsWhenTheTokenIsRevoked(t *testing.T) {
	h := newHarness(t, harnessOpts{revalidateEvery: 20 * time.Millisecond})
	h.seed("priv", domain.VisibilityPrivate)
	created, err := h.forge.Tokens.Create(h.ctx, domain.Root(), ports.CreateTokenParams{
		Label: "watcher", Scopes: scopes(domain.ScopeRepoRead),
	})
	require.NoError(t, err)
	w := h.watch(h.api(created.Secret), "")
	wt := h.workTree("main", "a.txt")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/priv.git"), "main")
	assert.Equal(t, "priv", w.next(t, 5*watchWait).GetPushed().GetRepo(), "the token may read priv")

	require.NoError(t, h.forge.Tokens.Revoke(h.ctx, domain.Root(), created.Token.ID))
	h.writeFile(wt, "b.txt", []byte("b\n"))
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/priv.git"), "main")

	select {
	case <-w.done:
	case <-time.After(5 * watchWait):
		t.Fatal("the stream of a revoked token did not end")
	}
	assert.Equal(t, "unauthenticated", codeName(w.err))
	for ev := range w.events {
		t.Fatalf("an event reached the revoked token: %v", ev)
	}
}

// Watch refuses too many topics, and too many streams from one client.
func TestWatchLimits(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	api := h.api(adminToken)

	topics := make([]string, app.MaxWatchTopics+1)
	for i := range topics {
		topics[i] = "home"
	}
	assert.Equal(t, "invalid_argument", codeName(watchErr(h.ctx, api, topics...)))

	for range app.MaxWatchersPerClient {
		h.watch(api, "")
	}
	require.Eventually(t, func() bool { return h.forge.EventLog.Watchers() == app.MaxWatchersPerClient }, 5*time.Second, time.Millisecond)
	assert.Equal(t, "resource_exhausted", codeName(watchErr(h.ctx, api)))
	// A stream of another client address is not affected (the harness trusts
	// the loopback proxy only when configured: use the in-process caller).
	inProc := newClientsOn(h.forge.API.InProcessClient(), rpc.InProcessURL)
	ctx, cancel := context.WithCancel(domain.ContextWithPrincipal(h.ctx, domain.Root()))
	defer cancel()
	st, err := inProc.event.Watch(ctx, connect.NewRequest(&pb.WatchRequest{}))
	require.NoError(t, err)
	_ = st.Close()
}

// sentinel proves that nothing reached the stream since the last event it
// returned: it creates a public repository, which every watcher of "home" may
// see, and requires its RepoCreated to be the next event. Events arrive in the
// order they were published.
func (h *harness) sentinel(t *testing.T, s *stream, name string) {
	t.Helper()
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name(name), Visibility: domain.VisibilityPublic})
	require.NoError(t, err)
	ev := s.next(t, watchWait)
	assert.Equal(t, name, ev.GetRepoCreated().GetRepo().GetName(), "an event came before the sentinel: %v", ev)
}
