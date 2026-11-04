// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// UnderRepoLock holds Bernard's per-repository lock while fn runs.
func (b *Bernard) UnderRepoLock(repo domain.RepoName, fn func()) {
	defer b.repoLock(repo)()
	fn()
}

// SetHoldForTest sets the hold release of b.
func (b *Bernard) SetHoldForTest(h interface {
	ReleaseHold(ctx context.Context, repo domain.RepoName, branch string, sha domain.ObjectID, created bool) error
}) {
	b.cfg.Hold = h
}

// Drain takes every queue step that can be taken now, as the queue loop does.
func (b *Bernard) Drain(ctx context.Context) { b.drain(ctx) }

// OpenNow is Open followed by the drain of the queue: the work Open used to
// do at once. It returns the record after the drain, or the error of the
// last queue step on the batch.
func (b *Bernard) OpenNow(ctx context.Context, p domain.Principal, repo domain.RepoName, key domain.DepBatchKey, expected []domain.DepRef) (domain.RepoDeps, error) {
	if _, err := b.Open(ctx, p, repo, key, expected); err != nil {
		return domain.RepoDeps{}, err
	}
	b.drainMu.Lock()
	b.stepErr = nil
	b.drainMu.Unlock()
	b.drain(ctx)
	b.drainMu.Lock()
	err := b.stepErr
	b.drainMu.Unlock()
	if err != nil {
		return domain.RepoDeps{}, err
	}
	return b.cfg.Store.Get(ctx, repo)
}

// SetConcurrencyForTest sets the dependency run limit of b.
func (b *Bernard) SetConcurrencyForTest(n int) { b.cfg.Concurrency = n }

// MetaForTest returns the repository metadata store of b.
func (b *Bernard) MetaForTest() any { return b.cfg.Meta }

// EnableAutoOpenForTest turns cfg.AutoOpen on with placeholder Open ports, so
// that canOpen holds; the queue is not started.
func (b *Bernard) EnableAutoOpenForTest(git ports.MergeGit, repos ports.GitRepos, signer ports.CommitSigner, mrs ports.MergeRequestService, mrst ports.MergeRequestStore) {
	b.cfg.AutoOpen = true
	b.cfg.Git, b.cfg.Repos, b.cfg.Signer, b.cfg.MRs, b.cfg.MRStore = git, repos, signer, mrs, mrst
}

// WriteDraftForTest takes the server step of the batch key of repo as the
// queue does, without the run step.
func (b *Bernard) WriteDraftForTest(ctx context.Context, repo domain.RepoName, key domain.DepBatchKey) (domain.RepoDeps, error) {
	defer b.repoLock(repo)()
	d, err := b.cfg.Store.Get(ctx, repo)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	batch, _, _ := d.Batch(key)
	return b.writeDraft(ctx, repo, batch, batch.Refs())
}
