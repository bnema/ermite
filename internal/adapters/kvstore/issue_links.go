// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	kv "bnema.dev/ermite-kv"
	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"google.golang.org/protobuf/proto"
)

const issueRefPrefix = "isref/"
const mrRefPrefix = "mrref/"

// IssueLinks stores the derived issue-to-merge-request index.
type IssueLinks struct{ db *kv.DB }

var _ ports.IssueLinkStore = (*IssueLinks)(nil)

// IssueLinks returns a link store sharing this Store's database and lifetime.
func (s *Store) IssueLinks() *IssueLinks { return &IssueLinks{db: s.db} }

func mrRefKey(repo domain.RepoName, mr int64) []byte {
	return []byte(mrRefPrefix + repo.String() + "/" + fmt.Sprintf(numberFormat, mr))
}
func issueRefKeys(repo domain.RepoName, issue int64) string {
	return issueRefPrefix + repo.String() + "/" + fmt.Sprintf(numberFormat, issue) + "/"
}
func issueRefKey(repo domain.RepoName, issue, mr int64) []byte {
	return []byte(issueRefKeys(repo, issue) + fmt.Sprintf(numberFormat, mr))
}

// ReplaceLinks atomically replaces both directions of the index.
func (l *IssueLinks) ReplaceLinks(ctx context.Context, repo domain.RepoName, mr int64, issues, closes []int64) (added, removed []domain.IssueLink, err error) {
	key := mrRefKey(repo, mr)
	item, exists := l.db.Get(ctx, key)
	var old storagepb.MergeRequestReferences
	if exists {
		if err := proto.Unmarshal(item.Value, &old); err != nil {
			return nil, nil, fmt.Errorf("decode MR references: %w", err)
		}
	}
	issues = slices.Sorted(slices.Values(issues))
	issues = slices.Compact(issues)
	closes = slices.Sorted(slices.Values(closes))
	closes = slices.Compact(closes)
	for _, n := range issues {
		if n < 1 {
			return nil, nil, domain.ErrInvalid
		}
	}
	for _, n := range closes {
		if !slices.Contains(issues, n) {
			return nil, nil, domain.ErrInvalid
		}
	}
	var b kv.Batch
	if err := guardRepoKey(ctx, l.db, &b, repo); err != nil {
		return nil, nil, err
	}
	if exists {
		b.RequireVersion(key, item.Version)
	} else {
		b.RequireAbsent(key)
	}
	for _, n := range old.GetIssues() {
		wasClosing := slices.Contains(old.GetCloses(), n)
		if !slices.Contains(issues, n) {
			b.Delete(issueRefKey(repo, n, mr))
			removed = append(removed, domain.IssueLink{Issue: n, MergeRequest: mr, Closes: wasClosing})
		} else if wasClosing && !slices.Contains(closes, n) {
			removed = append(removed, domain.IssueLink{Issue: n, MergeRequest: mr, Closes: true})
		}
	}
	for _, n := range issues {
		closing := slices.Contains(closes, n)
		b.Put(issueRefKey(repo, n, mr), mustMarshal(&storagepb.IssueLink{Closes: closing}))
		if !slices.Contains(old.GetIssues(), n) || closing && !slices.Contains(old.GetCloses(), n) {
			added = append(added, domain.IssueLink{Issue: n, MergeRequest: mr, Closes: closing})
		}
	}
	b.Put(key, mustMarshal(&storagepb.MergeRequestReferences{Issues: issues, Closes: closes}))
	if _, err := l.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return nil, nil, domain.ErrConflict
		}
		return nil, nil, fmt.Errorf("replace issue links: %w", err)
	}
	return added, removed, nil
}

// LinksOfIssue returns links ordered by merge request number.
func (l *IssueLinks) LinksOfIssue(ctx context.Context, repo domain.RepoName, issue int64, limit int) ([]domain.IssueLink, error) {
	var result []domain.IssueLink
	prefix := issueRefKeys(repo, issue)
	for key, item := range l.db.Snapshot(ctx).Prefix(ctx, []byte(prefix)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mr, err := strconv.ParseInt(strings.TrimPrefix(string(key), prefix), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode issue link key: %w", err)
		}
		var pb storagepb.IssueLink
		if err := proto.Unmarshal(item.Value, &pb); err != nil {
			return nil, fmt.Errorf("decode issue link: %w", err)
		}
		result = append(result, domain.IssueLink{Issue: issue, MergeRequest: mr, Closes: pb.GetCloses()})
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result, nil
}

// LinksOfMergeRequest returns the links recorded by its last rebuild.
func (l *IssueLinks) LinksOfMergeRequest(ctx context.Context, repo domain.RepoName, mr int64) ([]domain.IssueLink, error) {
	item, ok := l.db.Get(ctx, mrRefKey(repo, mr))
	if !ok {
		return nil, nil
	}
	var pb storagepb.MergeRequestReferences
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return nil, fmt.Errorf("decode MR references: %w", err)
	}
	var result []domain.IssueLink
	for _, n := range pb.GetIssues() {
		result = append(result, domain.IssueLink{Issue: n, MergeRequest: mr, Closes: slices.Contains(pb.GetCloses(), n)})
	}
	return result, nil
}
