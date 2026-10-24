// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Edit changes only present fields, preserving concurrent writes through CAS retries.
func (i *Issues) Edit(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params ports.EditIssueParams) (domain.Issue, error) {
	repo, err := i.writable(ctx, p, name)
	if err != nil {
		return domain.Issue{}, err
	}
	if params.Title == nil && params.Body == nil {
		return domain.Issue{}, fmt.Errorf("%w: at least one of title or body is required", domain.ErrInvalid)
	}
	var title domain.IssueTitle
	var body domain.IssueBody
	var v domain.Violations
	if params.Title != nil {
		title, err = domain.ParseIssueTitle(*params.Title)
		if err != nil {
			v.AddErr("title", err)
		}
	}
	if params.Body != nil {
		body, err = domain.ParseIssueBody(*params.Body)
		if err != nil {
			v.AddErr("body", err)
		}
	}
	if err := v.Err(); err != nil {
		return domain.Issue{}, err
	}
	for range maxWriteRetries {
		issue, err := i.store.Get(ctx, name, number)
		if err != nil {
			return domain.Issue{}, err
		}
		if issue.State != domain.IssueStateOpen && issue.State != domain.IssueStateClosed && issue.State != domain.IssueStateResolved {
			return domain.Issue{}, fmt.Errorf("%w: pending issues cannot be edited", domain.ErrPreconditionFailed)
		}
		var changed []string
		if params.Title != nil && title != issue.Title {
			issue.Title = title
			changed = append(changed, "title")
		}
		if params.Body != nil && body != issue.Body {
			issue.Body = body
			changed = append(changed, "body")
		}
		if len(changed) == 0 {
			return issue, nil
		}
		issue.UpdatedAt = i.clock.Now().UTC()
		err = i.store.Update(ctx, issue, domain.IssueEvent{Kind: domain.IssueEventEdited, ChangedFields: changed, Actor: domain.AuthorName(p), CreatedAt: issue.UpdatedAt})
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return domain.Issue{}, err
		}
		recordActivity(ctx, i.meta, name, issue.UpdatedAt)
		i.bus.Publish(ctx, domain.IssueEdited{Issue: issue, ChangedFields: changed, At: issue.UpdatedAt, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
		return issue, nil
	}
	return domain.Issue{}, domain.ErrConflict
}

// Edit changes the title and/or description of an open or closed merge request.
func (m *MergeRequests) Edit(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params ports.EditMergeRequestParams) (domain.MergeRequest, error) {
	repo, err := m.writable(ctx, p, name, domain.ScopeMRWrite)
	if err != nil {
		return domain.MergeRequest{}, err
	}
	if params.Title == nil && params.Description == nil {
		return domain.MergeRequest{}, fmt.Errorf("%w: at least one of title or description is required", domain.ErrInvalid)
	}
	var title domain.MRTitle
	var desc domain.PlainText
	var v domain.Violations
	if params.Title != nil {
		title, err = domain.ParseMRTitle(*params.Title)
		if err != nil {
			v.AddErr("title", err)
		}
	}
	if params.Description != nil {
		desc, err = domain.ParsePlainText(*params.Description)
		if err != nil {
			v.AddErr("description", err)
		}
	}
	if err := v.Err(); err != nil {
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
		if mr.State == domain.MRStateMerged {
			return domain.MergeRequest{}, fmt.Errorf("%w: merged merge requests cannot be edited", domain.ErrPreconditionFailed)
		}
		var changed []string
		if params.Title != nil && title != mr.Title {
			mr.Title = title
			changed = append(changed, "title")
		}
		if params.Description != nil && desc != mr.Description {
			mr.Description = desc
			changed = append(changed, "description")
		}
		if len(changed) == 0 {
			return mr, nil
		}
		mr.UpdatedAt = m.clock.Now().UTC()
		err = m.store.Update(ctx, mr)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return domain.MergeRequest{}, err
		}
		recordActivity(ctx, m.meta, name, mr.UpdatedAt)
		m.bus.Publish(ctx, domain.MergeRequestEdited{MergeRequest: mr, ChangedFields: changed, At: mr.UpdatedAt, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
		if m.links != nil {
			m.links.OnMergeRequestChanged(ctx, p, repo, number)
		}
		return mr, nil
	}
	return domain.MergeRequest{}, domain.ErrConflict
}
