// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// WithAutoMerge installs the asynchronous evaluator before serving requests.
func (m *MergeRequests) WithAutoMerge(t ports.AutoMergeTrigger) *MergeRequests {
	m.autoMerge = t
	return m
}

// EnableAutoMerge records the actor and current head; re-enabling replaces the
// previous intent rather than preserving an older actor's authority.
func (m *MergeRequests) EnableAutoMerge(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params ports.EnableAutoMergeParams) (domain.MergeRequest, error) {
	if _, err := m.writable(ctx, p, name, domain.ScopeMRMerge); err != nil {
		return domain.MergeRequest{}, err
	}
	if m.checks == nil {
		return domain.MergeRequest{}, fmt.Errorf("%w: this server has no CI", domain.ErrPreconditionFailed)
	}
	mode := params.Mode
	if mode == "" {
		mode = domain.MergeModeFastForward
	}
	if _, err := domain.ParseMergeMode(mode.String()); err != nil {
		return domain.MergeRequest{}, err
	}
	unlock, err := m.lockRepo(ctx, name)
	if err != nil {
		return domain.MergeRequest{}, err
	}
	defer unlock()
	for range maxWriteRetries {
		mr, err := m.store.Get(ctx, name, number)
		if err != nil {
			return domain.MergeRequest{}, err
		}
		if !mr.IsOpen() {
			return domain.MergeRequest{}, fmt.Errorf("%w: !%d is %s", domain.ErrPreconditionFailed, number, mr.State)
		}
		if mr.IsDraft() {
			return domain.MergeRequest{}, fmt.Errorf("%w: !%d is a draft: mark it ready for review first", domain.ErrPreconditionFailed, number)
		}
		if !p.AllowsRef("refs/heads/" + mr.Target) {
			return domain.MergeRequest{}, fmt.Errorf("%w: this token may not write %s", domain.ErrForbidden, mr.Target)
		}
		repo, err := m.meta.Get(ctx, name)
		if err != nil {
			return domain.MergeRequest{}, err
		}
		settings := repo.EffectiveSettings()
		if !settings.AllowAutoMerge {
			return domain.MergeRequest{}, fmt.Errorf("%w: auto-merge is off in %s: turn it on with `ermite repo edit --allow-auto-merge`", domain.ErrPreconditionFailed, name)
		}
		if !settings.Allows(mode) {
			return domain.MergeRequest{}, fmt.Errorf("%w: the %s merge mode is not allowed in %s", domain.ErrPreconditionFailed, mode, name)
		}
		head, err := m.branchOrZero(ctx, name, mr.Source)
		if err != nil {
			return domain.MergeRequest{}, err
		}
		if head.IsZero() {
			return domain.MergeRequest{}, fmt.Errorf("%w: the source branch %q no longer exists", domain.ErrPreconditionFailed, mr.Source)
		}
		mr.UpdatedAt = m.clock.Now().UTC()
		mr.AutoMerge = &domain.AutoMerge{Mode: mode, HeadID: head, EnabledBy: domain.PusherOf(p), EnablerLabel: domain.AuthorName(p), EnabledAt: mr.UpdatedAt}
		if err := m.store.Update(ctx, mr); errors.Is(err, domain.ErrConflict) {
			continue
		} else if err != nil {
			return domain.MergeRequest{}, err
		}
		recordActivity(ctx, m.meta, name, mr.UpdatedAt)
		m.bus.Publish(ctx, domain.MergeRequestAutoMergeEnabled{MergeRequest: mr, At: mr.UpdatedAt, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
		if m.autoMerge != nil {
			m.autoMerge.OnRepoChanged(ctx, name)
		}
		return m.withHead(ctx, mr)
	}
	return domain.MergeRequest{}, fmt.Errorf("enable auto-merge: %w", domain.ErrConflict)
}

// CancelAutoMerge clears an open request's intent with the same target access
// required to enable it.
func (m *MergeRequests) CancelAutoMerge(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.MergeRequest, error) {
	if _, err := m.writable(ctx, p, name, domain.ScopeMRMerge); err != nil {
		return domain.MergeRequest{}, err
	}
	unlock, err := m.lockRepo(ctx, name)
	if err != nil {
		return domain.MergeRequest{}, err
	}
	defer unlock()
	mr, err := m.store.Get(ctx, name, number)
	if err != nil {
		return domain.MergeRequest{}, err
	}
	if !mr.IsOpen() {
		return domain.MergeRequest{}, fmt.Errorf("%w: !%d is %s", domain.ErrPreconditionFailed, number, mr.State)
	}
	if !p.AllowsRef("refs/heads/" + mr.Target) {
		return domain.MergeRequest{}, fmt.Errorf("%w: this token may not write %s", domain.ErrForbidden, mr.Target)
	}
	if mr.AutoMerge == nil {
		return domain.MergeRequest{}, fmt.Errorf("%w: auto-merge is not enabled", domain.ErrPreconditionFailed)
	}
	mr, err = m.cancelAutoLocked(ctx, mr, "cancelled by "+domain.AuthorName(p))
	if err != nil {
		return domain.MergeRequest{}, err
	}
	return m.withHead(ctx, mr)
}

// cancelAuto takes the merge lock and only cancels the intent evaluated by the
// caller. A concurrent re-enable, close or merge must not be undone.
func (m *MergeRequests) cancelAuto(ctx context.Context, mr domain.MergeRequest, reason string) error {
	unlock, err := m.lockRepo(ctx, mr.Repo)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = m.cancelAutoLocked(ctx, mr, reason)
	return err
}

func (m *MergeRequests) cancelAutoLocked(ctx context.Context, expected domain.MergeRequest, reason string) (domain.MergeRequest, error) {
	for range maxWriteRetries {
		mr, err := m.store.Get(ctx, expected.Repo, expected.Number)
		if err != nil {
			return domain.MergeRequest{}, err
		}
		if !mr.IsOpen() || mr.AutoMerge == nil || expected.AutoMerge == nil || *mr.AutoMerge != *expected.AutoMerge {
			return mr, nil
		}
		repo, err := m.meta.Get(ctx, mr.Repo)
		if err != nil {
			return domain.MergeRequest{}, err
		}
		mr.AutoMerge, mr.UpdatedAt = nil, m.clock.Now().UTC()
		if err := m.store.Update(ctx, mr); errors.Is(err, domain.ErrConflict) {
			continue
		} else if err != nil {
			return domain.MergeRequest{}, err
		}
		recordActivity(ctx, m.meta, mr.Repo, mr.UpdatedAt)
		m.bus.Publish(ctx, domain.MergeRequestAutoMergeCancelled{MergeRequest: mr, Reason: reason, At: mr.UpdatedAt, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
		return mr, nil
	}
	return domain.MergeRequest{}, fmt.Errorf("cancel auto-merge: %w", domain.ErrConflict)
}
