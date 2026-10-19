// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Inbox is the owner's inbox (ADR 0031): the open merge requests and issues of
// every repository in one place. Only the owner may read it; anyone else
// gets domain.ErrNotFound, as for a page that does not exist. The issues of a
// repository whose issues mode is off are left out.
type Inbox struct {
	meta   ports.RepoMetaStore
	mrs    ports.MergeRequestStore
	issues ports.IssueStore
	// pending is nil when the public form is not wired.
	pending ports.PendingIssueStore
}

var _ ports.InboxService = (*Inbox)(nil)

// InboxConfig holds what the inbox needs. Every field is required.
type InboxConfig struct {
	Meta          ports.RepoMetaStore
	MergeRequests ports.MergeRequestStore
	Issues        ports.IssueStore
	// Pending lists the issues awaiting moderation. Optional: without it the
	// inbox has no pending section.
	Pending ports.PendingIssueStore
}

// NewInbox builds the inbox use case.
func NewInbox(cfg InboxConfig) *Inbox {
	return &Inbox{meta: cfg.Meta, mrs: cfg.MergeRequests, issues: cfg.Issues, pending: cfg.Pending}
}

// Overview lists what is open, through the indexes of open items. The counts
// and the list of repositories do not depend on the filter, the lists do.
func (in *Inbox) Overview(ctx context.Context, p domain.Principal, filter domain.RepoName) (ports.InboxOverview, error) {
	var out ports.InboxOverview
	if !p.HasOwnerRights() {
		return out, domain.ErrNotFound
	}
	mrs, err := in.mrs.ListOpenAll(ctx)
	if err != nil {
		return out, fmt.Errorf("list open merge requests: %w", err)
	}
	issues, err := in.issues.ListOpenAll(ctx)
	if err != nil {
		return out, fmt.Errorf("list open issues: %w", err)
	}

	// exists and issuesOn cache what is read of each repository.
	issuesOn := make(map[domain.RepoName]bool)
	exists := make(map[domain.RepoName]bool)
	look := func(name domain.RepoName) error {
		if _, ok := exists[name]; ok {
			return nil
		}
		repo, err := lookupRepo(ctx, in.meta, name)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			exists[name] = false
		case err != nil:
			return fmt.Errorf("read repository %q: %w", name, err)
		default:
			exists[name] = true
			issuesOn[name] = repo.Settings.Issues.OrDefault() != domain.IssuesOff
		}
		return nil
	}

	repos := make(map[domain.RepoName]bool)
	for _, m := range mrs {
		if err := look(m.Repo); err != nil {
			return out, err
		}
		if !exists[m.Repo] {
			continue
		}
		repos[m.Repo] = true
		out.Counts.MergeRequests++
		if filter.IsZero() || m.Repo == filter {
			out.MergeRequests = append(out.MergeRequests, ports.InboxMergeRequest{MergeRequest: m})
		}
	}
	for _, i := range issues {
		if err := look(i.Repo); err != nil {
			return out, err
		}
		if !exists[i.Repo] || !issuesOn[i.Repo] {
			continue
		}
		repos[i.Repo] = true
		out.Counts.Issues++
		if filter.IsZero() || i.Repo == filter {
			out.Issues = append(out.Issues, i)
		}
	}

	if in.pending != nil {
		page, err := in.pending.List(ctx, domain.RepoName{}, ports.ListPendingParams{PageSize: domain.MaxPendingIssues})
		if err != nil {
			return out, fmt.Errorf("list pending issues: %w", err)
		}
		for _, pi := range page.Issues {
			if err := look(pi.Repo); err != nil {
				return out, err
			}
			if !exists[pi.Repo] || !issuesOn[pi.Repo] {
				continue
			}
			pi.IPHash = nil // the hash is for bans, the page never needs it
			out.Pending = append(out.Pending, pi)
			repos[pi.Repo] = true
		}
		out.Counts.Pending = len(out.Pending)
	}

	slices.SortStableFunc(out.MergeRequests, func(a, b ports.InboxMergeRequest) int {
		return compareRecent(a.UpdatedAt, a.CreatedAt, a.Repo, a.Number, b.UpdatedAt, b.CreatedAt, b.Repo, b.Number)
	})
	slices.SortStableFunc(out.Issues, func(a, b domain.Issue) int {
		return compareRecent(a.UpdatedAt, a.CreatedAt, a.Repo, a.Number, b.UpdatedAt, b.CreatedAt, b.Repo, b.Number)
	})
	for r := range repos {
		out.Repos = append(out.Repos, r)
	}
	slices.SortFunc(out.Repos, func(a, b domain.RepoName) int { return strings.Compare(a.String(), b.String()) })
	return out, nil
}

// compareRecent orders by update time, most recent first (the creation time
// stands in for a missing one), then by repository and number.
func compareRecent(aUp, aCr time.Time, aRepo domain.RepoName, aN int64, bUp, bCr time.Time, bRepo domain.RepoName, bN int64) int {
	if aUp.IsZero() {
		aUp = aCr
	}
	if bUp.IsZero() {
		bUp = bCr
	}
	if c := bUp.Compare(aUp); c != 0 {
		return c
	}
	if c := strings.Compare(aRepo.String(), bRepo.String()); c != 0 {
		return c
	}
	switch {
	case aN > bN:
		return -1
	case aN < bN:
		return 1
	}
	return 0
}
