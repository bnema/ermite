// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/bnema/zerowrap"
)

const MaxIssueLinkCommits = ports.MaxMergeRequestCommits
const MaxIssueLinksPerIssue = 100
const maxLinkedMRsPerPush = 50

type IssueLinksConfig struct {
	Meta   ports.RepoMetaStore
	Issues ports.IssueStore
	MRs    ports.MergeRequestStore
	Links  ports.IssueLinkStore
	Git    ports.MergeGit
	Bus    ports.EventBus
	Clock  ports.Clock
}

// IssueLinks maintains the derived index and applies closing references on merge.
type IssueLinks struct {
	cfg   IssueLinksConfig
	mu    sync.Mutex
	locks map[linkKey]*linkLock
}

type linkKey struct {
	repo   domain.RepoName
	number int64
}

type linkLock struct {
	ch   chan struct{}
	refs int
}

// lockMR serializes reindexes of one merge request, waiting until ctx ends.
// Entries are removed when no one holds or waits for them.
func (l *IssueLinks) lockMR(ctx context.Context, repo domain.RepoName, number int64) (func(), error) {
	key := linkKey{repo, number}
	l.mu.Lock()
	e, ok := l.locks[key]
	if !ok {
		e = &linkLock{ch: make(chan struct{}, 1)}
		l.locks[key] = e
	}
	e.refs++
	l.mu.Unlock()
	release := func() {
		l.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
	select {
	case e.ch <- struct{}{}:
		return func() { <-e.ch; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

var _ ports.IssueLinkTrigger = (*IssueLinks)(nil)

func NewIssueLinks(cfg IssueLinksConfig) *IssueLinks {
	return &IssueLinks{cfg: cfg, locks: map[linkKey]*linkLock{}}
}

// Reindex replaces links from the current texts and bounded commit range.
func (l *IssueLinks) Reindex(ctx context.Context, p domain.Principal, repo domain.Repo, number int64) error {
	_, err := l.reindex(ctx, p, repo, number)
	return err
}
func (l *IssueLinks) reindex(ctx context.Context, p domain.Principal, repo domain.Repo, number int64) ([]int64, error) {
	unlock, err := l.lockMR(ctx, repo.Name, number)
	if err != nil {
		return nil, err
	}
	defer unlock()
	mr, err := l.cfg.MRs.Get(ctx, repo.Name, number)
	if err != nil {
		return nil, err
	}
	notes, err := l.cfg.MRs.ListComments(ctx, repo.Name, number)
	if err != nil {
		return nil, err
	}
	var texts, messages []string
	for _, note := range notes {
		if !note.Deleted {
			texts = append(texts, note.Text.String())
		}
	}
	if mr.IsOpen() {
		mr.HeadID, err = l.cfg.Git.BranchHead(ctx, repo.Name, mr.Source)
		if errors.Is(err, domain.ErrNotFound) {
			mr.HeadID = domain.ObjectID{}
		} else if err != nil {
			return nil, err
		}
	}
	exclude, _, err := mrRange(ctx, l.cfg.Git, mr)
	if err != nil {
		return nil, err
	}
	has := !mr.HeadID.IsZero()
	if has && !mr.IsOpen() {
		has, err = l.cfg.Git.HasCommit(ctx, repo.Name, mr.HeadID)
		if err != nil {
			return nil, err
		}
	}
	if has {
		commits, more, err := l.cfg.Git.ListRange(ctx, repo.Name, exclude, mr.HeadID, MaxIssueLinkCommits)
		if err != nil {
			return nil, err
		}
		if more {
			log := zerowrap.FromCtx(ctx)
			log.Debug().Msg("issue link commit range truncated")
		}
		for _, commit := range commits {
			messages = append(messages, commit.Message)
		}
	}
	refs := domain.ScanMergeRequest(mr.Title.String(), mr.Description.String(), texts, messages)
	var mentions, closes []int64
	for _, n := range refs.Mentions {
		_, err := l.cfg.Issues.Get(ctx, repo.Name, n)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		mentions = append(mentions, n)
		if slices.Contains(refs.Closes, n) {
			closes = append(closes, n)
		}
	}
	for range maxWriteRetries {
		added, removed, err := l.cfg.Links.ReplaceLinks(ctx, repo.Name, number, mentions, closes, ports.ReferenceEvent{Actor: domain.AuthorName(p), At: l.cfg.Clock.Now().UTC()})
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		type pair struct{ issue, mr int64 }
		sent := map[pair]struct{}{}
		for _, link := range slices.Concat(added, removed) {
			p := pair{link.Issue, link.MergeRequest}
			if _, ok := sent[p]; ok {
				continue
			}
			sent[p] = struct{}{}
			l.notify(ctx, repo, link)
		}
		return closes, nil
	}
	return nil, domain.ErrConflict
}
func (l *IssueLinks) notify(ctx context.Context, repo domain.Repo, link domain.IssueLink) {
	l.cfg.Bus.Publish(ctx, domain.IssueReferenced{Repo: repo.Name, Issue: link.Issue, MergeRequest: link.MergeRequest, At: l.cfg.Clock.Now().UTC(), Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
}
func linkContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}
func linkWarning(ctx context.Context, err error) {
	if err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Msg("issue links not updated")
	}
}
func (l *IssueLinks) OnMergeRequestChanged(ctx context.Context, p domain.Principal, repo domain.Repo, number int64) {
	ctx, cancel := linkContext(ctx)
	defer cancel()
	linkWarning(ctx, l.Reindex(ctx, p, repo, number))
}
func (l *IssueLinks) settled(ctx context.Context, repo domain.Repo, mr domain.MergeRequest) {
	links, err := l.cfg.Links.LinksOfMergeRequest(ctx, repo.Name, mr.Number)
	linkWarning(ctx, err)
	if err == nil {
		for _, link := range links {
			l.notify(ctx, repo, link)
		}
	}
}
func (l *IssueLinks) OnMergeRequestSettled(ctx context.Context, repo domain.Repo, mr domain.MergeRequest) {
	ctx, cancel := linkContext(ctx)
	defer cancel()
	l.settled(ctx, repo, mr)
}
func (l *IssueLinks) OnBranchesMoved(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate) {
	ctx, cancel := linkContext(ctx)
	defer cancel()
	var branches []string
	for _, up := range updates {
		if b, ok := strings.CutPrefix(up.Ref, "refs/heads/"); ok && !up.IsDelete() {
			branches = append(branches, b)
		}
	}
	if len(branches) == 0 {
		return
	}
	repo, err := l.cfg.Meta.Get(ctx, name)
	if err != nil {
		linkWarning(ctx, err)
		return
	}
	numbers, truncated, err := l.cfg.MRs.OpenBySource(ctx, name, branches, maxLinkedMRsPerPush)
	if err != nil {
		linkWarning(ctx, err)
		return
	}
	if truncated {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Str("repo", name.String()).Msg("issue link merge requests truncated")
	}
	for _, n := range numbers {
		linkWarning(ctx, l.Reindex(ctx, p, repo, n))
	}
}
func (l *IssueLinks) OnMerged(ctx context.Context, p domain.Principal, repo domain.Repo, mr domain.MergeRequest) {
	ctx, cancel := linkContext(ctx)
	defer cancel()
	defer l.settled(ctx, repo, mr)
	closes, err := l.reindex(ctx, p, repo, mr.Number)
	if err != nil {
		linkWarning(ctx, err)
		return
	}
	if !repo.Settings.IssuesEnabled() {
		log := zerowrap.FromCtx(ctx)
		log.Debug().Msg("merge issue closing disabled")
		return
	}
	for _, n := range closes {
		err := l.resolve(ctx, p, repo, mr.Number, n)
		if err != nil {
			log := zerowrap.FromCtx(ctx)
			log.Warn().Err(err).Str("repo", repo.Name.String()).Int64("number", n).Int64("merge_request", mr.Number).Msg("issue not resolved by the merge")
		}
	}
}
func (l *IssueLinks) resolve(ctx context.Context, p domain.Principal, repo domain.Repo, mr, n int64) error {
	for range maxWriteRetries {
		issue, err := l.cfg.Issues.Get(ctx, repo.Name, n)
		if err != nil {
			return err
		}
		if !issue.IsOpen() {
			return nil
		}
		issue.State = domain.IssueStateResolved
		issue.ClosedByMergeRequest = mr
		issue.UpdatedAt = l.cfg.Clock.Now().UTC()
		err = l.cfg.Issues.Update(ctx, issue, domain.IssueEvent{Kind: domain.IssueEventResolved, MergeRequest: mr, Actor: domain.AuthorName(p), CreatedAt: issue.UpdatedAt})
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		recordActivity(ctx, l.cfg.Meta, repo.Name, issue.UpdatedAt)
		log := zerowrap.FromCtx(ctx)
		log.Info().Str("repo", repo.Name.String()).Int64("number", n).Int64("merge_request", mr).Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("issue resolved by merge")
		l.cfg.Bus.Publish(ctx, domain.IssueResolved{Issue: issue, At: issue.UpdatedAt, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
		return nil
	}
	return domain.ErrConflict
}
