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

// MaxCommentsPerIssue is the most comments an issue holds. Past it AddComment
// fails with domain.ErrResourceExhausted. Comments are never deleted, so the
// limit counts the ones ever added.
const MaxCommentsPerIssue = 1000

// Issues are the issue use cases (ADR 0031): open, comment on, close and
// reopen an issue. Issues share their numbers with the merge requests of the
// repository. A repository whose issues mode is off answers domain.ErrNotFound
// to everyone, the owner included, like a repository that does not exist.
// Reading needs the access of reading the repository (canRead), writing needs
// issue:write on it, which the branch prefixes of a token do not restrict:
// an issue writes no ref.
type Issues struct {
	links ports.IssueLinkStore
	meta  ports.RepoMetaStore
	store ports.IssueStore
	mrs   ports.MergeRequestStore
	bus   ports.EventBus
	clock ports.Clock
}

var (
	_ ports.IssueService        = (*Issues)(nil)
	_ ports.IssueNumberResolver = (*Issues)(nil)
)

// IssuesConfig holds what the issue use cases need. Every field is required.
type IssuesConfig struct {
	Links ports.IssueLinkStore
	Meta  ports.RepoMetaStore
	Store ports.IssueStore
	// MergeRequests tells which numbers name a merge request (Kind).
	MergeRequests ports.MergeRequestStore
	Bus           ports.EventBus
	Clock         ports.Clock
}

// NewIssues builds the issue use cases.
func NewIssues(cfg IssuesConfig) *Issues {
	return &Issues{links: cfg.Links, meta: cfg.Meta, store: cfg.Store, mrs: cfg.MergeRequests, bus: cfg.Bus, clock: cfg.Clock}
}

