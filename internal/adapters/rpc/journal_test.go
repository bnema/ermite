// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// Every domain kind has a contract kind and back.
func TestJournalKindsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, k := range domain.JournalKinds() {
		p, ok := journalKindToPB[k]
		require.True(t, ok, k)
		back, err := journalKindFromPB(p)
		require.NoError(t, err)
		assert.Equal(t, k, back)
	}
	assert.Len(t, journalKindToPB, len(pb.ActivityKind_name)-1, "every contract kind but UNSPECIFIED")
	_, err := journalKindFromPB(pb.ActivityKind_ACTIVITY_KIND_UNSPECIFIED)
	assert.True(t, domain.IsInvalid(err))
}

func TestActivityEntryToPB(t *testing.T) {
	t.Parallel()
	repo := mustRepoName(t, "app")
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	head, err := domain.ParseObjectID("0123456789abcdef0123456789abcdef01234567")
	require.NoError(t, err)
	got := activityEntryToPB(domain.JournalEntry{
		ID: "0000000000000001000000000000000a", At: at, Repo: repo, Kind: domain.JournalDefaultBranchPush,
		Actor: "claude", Ref: "main", Commits: domain.MaxJournalCommits, More: true, Commit: head, Summary: "fix: typo",
	})
	assert.Equal(t, &pb.ActivityEntry{
		Id: "0000000000000001000000000000000a", Kind: pb.ActivityKind_ACTIVITY_KIND_PUSH, Repo: "app", At: timestamppb.New(at),
		Actor: "claude", Ref: "main", CommitCount: 100, CommitsCapped: true, Commit: head.String(), Summary: "fix: typo",
	}, got)
	mr := activityEntryToPB(domain.JournalEntry{Repo: repo, Kind: domain.JournalMergeRequestMerged, Number: 4, Title: "Add login", Ref: "main"})
	assert.Equal(t, pb.ActivityKind_ACTIVITY_KIND_MR_MERGED, mr.GetKind())
	assert.EqualValues(t, 4, mr.GetNumber())
	assert.Empty(t, mr.GetCommit())
	assert.Nil(t, mr.GetAt())
}

func TestUnreleasedToPB(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	got := unreleasedToPB(ports.UnreleasedRepo{Repo: mustRepoName(t, "api"), Branch: "main", Distance: domain.TagDistance{Tag: domain.TagInfo{Name: "v0.12.0", At: at}, Commits: 4}})
	assert.Equal(t, &pb.UnreleasedRepo{Repo: "api", LatestTag: "v0.12.0", LatestTagAt: timestamppb.New(at), CommitsSince: 4, DefaultBranch: "main"}, got)
	capped := unreleasedToPB(ports.UnreleasedRepo{Repo: mustRepoName(t, "big"), Branch: "trunk", Distance: domain.TagDistance{Commits: domain.MaxTagDistance, More: true}})
	assert.True(t, capped.GetCommitsCapped())
	assert.EqualValues(t, 500, capped.GetCommitsSince())
	assert.Nil(t, capped.GetLatestTagAt())
}

