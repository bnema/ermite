// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// FeatureJournal is listed by GetVersion when JournalService and
// ReleaseService.ListUnreleased are served.
const FeatureJournal = "journal"

// journalService implements ermiteconnect.JournalServiceHandler.
type journalService struct{ journal ports.JournalService }

var _ ermiteconnect.JournalServiceHandler = (*journalService)(nil)

var journalKindToPB = map[domain.JournalKind]pb.ActivityKind{
	domain.JournalMergeRequestOpened: pb.ActivityKind_ACTIVITY_KIND_MR_OPENED,
	domain.JournalMergeRequestMerged: pb.ActivityKind_ACTIVITY_KIND_MR_MERGED,
	domain.JournalMergeRequestClosed: pb.ActivityKind_ACTIVITY_KIND_MR_CLOSED,
	domain.JournalIssueOpened:        pb.ActivityKind_ACTIVITY_KIND_ISSUE_OPENED,
	domain.JournalIssueClosed:        pb.ActivityKind_ACTIVITY_KIND_ISSUE_CLOSED,
	domain.JournalTagCreated:         pb.ActivityKind_ACTIVITY_KIND_TAG_CREATED,
	domain.JournalTagDeleted:         pb.ActivityKind_ACTIVITY_KIND_TAG_DELETED,
	domain.JournalReleasePublished:   pb.ActivityKind_ACTIVITY_KIND_RELEASE_PUBLISHED,
	domain.JournalDefaultBranchPush:  pb.ActivityKind_ACTIVITY_KIND_PUSH,
}

func journalKindFromPB(k pb.ActivityKind) (domain.JournalKind, error) {
	for d, p := range journalKindToPB {
		if p == k {
			return d, nil
		}
	}
	return "", fmt.Errorf("%w: activity kind %v", domain.ErrInvalid, k)
}

func activityEntryToPB(e domain.JournalEntry) *pb.ActivityEntry {
	out := &pb.ActivityEntry{
		Id: e.ID, Kind: journalKindToPB[e.Kind], Repo: e.Repo.String(), At: timestamp(e.At),
		Actor: e.Actor, Number: e.Number, Ref: e.Ref, Title: e.Title,
		CommitCount: int32(min(e.Commits, domain.MaxJournalCommits)), CommitsCapped: e.More, Summary: e.Summary,
	}
	if !e.Commit.IsZero() {
		out.Commit = e.Commit.String()
	}
	return out
}

func unreleasedToPB(u ports.UnreleasedRepo) *pb.UnreleasedRepo {
	return &pb.UnreleasedRepo{
		Repo: u.Repo.String(), LatestTag: u.Distance.Tag.Name, LatestTagAt: timestamp(u.Distance.Tag.At),
		CommitsSince: int32(min(u.Distance.Commits, domain.MaxTagDistance)), CommitsCapped: u.Distance.More,
		DefaultBranch: u.Branch,
	}
}

// optionalRepo parses an optional repository field; absent is zero.
func optionalRepo(s *string) (domain.RepoName, error) {
	if s == nil {
		return domain.RepoName{}, nil
	}
	return domain.ParseRepoName(*s)
}

func (s *journalService) ListActivity(ctx context.Context, req *connect.Request[pb.ListActivityRequest]) (*connect.Response[pb.ListActivityResponse], error) {
	m := req.Msg
	repo, err := optionalRepo(m.Repo)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	q := ports.ActivityQuery{Repo: repo, PageSize: int(m.GetPageSize()), PageToken: m.GetPageToken()}
	if m.GetSince() != nil {
		if err := m.GetSince().CheckValid(); err != nil {
			return nil, connectError(ctx, fmt.Errorf("%w: since: %w", domain.ErrInvalid, err))
		}
		q.Since = m.GetSince().AsTime()
	}
	for _, k := range m.GetKinds() {
		kind, err := journalKindFromPB(k)
		if err != nil {
			return nil, connectError(ctx, err)
		}
		q.Kinds = append(q.Kinds, kind)
	}
	page, err := s.journal.ListActivity(ctx, caller(ctx), q)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListActivityResponse{NextPageToken: page.NextPageToken}
	for _, e := range page.Entries {
		out.Entries = append(out.Entries, activityEntryToPB(e))
	}
	return connect.NewResponse(out), nil
}

// errNoJournal answers ListUnreleased on a server without the journal.
var errNoJournal = errors.New("the server has no journal")

func (s *releaseService) ListUnreleased(ctx context.Context, req *connect.Request[pb.ListUnreleasedRequest]) (*connect.Response[pb.ListUnreleasedResponse], error) {
	if s.journal == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errNoJournal)
	}
	repo, err := optionalRepo(req.Msg.Repo)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	list, err := s.journal.ListUnreleased(ctx, caller(ctx), repo)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListUnreleasedResponse{}
	for _, u := range list {
		out.Repos = append(out.Repos, unreleasedToPB(u))
	}
	return connect.NewResponse(out), nil
}
