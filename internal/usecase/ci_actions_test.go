// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func TestParseActionRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		uses                         string
		owner, repo, path, ref, fail string
	}{
		{uses: "actions/checkout@v4", owner: "actions", repo: "checkout", ref: "v4"},
		{uses: "actions/cache/save@v4", owner: "actions", repo: "cache", path: "save", ref: "v4"},
		{uses: "o/r/a/b/c@feature/x", owner: "o", repo: "r", path: "a/b/c", ref: "feature/x"},
		{uses: "o\\r\\p@v1", owner: "o", repo: "r", path: "p", ref: "v1"}, // GitHub splits on both separators
		{uses: "o//r@v1", owner: "o", repo: "r", ref: "v1"},               // and ignores empty segments
		{uses: "noslash@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'noslash@v1'"},
		{uses: "o/r", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'o/r'"},
		{uses: "o/r@", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'o/r@'"},
		{uses: "@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual '@v1'"},
		{uses: "o/r@a@b", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'o/r@a@b'"},
		{uses: "o/r/../x@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'o/r/../x@v1'"},
		{uses: "o/r/./x@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'o/r/./x@v1'"},
		{uses: "a/...@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'a/...@v1'"},
		{uses: "../..@" + strings.Repeat("a", 40), fail: "Expected format {org}/{repo}[/path]@ref. Actual '../..@" + strings.Repeat("a", 40) + "'"},
		{uses: "a%2f/b@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'a%2f/b@v1'"},
		{uses: "a/b?x@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'a/b?x@v1'"},
		{uses: "a/b#x@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'a/b#x@v1'"},
		{uses: "-a/_b.c@v1", owner: "-a", repo: "_b.c", ref: "v1"},
		{uses: "", fail: "Expected format {org}/{repo}[/path]@ref. Actual ''"},
	}
	for _, tt := range tests {
		t.Run(tt.uses, func(t *testing.T) {
			t.Parallel()
			a := parseActionRef(tt.uses)
			assert.Equal(t, tt.fail, a.err)
			if tt.fail == "" {
				assert.Equal(t, []string{tt.owner, tt.repo, tt.path, tt.ref}, []string{a.owner, a.repo, a.path, a.ref})
			}
		})
	}
	assert.Equal(t, parseActionRef("Actions/Checkout@v4").key(), parseActionRef("actions/checkout@v4").key(), "names are case-insensitive")
	assert.NotEqual(t, parseActionRef("a/b@V4").key(), parseActionRef("a/b@v4").key(), "refs are not")
}

func TestPickRemoteRef(t *testing.T) {
	t.Parallel()
	refs := []ports.RemoteRef{
		{Name: "refs/heads/v1", ID: "b1"},
		{Name: "refs/tags/v1", ID: "t1"},
		{Name: "refs/tags/v2", ID: "tobj", Peeled: "c2"},
		{Name: "refs/heads/main", ID: "m"},
	}
	assert.Equal(t, "t1", pickRemoteRef(refs, "v1"), "a tag before a branch")
	assert.Equal(t, "c2", pickRemoteRef(refs, "v2"), "the peeled commit of an annotated tag")
	assert.Equal(t, "m", pickRemoteRef(refs, "main"))
	assert.Empty(t, pickRemoteRef(refs, "refs/heads/main"), "short names only")
	assert.Empty(t, pickRemoteRef(refs, "nope"))
	assert.Empty(t, pickRemoteRef(nil, "v1"))
}

// countingRemote answers ListRefs from a function and counts the calls.
type countingRemote struct {
	calls  int
	answer func() ([]ports.RemoteRef, error)
}

func (r *countingRemote) ListRefs(context.Context, string, string) ([]ports.RemoteRef, error) {
	r.calls++
	return r.answer()
}

// The refs of a github.com repository are listed once for the jobs of the next
// minutes; when github.com does not answer, the last list is used.
func TestRemoteRefsCache(t *testing.T) {
	v4 := []ports.RemoteRef{{Name: "refs/tags/v4", ID: strings.Repeat("a", 40)}}
	remote := &countingRemote{answer: func() ([]ports.RemoteRef, error) { return v4, nil }}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := &CI{cfg: CIConfig{Remote: remote}, remoteSlots: make(chan struct{}, 1), remoteCache: newRefsCache()}
	list := func() ([]ports.RemoteRef, error) {
		r := c.newRemoteRefs(1)
		r.now = func() time.Time { return now }
		return r.list(context.Background(), "Actions", "checkout")
	}

	got, err := list()
	require.NoError(t, err)
	assert.Equal(t, v4, got)
	now = now.Add(refsFresh - time.Second)
	_, err = list()
	require.NoError(t, err)
	assert.Equal(t, 1, remote.calls, "a fresh list is not asked again, whatever the case of the name")
	rerun := c.newRemoteRefs(2)
	rerun.now = func() time.Time { return now }
	_, err = rerun.list(context.Background(), "actions", "checkout")
	require.NoError(t, err)
	assert.Equal(t, 2, remote.calls, "a re-run asks again")

	now = now.Add(refsFresh + time.Second) // the re-run listed it afresh
	remote.answer = func() ([]ports.RemoteRef, error) { return nil, fmt.Errorf("%w: timeout", domain.ErrUnavailable) }
	got, err = list()
	require.NoError(t, err, "github.com did not answer: the last list is used")
	assert.Equal(t, v4, got)
	assert.Equal(t, 3, remote.calls, "an old list is asked again")

	now = now.Add(refsStale)
	_, err = list()
	require.ErrorIs(t, err, domain.ErrUnavailable, "too old to stand in")

	remote.answer = func() ([]ports.RemoteRef, error) { return nil, domain.ErrNotFound }
	c.remoteCache = newRefsCache()
	_, err = list()
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = list()
	require.ErrorIs(t, err, domain.ErrNotFound)
	assert.Equal(t, 6, remote.calls, "a missing repository is not cached")
}

// A stale list stands in without becoming younger, the oldest repository makes
// room for a new one, and the cache is safe for jobs resolving at once.
func TestRefsCacheBounds(t *testing.T) {
	v4 := []ports.RemoteRef{{Name: "refs/tags/v4", ID: strings.Repeat("a", 40)}}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	remote := &countingRemote{answer: func() ([]ports.RemoteRef, error) { return v4, nil }}
	c := &CI{cfg: CIConfig{Remote: remote}, remoteSlots: make(chan struct{}, 1), remoteCache: newRefsCache()}
	list := func(repo string) error {
		r := c.newRemoteRefs(1)
		r.now = func() time.Time { return now }
		_, err := r.list(context.Background(), "actions", repo)
		return err
	}
	require.NoError(t, list("checkout"))
	remote.answer = func() ([]ports.RemoteRef, error) { return nil, domain.ErrUnavailable }
	now = now.Add(refsStale - time.Hour)
	require.NoError(t, list("checkout"), "stands in")
	now = now.Add(time.Hour)
	require.ErrorIs(t, list("checkout"), domain.ErrUnavailable, "standing in did not make it younger")

	cache := newRefsCache()
	for i := range refsMax {
		cache.put(fmt.Sprintf("o/r%d", i), v4, now.Add(time.Duration(i)*time.Second))
	}
	cache.put("o/new", v4, now.Add(time.Hour))
	_, ok := cache.stale("o/r0", now)
	assert.False(t, ok, "the oldest is evicted")
	_, ok = cache.stale("o/r1", now.Add(time.Second))
	assert.True(t, ok)
	assert.Len(t, cache.m, refsMax)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			key := fmt.Sprintf("o/r%d", i%3)
			cache.put(key, v4, now)
			cache.get(key, now)
			cache.stale(key, now)
		})
	}
	wg.Wait()
}

