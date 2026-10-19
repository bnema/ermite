// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// IssueModeration is the owner's moderation of the issues submitted through
// the public form (ADR 0031): list, approve and reject, with an optional ban of
// the submitter. Owner rights only; anyone else gets domain.ErrNotFound, as
// for a page that does not exist.
type IssueModeration struct {
	meta    ports.RepoMetaStore
	pending ports.PendingIssueStore
	bans    ports.IssueBanStore
	bus     ports.EventBus
	clock   ports.Clock
}

var _ ports.IssueModerationService = (*IssueModeration)(nil)

// IssueModerationConfig holds what the moderation needs. Every field is
// required.
type IssueModerationConfig struct {
	Meta    ports.RepoMetaStore
	Pending ports.PendingIssueStore
	Bans    ports.IssueBanStore
	Bus     ports.EventBus
	Clock   ports.Clock
}

// NewIssueModeration builds the moderation use case.
func NewIssueModeration(cfg IssueModerationConfig) *IssueModeration {
	return &IssueModeration{meta: cfg.Meta, pending: cfg.Pending, bans: cfg.Bans, bus: cfg.Bus, clock: cfg.Clock}
}

// ListPending lists the pending issues of a repository, or of every repository
// when repo is zero, oldest first.
func (m *IssueModeration) ListPending(ctx context.Context, p domain.Principal, repo domain.RepoName, page ports.ListPendingParams) (ports.PendingIssuePage, error) {
	if !p.HasOwnerRights() {
		return ports.PendingIssuePage{}, domain.ErrNotFound
	}
	size, err := pageSize(page.PageSize)
	if err != nil {
		return ports.PendingIssuePage{}, err
	}
	page.PageSize = size
	out, err := m.pending.List(ctx, repo, page)
	if err != nil {
		return ports.PendingIssuePage{}, fmt.Errorf("list pending issues: %w", err)
	}
	return out, nil
}

// Approve gives the pending issue the next number of its repository and opens
// it, then publishes IssueOpened. The repository must exist with issues on
// (else domain.ErrNotFound), and so must the pending issue.
func (m *IssueModeration) Approve(ctx context.Context, p domain.Principal, repo domain.RepoName, id domain.PendingIssueID) (domain.Issue, error) {
	if !p.HasOwnerRights() {
		return domain.Issue{}, domain.ErrNotFound
	}
	r, err := lookupRepo(ctx, m.meta, repo)
	if err != nil {
		return domain.Issue{}, err
	}
	if !r.Settings.IssuesEnabled() {
		return domain.Issue{}, domain.ErrNotFound
	}
	now := m.clock.Now().UTC()
	issue, err := m.pending.Approve(ctx, repo, id, now)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Issue{}, err
		}
		return domain.Issue{}, fmt.Errorf("approve pending issue of %q: %w", repo, err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", repo.String()).Int64("number", issue.Number).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("pending issue approved")
	recordActivity(ctx, m.meta, repo, now)
	m.bus.Publish(ctx, domain.IssueOpened{Issue: issue, At: now, Visibility: r.Visibility, CreatedAt: r.CreatedAt})
	return issue, nil
}

// Reject deletes the pending issue. With ban, its submitter is first banned
// for domain.BanDuration.
func (m *IssueModeration) Reject(ctx context.Context, p domain.Principal, repo domain.RepoName, id domain.PendingIssueID, ban bool) error {
	if !p.HasOwnerRights() {
		return domain.ErrNotFound
	}
	pending, err := m.pending.Get(ctx, repo, id)
	if err != nil {
		return err
	}
	if ban {
		until := m.clock.Now().UTC().Add(domain.BanDuration)
		if err := m.bans.Ban(ctx, pending.IPHash, until); err != nil {
			return fmt.Errorf("ban submitter of pending issue of %q: %w", repo, err)
		}
	}
	if err := m.pending.Delete(ctx, repo, id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return err
		}
		return fmt.Errorf("delete pending issue of %q: %w", repo, err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", repo.String()).Bool("ban", ban).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("pending issue rejected")
	m.bus.Publish(ctx, domain.IssuePendingRemoved{Repo: repo, ID: id, At: m.clock.Now().UTC()})
	return nil
}
