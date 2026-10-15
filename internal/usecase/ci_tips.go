// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Who moves a branch or a tag (CI-3, D1; domain.CIRefTip). A scheduled run has
// no pusher of its own: like GitHub's, it is the run of the last commit of the
// default branch, and it takes the actor and the trust of whoever moved that
// branch to that commit. A dispatched run is capped by the trust of the ref it
// runs on. Without this a token limited to some branches could push the first
// branch of an empty repository (which becomes the default branch) with a cron
// workflow, and have it run as the owner with the owner's secrets.
//
// The record is written BEFORE the ref moves: by the pre-receive decision of a
// push (Pushes.Check) and by a merge before it updates the target, under the
// merge lock. Written after, a delayed write could leave an earlier pusher's
// record on a tip moved back to the same commit (owner A, agent B, agent A).
// Writing it is part of the decision: when it fails the push or the merge is
// refused, never let through unrecorded. A push that is refused later leaves a
// record that names an object the ref does not have, which is not trusted.

// recordRefTips stores who is about to move the branches and tags of a push or a
// merge, and removes the records of the refs it deletes. actor is what the runs
// show, pusher and trusted are as for a push run. refs/pull/* is ermite's own
// and has none.
func recordRefTips(ctx context.Context, tips ports.CIRefTipStore, repo domain.RepoName, updates []domain.RefUpdate, actor string, pusher domain.CIPusher, trusted bool) error {
	if tips == nil {
		return nil
	}
	for _, u := range updates {
		if !strings.HasPrefix(u.Ref, "refs/heads/") && !strings.HasPrefix(u.Ref, "refs/tags/") {
			continue
		}
		var err error
		if u.IsDelete() {
			err = tips.DeleteRefTip(ctx, repo, u.Ref)
		} else {
			err = tips.PutRefTip(ctx, domain.CIRefTip{Repo: repo, Ref: u.Ref, Sha: u.New, Actor: actor, Pusher: pusher, Trusted: trusted})
		}
		if errors.Is(err, domain.ErrNotFound) {
			continue // the repository is gone: nothing to record, nothing to run
		}
		if err != nil {
			return fmt.Errorf("record who pushes %s: %w", u.Ref, err)
		}
	}
	return nil
}

// refTip returns the record of a ref when it names the object the ref has now;
// ok is false when there is none, it is for another object, or it cannot be
// read (logged): the caller then treats the ref as not trusted.
func (c *CI) refTip(ctx context.Context, repo domain.RepoName, ref string, objects ...domain.ObjectID) (domain.CIRefTip, bool) {
	if c.cfg.RefTips == nil {
		return domain.CIRefTip{}, false
	}
	t, err := c.cfg.RefTips.GetRefTip(ctx, repo, ref)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			ctxLog(ctx).Error().Err(err).Str("repo", repo.String()).Str("ref", ref).Msg("ci: read who pushed a ref: it is not trusted")
		}
		return domain.CIRefTip{}, false
	}
	for _, o := range objects {
		if !o.IsZero() && t.Sha == o.String() {
			return t, true
		}
	}
	return domain.CIRefTip{}, false
}

// scheduleActor returns the actor, the pusher and the trust of a scheduled run
// on the tip of a branch: those of the record of the branch when it names this
// very commit; otherwise the owner's name, no pusher and no trust.
func (c *CI) scheduleActor(ctx context.Context, repo domain.RepoName, branch string, tip domain.ObjectID) (actor string, pusher domain.CIPusher, trusted bool) {
	if t, ok := c.refTip(ctx, repo, "refs/heads/"+branch, tip); ok {
		return t.Actor, t.Pusher, t.Trusted
	}
	return authorOf(domain.Owner()), domain.CIPusher{}, false
}

// refTrusted reports whether the ref was set to exactly the pinned object by a
// trusted principal. The record must name the object the dispatch resolved (the
// commit, or the tag object of an annotated tag), which the dispatch captured
// together with the commit it runs: the refs are not read again here, so a tag
// retargeted since cannot lend its new record to the old commit.
func (c *CI) refTrusted(ctx context.Context, repo domain.RepoName, pin pinnedRef) bool {
	t, ok := c.refTip(ctx, repo, pin.ref, pin.object)
	return ok && t.Trusted
}
