// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"fmt"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// SetHold installs the hold of the branch heads after construction (Bernard is
// built after CI); nil turns it off. While a head is held, its push workflows
// and the pull_request runs of the merge requests it is the head of do not
// start; everything else a push does (refs of the requests, events) goes on.
func (c *CI) SetHold(h ports.CIHold) {
	c.mu.Lock()
	c.hold = h
	c.mu.Unlock()
}

func (c *CI) holder() ports.CIHold {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hold
}

// held reports whether the head of a branch is held.
func (c *CI) held(ctx context.Context, repo domain.RepoName, branch string, sha domain.ObjectID) bool {
	h := c.holder()
	if h == nil || branch == "" || sha.IsZero() || !h.Held(ctx, repo, branch, sha) {
		return false
	}
	ctxLog(ctx).Debug().Str("repo", repo.String()).Str("branch", branch).Str("sha", sha.String()).Msg("ci: the head is held: its runs wait for the release")
	return true
}

// heldRef reports whether a pushed branch is set to a held head.
func (c *CI) heldRef(ctx context.Context, repo domain.RepoName, u domain.RefUpdate) bool {
	branch, ok := strings.CutPrefix(u.Ref, "refs/heads/")
	if !ok {
		return false
	}
	sha, err := domain.ParseObjectID(u.New)
	return err == nil && c.held(ctx, repo, branch, sha)
}

// heldPull reports whether the runs of a merge request activity wait for the
// release of its head (opened, synchronize, reopened; a closing runs anyway).
func (c *CI) heldPull(ctx context.Context, e pullEvent, mr domain.MergeRequest, st pullState) bool {
	if e.release {
		return false
	}
	switch e.action {
	case domain.PullOpened, domain.PullSynchronize, domain.PullReopened:
		return c.held(ctx, e.repo, mr.Source, st.head)
	}
	return false
}

// ReleaseHold starts the runs a held head did not start: the push workflows of
// a push of sha on branch, and the pull_request synchronize runs of the open
// merge requests from that branch. The runs are attributed to whoever the
// record of the branch says set it to sha (untrusted without one). Nothing
// starts when the branch is no longer at sha: the push that moved it started
// its own runs. created replays the push as the creation of the branch (its
// create workflows run too). Asynchronous, as a push: it returns once the work
// is queued.
func (c *CI) ReleaseHold(ctx context.Context, repo domain.RepoName, branch string, sha domain.ObjectID, created bool) error {
	head, err := c.cfg.Git.BranchHead(ctx, repo, branch)
	if err != nil {
		return fmt.Errorf("read the head of %s: %w", branch, err)
	}
	if head != sha {
		return nil
	}
	// The push replayed is the creation, or the one from the first parent, as
	// a fast-forward of one commit; a root commit is a creation.
	old := domain.ZeroOID
	if cs, _, err := c.cfg.Git.ListRange(ctx, repo, domain.ObjectID{}, sha, 1); !created && err == nil && len(cs) == 1 && len(cs[0].Parents) > 0 {
		old = cs[0].Parents[0].String()
	}
	updates := []domain.RefUpdate{{Ref: "refs/heads/" + branch, Old: old, New: sha.String()}}
	actor, pusher, trusted := c.scheduleActor(ctx, repo, branch, sha)
	done := c.pushLifecycle(ctx, repo, 2)
	defer done()
	defer c.onBranchesMoved(ctx, repo, updates, actor, pusher, trusted, true, true)
	c.enqueuePush(ctx, repo, updates, actor, pusher, trusted, done, true)
	return nil
}
