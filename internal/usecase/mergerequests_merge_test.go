// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/sshsig"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

// gateHooks is a CI and a pull_request trigger that block on a channel, to
// tell what runs before Merge returns.
type gateHooks struct {
	release chan struct{}
	mu      sync.Mutex
	calls   []string
}

func newGateHooks() *gateHooks { return &gateHooks{release: make(chan struct{})} }

func (g *gateHooks) note(s string) {
	<-g.release
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, s)
}

func (g *gateHooks) ran() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func (g *gateHooks) OnPush(context.Context, domain.RepoName, []domain.RefUpdate, domain.Principal) {}

func (g *gateHooks) OnMerge(context.Context, domain.RepoName, []domain.RefUpdate, domain.Principal) {
	g.note("ci")
}

// OnPullRequest blocks for the closing of a request only: Create reports the
// opening on the request path.
func (g *gateHooks) OnPullRequest(_ context.Context, _ domain.RepoName, ev ports.PullRequestEvent, _ domain.Principal) {
	if ev.Action == domain.PullClosed {
		g.note("pulls")
	}
}

// gated builds merge request use cases over the rig whose CI and pull hooks
// block until g.release is closed.
func (r *mrRig) gated(g *gateHooks) *usecase.MergeRequests {
	signer, err := sshsig.New(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	require.NoError(r.t, err)
	return usecase.NewMergeRequests(usecase.MergeRequestsConfig{
		Meta: r.store, Store: r.store.MergeRequests(), Repos: r.git, Git: r.git, Signer: signer,
		Committer: domain.Identity{Name: "ermite", Email: "ermite@git.example.org"},
		Bus:       r.bus, Clock: fixedClock{}, Maint: r.sched, CI: g, PullRequests: g,
	})
}

// behind moves main on with a commit of its own (a file the feature does not
// touch), and returns the new tip.
func (r *mrRig) behind() domain.ObjectID {
	r.sh("checkout", "-q", "main")
	r.commit(author, "news.txt", "n\n", "main moved")
	r.push("main")
	return r.branchHead("main")
}

func TestMergeModesWhenBehind(t *testing.T) {
	t.Parallel()
	t.Run("fast-forward is refused", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		tip := r.behind()
		_, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "the target has moved: rebase and push again, or merge with squash or a merge commit")
		assert.Equal(t, tip, r.branchHead("main"))
	})

	t.Run("squash merges the merged tree", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		tip := r.behind()
		res, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: domain.MergeModeSquash})
		require.NoError(t, err)
		sq := res.MergedCommitID.String()
		g := func(args ...string) string { return r.run(r.bare(), r.env(author), args...) }
		assert.Equal(t, tip.String(), g("rev-parse", sq+"^"), "the current tip is the parent")
		files := g("ls-tree", "-r", "--name-only", sq)
		assert.Contains(t, files, "news.txt", "the target's own change is kept")
		assert.Contains(t, files, "c.txt", "the branch's change is there")
		r.verifyCommit(sq)
	})

	t.Run("merge commit has two parents, the merge tree and a good signature", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		tip := r.behind()
		want := r.run(r.bare(), r.env(author), "merge-tree", "--write-tree", tip.String(), head.String())
		res, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: domain.MergeModeMerge})
		require.NoError(t, err)
		assert.Equal(t, domain.MergeModeMerge, res.MergeMode)
		assert.Equal(t, tip, res.BaseID)
		id := res.MergedCommitID.String()
		assert.Equal(t, id, r.branchHead("main").String())
		g := func(args ...string) string { return r.run(r.bare(), r.env(author), args...) }
		assert.Equal(t, tip.String()+" "+head.String(), g("log", "-1", "--format=%P", id), "parents: target, head")
		assert.Equal(t, strings.TrimSpace(want), g("rev-parse", id+"^{tree}"))
		assert.Equal(t, "Merge branch 'feat' into main (!1)\n\nAdd feat", g("log", "-1", "--format=%B", id))
		assert.Equal(t, "ermite <ermite@git.example.org>", g("log", "-1", "--format=%an <%ae>", id))
		assert.Equal(t, "ermite <ermite@git.example.org>", g("log", "-1", "--format=%cn <%ce>", id))
		r.verifyCommit(id)
	})

	t.Run("merge commit when up to date", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		main := r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		res, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: domain.MergeModeMerge})
		require.NoError(t, err)
		id := res.MergedCommitID.String()
		g := func(args ...string) string { return r.run(r.bare(), r.env(author), args...) }
		assert.Equal(t, main.String()+" "+head.String(), g("log", "-1", "--format=%P", id))
		assert.Equal(t, g("rev-parse", head.String()+"^{tree}"), g("rev-parse", id+"^{tree}"))
	})
}