// enabled looks the repository up and hides it when issues are off.
func (i *Issues) enabled(ctx context.Context, name domain.RepoName) (domain.Repo, error) {
	repo, err := lookupRepo(ctx, i.meta, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if !repo.Settings.IssuesEnabled() {
		return domain.Repo{}, domain.ErrNotFound
	}
	return repo, nil
}

// readable applies the read rule: the access of reading the repository.
func (i *Issues) readable(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
	repo, err := i.enabled(ctx, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if !canRead(p, repo) {
		return domain.Repo{}, domain.ErrNotFound
	}
	return repo, nil
}

// writable is the write rule, as MergeRequests.writable: a principal that can
// neither read the repository nor holds the scope on it gets
// domain.ErrNotFound, one that cannot write the error of scopeAccess.
func (i *Issues) writable(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
	repo, err := i.enabled(ctx, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if !canRead(p, repo) && !p.Allows(domain.ScopeIssueWrite, name) {
		return domain.Repo{}, domain.ErrNotFound
	}
	if err := scopeAccess(p, repo, domain.ScopeIssueWrite); err != nil {
		return domain.Repo{}, err
	}
	return repo, nil
}

// Create opens an issue and publishes IssueOpened. The principal needs
// issue:write. Invalid texts give an *domain.InvalidFieldsError.
func (i *Issues) Create(ctx context.Context, p domain.Principal, name domain.RepoName, params ports.CreateIssueParams) (domain.Issue, error) {
	repo, err := i.writable(ctx, p, name)
	if err != nil {
		return domain.Issue{}, err
	}
	var v domain.Violations
	title, err := domain.ParseIssueTitle(params.Title)
	if err != nil {
		v.AddErr("title", err)
	}
	body, err := domain.ParseIssueBody(params.Body)
	if err != nil {
		v.AddErr("body", err)
	}
	if err := v.Err(); err != nil {
		return domain.Issue{}, err
	}
	now := i.clock.Now().UTC()
	issue, err := i.store.Create(ctx, domain.Issue{
		Repo: name, Title: title, Body: body, State: domain.IssueStateOpen,
		Author: domain.AuthorName(p), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return domain.Issue{}, fmt.Errorf("create issue of %q: %w", name, err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", name.String()).Int64("number", issue.Number).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("issue opened")
	recordActivity(ctx, i.meta, repo.Name, now)
	i.bus.Publish(ctx, domain.IssueOpened{Issue: issue, At: now, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
	return issue, nil
}

// List returns a page of the issues of a repository, highest number first.
func (i *Issues) List(ctx context.Context, p domain.Principal, name domain.RepoName, params ports.ListIssuesParams) (ports.IssuePage, error) {
	size, err := pageSize(params.PageSize)
	if err != nil {
		return ports.IssuePage{}, err
	}
	if _, err := i.readable(ctx, p, name); err != nil {
		return ports.IssuePage{}, err
	}
	params.PageSize = size
	page, err := i.store.List(ctx, name, params)
	if err != nil {
		return ports.IssuePage{}, fmt.Errorf("list issues of %q: %w", name, err)
	}
	return page, nil
}

// Get returns an issue with its comments.
func (i *Issues) Get(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (ports.IssueDetail, error) {
	if _, err := i.readable(ctx, p, name); err != nil {
		return ports.IssueDetail{}, err
	}
	issue, err := i.store.Get(ctx, name, number)
	if err != nil {
		return ports.IssueDetail{}, err
	}
	comments, err := i.store.ListComments(ctx, name, number)
	if err != nil {
		return ports.IssueDetail{}, fmt.Errorf("list comments of #%d of %q: %w", number, name, err)
	}
	detail := ports.IssueDetail{Issue: issue, Comments: comments}
	if i.links != nil {
		links, err := i.links.LinksOfIssue(ctx, name, number, MaxIssueLinksPerIssue)
		if err != nil {
			return ports.IssueDetail{}, err
		}
		for _, link := range links {
			mr, err := i.mrs.Get(ctx, name, link.MergeRequest)
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err != nil {
				return ports.IssueDetail{}, err
			}
			detail.References = append(detail.References, ports.IssueReference{Number: mr.Number, Title: mr.Title, State: mr.State, Closes: link.Closes})
		}
	}
	return detail, nil
}

// AddComment adds a comment and publishes IssueCommented. The principal needs
// issue:write. The text must not be blank. A comment may follow a closed
// issue; one that already holds MaxCommentsPerIssue comments gives
// domain.ErrResourceExhausted.
func (i *Issues) AddComment(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, text string) (domain.IssueComment, error) {
	repo, err := i.writable(ctx, p, name)
	if err != nil {
		return domain.IssueComment{}, err
	}
	issue, err := i.store.Get(ctx, name, number)
	if err != nil {
		return domain.IssueComment{}, err
	}
	var v domain.Violations
	body, err := domain.ParseIssueBody(text)
	switch {
	case err != nil:
		v.AddErr("text", err)
	case body.IsBlank():
		v.Add("text", "is required")
	}
	if err := v.Err(); err != nil {
		return domain.IssueComment{}, err
	}
	var c domain.IssueComment
	saved := false
	for range maxWriteRetries {
		if issue.CommentSeq >= MaxCommentsPerIssue {
			return domain.IssueComment{}, fmt.Errorf("%w: #%d already has %d comments", domain.ErrResourceExhausted, number, MaxCommentsPerIssue)
		}
		c = domain.IssueComment{ID: issue.CommentSeq + 1, Author: domain.AuthorName(p), Text: body, CreatedAt: i.clock.Now().UTC()}
		issue.CommentSeq, issue.UpdatedAt = c.ID, c.CreatedAt
		err = i.store.SaveComment(ctx, issue, c)
		if err == nil {
			saved = true
			break
		}
		if !errors.Is(err, domain.ErrConflict) {
			return domain.IssueComment{}, fmt.Errorf("save comment on #%d of %q: %w", number, name, err)
		}
		if issue, err = i.store.Get(ctx, name, number); err != nil {
			return domain.IssueComment{}, err
		}
	}
	if !saved {
		return domain.IssueComment{}, fmt.Errorf("save comment on #%d of %q: %w", number, name, domain.ErrConflict)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", name.String()).Int64("number", number).Int64("comment", c.ID).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("issue comment added")
	recordActivity(ctx, i.meta, name, c.CreatedAt)
	i.bus.Publish(ctx, domain.IssueCommented{
		Repo: name, Number: number, Comment: c, At: c.CreatedAt, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt,
	})
	return c, nil
}

// Close closes an open issue and publishes IssueClosed. The principal needs
// issue:write. An issue that is not open gives domain.ErrPreconditionFailed.
func (i *Issues) Close(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.Issue, error) {
	return i.transition(ctx, p, name, number, domain.IssueStateOpen, domain.IssueStateClosed)
}

// Reopen reopens a closed issue and publishes IssueReopened. The principal
// needs issue:write. An issue that is not closed gives
// domain.ErrPreconditionFailed.
func (i *Issues) Reopen(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.Issue, error) {
	return i.transition(ctx, p, name, number, domain.IssueStateClosed, domain.IssueStateOpen)
}

// transition moves an issue from one state to the other, retrying when another
// write changes it in between.
func (i *Issues) transition(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, from, to domain.IssueState) (domain.Issue, error) {
	repo, err := i.writable(ctx, p, name)
	if err != nil {
		return domain.Issue{}, err
	}
	verb := "close"
	if to == domain.IssueStateOpen {
		verb = "reopen"
	}
	for range maxWriteRetries {
		issue, err := i.store.Get(ctx, name, number)
		if err != nil {
			return domain.Issue{}, err
		}
		if issue.State != from {
			return domain.Issue{}, fmt.Errorf("%w: #%d is %s", domain.ErrPreconditionFailed, number, issue.State)
		}
		issue.ClosedByMergeRequest = 0
		issue.State, issue.UpdatedAt = to, i.clock.Now().UTC()
		err = i.store.Update(ctx, issue)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return domain.Issue{}, fmt.Errorf("%s #%d of %q: %w", verb, number, name, err)
		}
		log := zerowrap.FromCtx(ctx)
		log.Info().Str("repo", name.String()).Int64("number", number).
			Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("issue " + verb + "d")
		recordActivity(ctx, i.meta, repo.Name, issue.UpdatedAt)
		if to == domain.IssueStateOpen {
			i.bus.Publish(ctx, domain.IssueReopened{Issue: issue, At: issue.UpdatedAt, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
		} else {
			i.bus.Publish(ctx, domain.IssueClosed{Issue: issue, At: issue.UpdatedAt, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
		}
		return issue, nil
	}
	return domain.Issue{}, fmt.Errorf("%s #%d of %q: %w", verb, number, name, domain.ErrConflict)
}

// Kind tells what number n of the repository names. The principal needs the
// access of reading the repository. Issues are resolved only when enabled;
// merge requests are resolved even when issues are off.
func (i *Issues) Kind(ctx context.Context, p domain.Principal, name domain.RepoName, n int64) (domain.NumberKind, error) {
	repo, err := lookupRepo(ctx, i.meta, name)
	if err != nil {
		return domain.NumberNone, err
	}
	if !canRead(p, repo) {
		return domain.NumberNone, domain.ErrNotFound
	}
	if repo.Settings.IssuesEnabled() {
		if _, err := i.store.Get(ctx, name, n); err == nil {
			return domain.NumberIssue, nil
		} else if !errors.Is(err, domain.ErrNotFound) {
			return domain.NumberNone, fmt.Errorf("read #%d of %q: %w", n, name, err)
		}
	}
	if _, err := i.mrs.Get(ctx, name, n); err == nil {
		return domain.NumberMergeRequest, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.NumberNone, fmt.Errorf("read !%d of %q: %w", n, name, err)
	}
	return domain.NumberNone, nil
}

// Resolve is reserved for Phase A; no state is changed by this contract stub.
func (i *Issues) Resolve(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.Issue, error) {
	return domain.Issue{}, domain.ErrUnavailable
}