// An unanswered ls-remote fails the call with the action named.
func TestResolveActionNamesTheActionGitHubDidNotAnswer(t *testing.T) {
	remote := &countingRemote{answer: func() ([]ports.RemoteRef, error) {
		return nil, fmt.Errorf("%w: git ls-remote actions/checkout did not answer within 20s (2 attempts)", domain.ErrUnavailable)
	}}
	c := &CI{cfg: CIConfig{Remote: remote, Owner: "owner", Clock: stillClock{}}, remoteSlots: make(chan struct{}, 1), remoteCache: newRefsCache()}
	_, _, err := c.resolveAction(context.Background(), domain.CIJob{}, parseActionRef("actions/checkout@v4"), c.newRemoteRefs(1), pusherAccess{})
	require.ErrorIs(t, err, domain.ErrUnavailable)
	assert.Contains(t, err.Error(), "actions/checkout@v4")
	assert.Contains(t, err.Error(), "github.com did not answer")
}

type stillClock struct{}

func (stillClock) Now() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func TestPrincipalOfPusherBotIsBoundToItsRepository(t *testing.T) {
	t.Parallel()
	home, err := domain.ParseRepoName("home")
	require.NoError(t, err)
	other, err := domain.ParseRepoName("other")
	require.NoError(t, err)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	by := domain.PusherOf(domain.Bot(home))

	p, ok, err := principalOfPusher(context.Background(), nil, by, home, now)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, domain.Bot(home), p)

	p, ok, err = principalOfPusher(context.Background(), nil, by, other, now)
	require.NoError(t, err)
	assert.False(t, ok, "a record of another repository is void")
	assert.Equal(t, domain.Principal{}, p)
}

// A bot pusher reads nothing of another private repository, so an action of
// that repository is not resolved on its behalf.
func TestPusherCanReadBotCannotReadAnotherPrivateRepository(t *testing.T) {
	t.Parallel()
	home, err := domain.ParseRepoName("home")
	require.NoError(t, err)
	secret, err := domain.ParseRepoName("secret")
	require.NoError(t, err)
	c := &CI{cfg: CIConfig{Clock: stillClock{}}}
	by := pusherAccess{pusher: domain.PusherOf(domain.Bot(home))}

	ok, err := c.pusherCanRead(context.Background(), by, home, domain.Repo{Name: secret, Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	assert.False(t, ok, "another private repository")
	ok, err = c.pusherCanRead(context.Background(), by, home, domain.Repo{Name: secret, Visibility: domain.VisibilityPublic})
	require.NoError(t, err)
	assert.True(t, ok, "a public one")
	ok, err = c.pusherCanRead(context.Background(), by, home, domain.Repo{Name: home, Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	assert.True(t, ok, "its own")

	// A record bound to another repository than the run's reads nothing.
	forged := pusherAccess{pusher: domain.PusherOf(domain.Bot(secret))}
	ok, err = c.pusherCanRead(context.Background(), forged, home, domain.Repo{Name: secret, Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	assert.False(t, ok)
}