func TestMergeRefusedOnConflictAndNoCommonHistory(t *testing.T) {
	t.Parallel()
	conflict := func(t *testing.T) (*mrRig, domain.MergeRequest, domain.ObjectID, domain.ObjectID) {
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat") // changes line 2 of a.txt
		mr := r.open("feat")
		r.sh("checkout", "-q", "main")
		r.commit(author, "a.txt", "one\nTWO\nthree\n", "main changes a too")
		r.push("main")
		return r, mr, head, r.branchHead("main")
	}
	for _, mode := range []domain.MergeMode{domain.MergeModeFastForward, domain.MergeModeSquash, domain.MergeModeMerge} {
		t.Run("conflict "+string(mode), func(t *testing.T) {
			t.Parallel()
			r, mr, head, tip := conflict(t)
			_, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: mode})
			require.ErrorIs(t, err, domain.ErrPreconditionFailed)
			if mode != domain.MergeModeFastForward {
				assert.Contains(t, err.Error(), "conflicts with main: a.txt: rebase and push again")
			}
			assert.Equal(t, tip, r.branchHead("main"))
			d, err := r.mrs.Get(r.ctx, mrM, r.repo, mr.Number, ports.GetMergeRequestOptions{})
			require.NoError(t, err)
			assert.Equal(t, domain.MRStateOpen, d.MergeRequest.State)
		})
	}

	for _, mode := range []domain.MergeMode{domain.MergeModeSquash, domain.MergeModeMerge} {
		t.Run("no common history "+string(mode), func(t *testing.T) {
			t.Parallel()
			r := newMRRig(t)
			r.seed()
			r.sh("checkout", "-q", "--orphan", "island")
			r.commit(author, "island.txt", "i\n", "an unrelated root")
			r.push("island")
			head := r.branchHead("island")
			mr, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "island", Source: "island"})
			if err != nil {
				t.Skipf("a request without common history cannot be opened: %v", err)
			}
			tip := r.branchHead("main")
			_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: mode})
			require.ErrorIs(t, err, domain.ErrPreconditionFailed)
			assert.Contains(t, err.Error(), "no common history")
			assert.Equal(t, tip, r.branchHead("main"))
		})
	}
}

func TestMergeReturnsBeforeTheSideEffects(t *testing.T) {
	t.Parallel()
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	r.drain()
	g := newGateHooks()
	mrs := r.gated(g)

	done := make(chan error, 1)
	go func() {
		_, err := mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: true})
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err, "Merge does not wait for the blocked hooks")
	case <-time.After(10 * time.Second):
		t.Fatal("Merge waited for the side effects")
	}
	// The merge is complete and recorded though the hooks have not run.
	d, err := mrs.Get(r.ctx, mrM, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, domain.MRStateMerged, d.MergeRequest.State)
	assert.Equal(t, d.MergeRequest.MergedCommitID, r.branchHead("main"))
	assert.Empty(t, g.ran())

	// Wait waits for them: it times out while they are blocked, then returns.
	ctx, cancel := context.WithTimeout(r.ctx, 50*time.Millisecond)
	start := time.Now()
	mrs.Wait(ctx)
	cancel()
	assert.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond, "Wait waits for the pending side effects")

	waited := make(chan struct{})
	go func() { mrs.Wait(context.Background()); close(waited) }()
	select {
	case <-waited:
		t.Fatal("Wait returned with side effects pending")
	case <-time.After(50 * time.Millisecond):
	}
	close(g.release)
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after the side effects ended")
	}
	assert.ElementsMatch(t, []string{"ci", "pulls"}, g.ran())
	_, err = r.git.BranchHead(r.ctx, r.repo, "feat")
	assert.ErrorIs(t, err, domain.ErrNotFound, "the source was deleted")

	// Merged, then pushed, with the deletion in the push.
	evs := r.drain()
	require.Len(t, evs, 2)
	assert.IsType(t, domain.MergeRequestMerged{}, evs[0])
	push, ok := evs[1].(domain.PushEvent)
	require.True(t, ok)
	assert.Len(t, push.Updates, 2)
}

func TestMergeReleasesTheLockBeforeTheSideEffects(t *testing.T) {
	t.Parallel()
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	r.sh("checkout", "-q", "main")
	other := r.openFeature("other")
	g := newGateHooks()
	defer func() {
		close(g.release)
	}()
	mrs := r.gated(g)

	_, err := mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
	require.NoError(t, err)
	t.Cleanup(func() { mrs.Wait(context.Background()) })
	require.Empty(t, g.ran(), "the side effects are still blocked")

	// Create and another Merge need the repository lock: they must finish.
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()
	r.sh("checkout", "-q", "-B", "third", "main")
	r.commit(author, "third.txt", "t\n", "third")
	r.push("third")
	_, err = mrs.Create(ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "third", Source: "third"})
	require.NoError(t, err, "Create is not blocked by the side effects of a merge")
	_, err = mrs.Merge(ctx, mrM, r.repo, other.Number, ports.MergeParams{HeadID: r.branchHead("other"), Mode: domain.MergeModeMerge})
	require.NoError(t, err, "a second merge is not blocked either")
	assert.Empty(t, g.ran())
}