// The request reaches the use case as a query; its errors keep their codes.
func TestJournalServiceListActivity(t *testing.T) {
	t.Parallel()
	svc := mocks.NewJournalService(t)
	s := &journalService{svc}
	ctx := ownerCtx()
	since := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	repo := mustRepoName(t, "app")
	app := "app"
	svc.EXPECT().ListActivity(mock.Anything, domain.Owner(), ports.ActivityQuery{
		Since: since, Repo: repo, Kinds: []domain.JournalKind{domain.JournalMergeRequestMerged, domain.JournalTagCreated}, PageSize: 7, PageToken: "tok",
	}).Return(ports.ActivityPage{Entries: []domain.JournalEntry{{ID: "x", Repo: repo, Kind: domain.JournalTagCreated, Ref: "v1"}}, NextPageToken: "next"}, nil).Once()
	res, err := s.ListActivity(ctx, connect.NewRequest(&pb.ListActivityRequest{
		Since: timestamppb.New(since), Repo: &app, PageSize: 7, PageToken: "tok",
		Kinds: []pb.ActivityKind{pb.ActivityKind_ACTIVITY_KIND_MR_MERGED, pb.ActivityKind_ACTIVITY_KIND_TAG_CREATED},
	}))
	require.NoError(t, err)
	assert.Equal(t, "next", res.Msg.GetNextPageToken())
	require.Len(t, res.Msg.GetEntries(), 1)
	assert.Equal(t, "v1", res.Msg.GetEntries()[0].GetRef())

	// Refused before the use case: a bad repository, an unspecified kind, an
	// invalid timestamp.
	bad := "Not Valid"
	for name, req := range map[string]*pb.ListActivityRequest{
		"repo":  {Since: timestamppb.New(since), Repo: &bad},
		"kind":  {Since: timestamppb.New(since), Kinds: []pb.ActivityKind{pb.ActivityKind_ACTIVITY_KIND_UNSPECIFIED}},
		"since": {Since: &timestamppb.Timestamp{Nanos: -1}},
	} {
		_, err := s.ListActivity(ctx, connect.NewRequest(req))
		require.Error(t, err, name)
		assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), name)
	}

	for _, tc := range []struct {
		err  error
		code connect.Code
	}{
		{domain.ErrUnauthenticated, connect.CodeUnauthenticated},
		{domain.ErrForbidden, connect.CodePermissionDenied},
		{domain.ErrNotFound, connect.CodeNotFound},
		{domain.ErrInvalidPageToken, connect.CodeInvalidArgument},
	} {
		svc.EXPECT().ListActivity(mock.Anything, domain.Owner(), ports.ActivityQuery{}).Return(ports.ActivityPage{}, tc.err).Once()
		_, err := s.ListActivity(ctx, connect.NewRequest(&pb.ListActivityRequest{}))
		requireCode(t, tc.code, err)
	}
}

func TestReleaseServiceListUnreleased(t *testing.T) {
	t.Parallel()
	svc := mocks.NewJournalService(t)
	s := &releaseService{journal: svc}
	ctx := ownerCtx()
	repo := mustRepoName(t, "app")
	svc.EXPECT().ListUnreleased(mock.Anything, domain.Owner(), domain.RepoName{}).
		Return([]ports.UnreleasedRepo{{Repo: repo, Branch: "main", Distance: domain.TagDistance{Commits: 2, Tag: domain.TagInfo{Name: "v1"}}}}, nil).Once()
	res, err := s.ListUnreleased(ctx, connect.NewRequest(&pb.ListUnreleasedRequest{}))
	require.NoError(t, err)
	require.Len(t, res.Msg.GetRepos(), 1)
	assert.Equal(t, "v1", res.Msg.GetRepos()[0].GetLatestTag())

	app := "app"
	svc.EXPECT().ListUnreleased(mock.Anything, domain.Owner(), repo).Return(nil, domain.ErrForbidden).Once()
	_, err = s.ListUnreleased(ctx, connect.NewRequest(&pb.ListUnreleasedRequest{Repo: &app}))
	requireCode(t, connect.CodePermissionDenied, err)

	_, err = (&releaseService{}).ListUnreleased(ctx, connect.NewRequest(&pb.ListUnreleasedRequest{}))
	requireCode(t, connect.CodeUnimplemented, err)
}

// "journal" is listed, and JournalService served, only with both the journal
// and the releases; without them ListUnreleased answers unimplemented.
func TestAPIListsJournalFeature(t *testing.T) {
	t.Parallel()
	features := func(cfg Config) ([]string, *httptest.Server) {
		cfg.Sessions = mocks.NewSessionService(t)
		cfg.SigningKey = mocks.NewSigningKeyService(t)
		srv := httptest.NewServer(New(cfg).Handler())
		t.Cleanup(srv.Close)
		c := ermiteconnect.NewServerServiceClient(srv.Client(), srv.URL)
		resp, err := c.GetVersion(context.Background(), connect.NewRequest(&pb.GetVersionRequest{}))
		require.NoError(t, err)
		return resp.Msg.GetFeatures(), srv
	}
	got, _ := features(Config{Journal: mocks.NewJournalService(t)})
	assert.NotContains(t, got, FeatureJournal)
	got, srv := features(Config{Releases: mocks.NewReleaseService(t)})
	assert.NotContains(t, got, FeatureJournal)
	_, err := ermiteconnect.NewReleaseServiceClient(srv.Client(), srv.URL).ListUnreleased(context.Background(), connect.NewRequest(&pb.ListUnreleasedRequest{}))
	requireCode(t, connect.CodeUnimplemented, err)
	got, _ = features(Config{Releases: mocks.NewReleaseService(t), Journal: mocks.NewJournalService(t)})
	assert.Contains(t, got, FeatureJournal)
}
