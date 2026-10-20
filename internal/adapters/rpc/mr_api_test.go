// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"strings"
	"testing"
	"time"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestNoteRPCFields(t *testing.T) {
	now := time.Now().UTC()
	note := domain.Comment{ID: 4, ReplyTo: 2, StartLine: 10, Line: 24, Verdict: domain.VerdictApprove, EditedAt: now, Deleted: true}
	got := commentToPB(note)
	assert.Equal(t, "2", got.GetReplyTo())
	assert.EqualValues(t, 10, got.GetStartLine())
	assert.Equal(t, pb.Verdict_VERDICT_APPROVE, got.GetVerdict())
	assert.Equal(t, now, got.GetEditedAt().AsTime())
	assert.True(t, got.GetDeleted())
	assert.Empty(t, commentToPB(domain.Comment{}).GetReplyTo())
	assert.Nil(t, commentToPB(domain.Comment{}).GetEditedAt())
	for _, v := range []domain.Verdict{domain.VerdictNone, domain.VerdictApprove, domain.VerdictChangesRequested} {
		back, err := verdictFromPB(verdictToPB(v))
		require.NoError(t, err)
		assert.Equal(t, v, back)
	}
	m := mocks.NewMergeRequestService(t)
	m.EXPECT().AddComment(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), ports.AddCommentParams{Text: "note", StartLine: 10, Line: 24, Verdict: domain.VerdictApprove}).Return(note, nil).Once()
	res, err := (&mergeRequestService{m}).AddComment(ownerCtx(), connect.NewRequest(&pb.AddCommentRequest{Repo: "demo", Number: 1, Text: "note", StartLine: 10, Line: 24, Verdict: pb.Verdict_VERDICT_APPROVE}))
	require.NoError(t, err)
	assert.Equal(t, got, res.Msg.GetComment())
	_, err = (&mergeRequestService{m}).AddComment(ownerCtx(), connect.NewRequest(&pb.AddCommentRequest{Repo: "demo", Verdict: pb.Verdict(99)}))
	requireCode(t, connect.CodeInvalidArgument, err)
}

func TestNoteMutationRPCCodes(t *testing.T) {
	for _, edit := range []bool{false, true} {
		for _, failure := range []error{nil, domain.ErrForbidden, domain.ErrPreconditionFailed, domain.ErrNotFound} {
			m := mocks.NewMergeRequestService(t)
			note := domain.Comment{ID: 2, Deleted: !edit}
			var err error
			if edit {
				m.EXPECT().EditComment(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), int64(2), "new").Return(note, failure).Once()
				_, err = (&mergeRequestService{m}).EditComment(ownerCtx(), connect.NewRequest(&pb.EditCommentRequest{Repo: "demo", Number: 1, CommentId: "2", Text: "new"}))
			} else {
				m.EXPECT().DeleteComment(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), int64(2)).Return(note, failure).Once()
				_, err = (&mergeRequestService{m}).DeleteComment(ownerCtx(), connect.NewRequest(&pb.DeleteCommentRequest{Repo: "demo", Number: 1, CommentId: "2"}))
			}
			switch failure {
			case nil:
				require.NoError(t, err)
			case domain.ErrForbidden:
				requireCode(t, connect.CodePermissionDenied, err)
			case domain.ErrPreconditionFailed:
				requireCode(t, connect.CodeFailedPrecondition, err)
			case domain.ErrNotFound:
				requireCode(t, connect.CodeNotFound, err)
			}
		}
	}
}

func TestNoteEventsRPC(t *testing.T) {
	repo := mustRepoName(t, "demo")
	note := domain.Comment{ID: 3, Verdict: domain.VerdictApprove, Deleted: true}
	at := time.Now().UTC()
	events := []domain.Event{
		domain.CommentEdited{Repo: repo, Number: 1, Comment: note, At: at},
		domain.CommentDeleted{Repo: repo, Number: 1, Comment: note, At: at},
		domain.CommentResolved{Repo: repo, Number: 1, Comment: note, At: at},
	}
	for i, ev := range events {
		got := watchEventToPB(ports.WatchEvent{ID: "e", Event: ev})
		require.NotNil(t, got)
		assert.Equal(t, at, got.GetTime().AsTime())
		var c *pb.Comment
		switch i {
		case 0:
			c = got.GetCommentEdited().GetComment()
		case 1:
			c = got.GetCommentDeleted().GetComment()
		case 2:
			c = got.GetCommentResolved().GetComment()
		}
		assert.Equal(t, "3", c.GetId())
		assert.True(t, c.GetDeleted())
		assert.Equal(t, pb.Verdict_VERDICT_APPROVE, c.GetVerdict())
	}
}

func TestNoteReplyIDValidation(t *testing.T) {
	for _, id := range []string{"abc", "0", "-1", "01", "9223372036854775808"} {
		_, err := (&mergeRequestService{mocks.NewMergeRequestService(t)}).AddComment(ownerCtx(), connect.NewRequest(&pb.AddCommentRequest{Repo: "demo", Number: 1, ReplyTo: id, Text: "reply"}))
		requireCode(t, connect.CodeInvalidArgument, err)
		assert.Contains(t, err.Error(), "reply_to")
		assert.Contains(t, err.Error(), "is not a note id")
	}
	m := mocks.NewMergeRequestService(t)
	m.EXPECT().AddComment(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), ports.AddCommentParams{ReplyTo: 2, Text: "reply"}).Return(domain.Comment{ID: 3, ReplyTo: 2}, nil).Once()
	res, err := (&mergeRequestService{m}).AddComment(ownerCtx(), connect.NewRequest(&pb.AddCommentRequest{Repo: "demo", Number: 1, ReplyTo: "2", Text: "reply"}))
	require.NoError(t, err)
	assert.Equal(t, "2", res.Msg.GetComment().GetReplyTo())
}

func TestNoteVerdictsDetailAndScope(t *testing.T) {
	m := mocks.NewMergeRequestService(t)
	m.EXPECT().Get(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), ports.GetMergeRequestOptions{}).Return(ports.MergeRequestDetail{Verdicts: []domain.AuthorVerdict{{Author: "owner", Verdict: domain.VerdictApprove, CommentID: 4, Stale: true}}}, nil).Once()
	res, err := (&mergeRequestService{m}).GetMergeRequest(ownerCtx(), connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "demo", Number: 1}))
	require.NoError(t, err)
	require.Len(t, res.Msg.GetVerdicts(), 1)
	assert.Equal(t, "4", res.Msg.GetVerdicts()[0].GetCommentId())
	assert.True(t, res.Msg.GetVerdicts()[0].GetStale())
	m.EXPECT().AddComment(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), ports.AddCommentParams{Verdict: domain.VerdictApprove}).Return(domain.Comment{}, domain.ErrForbidden).Once()
	_, err = (&mergeRequestService{m}).AddComment(ownerCtx(), connect.NewRequest(&pb.AddCommentRequest{Repo: "demo", Number: 1, Verdict: pb.Verdict_VERDICT_APPROVE}))
	requireCode(t, connect.CodePermissionDenied, err)
}

func TestAutoMergeRPCCodesAndFields(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	head, err := domain.ParseObjectID(strings.Repeat("a", 40))
	require.NoError(t, err)
	armed := domain.MergeRequest{
		Repo: mustRepoName(t, "demo"), Number: 1, State: domain.MRStateOpen, Source: "feat", Target: "main", HeadID: head,
		AutoMerge: &domain.AutoMerge{Mode: domain.MergeModeSquash, HeadID: head, EnablerLabel: "owner", EnabledAt: now},
	}
	idle := armed
	idle.AutoMerge = nil

	pbArmed := mergeRequestToPB(armed)
	require.NotNil(t, pbArmed.GetAutoMerge())
	assert.Equal(t, pb.MergeMode_MERGE_MODE_SQUASH, pbArmed.GetAutoMerge().GetMode())
	assert.Equal(t, head.String(), pbArmed.GetAutoMerge().GetHeadId())
	assert.Equal(t, "owner", pbArmed.GetAutoMerge().GetEnabledBy())
	assert.Equal(t, now, pbArmed.GetAutoMerge().GetEnabledAt().AsTime())
	assert.False(t, pbArmed.GetAutoMerged())
	assert.Nil(t, mergeRequestToPB(idle).GetAutoMerge())
	done := idle
	done.State, done.AutoMerged = domain.MRStateMerged, true
	assert.True(t, mergeRequestToPB(done).GetAutoMerged())

	// Success: the mode and the request reach the service, the fields come back.
	m := mocks.NewMergeRequestService(t)
	m.EXPECT().EnableAutoMerge(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), ports.EnableAutoMergeParams{Mode: domain.MergeModeSquash, HeadID: mustObjectID(t, strings.Repeat("a", domain.ObjectIDLen))}).Return(armed, nil).Once()
	res, err := (&mergeRequestService{m}).EnableAutoMerge(ownerCtx(), connect.NewRequest(&pb.EnableAutoMergeRequest{Repo: "demo", Number: 1, Mode: pb.MergeMode_MERGE_MODE_SQUASH, HeadId: strings.Repeat("a", domain.ObjectIDLen)}))
	require.NoError(t, err)
	assert.Equal(t, pbArmed, res.Msg.GetMergeRequest())
	m.EXPECT().CancelAutoMerge(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1)).Return(idle, nil).Once()
	cres, err := (&mergeRequestService{m}).CancelAutoMerge(ownerCtx(), connect.NewRequest(&pb.CancelAutoMergeRequest{Repo: "demo", Number: 1}))
	require.NoError(t, err)
	assert.Nil(t, cres.Msg.GetMergeRequest().GetAutoMerge())

	// Bad input never reaches the service.
	_, err = (&mergeRequestService{m}).EnableAutoMerge(ownerCtx(), connect.NewRequest(&pb.EnableAutoMergeRequest{Repo: "demo", Number: 1, Mode: pb.MergeMode(99), HeadId: strings.Repeat("a", domain.ObjectIDLen)}))
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = (&mergeRequestService{m}).EnableAutoMerge(ownerCtx(), connect.NewRequest(&pb.EnableAutoMergeRequest{Repo: "demo", Number: 1, HeadId: "abc"}))
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = (&mergeRequestService{m}).CancelAutoMerge(ownerCtx(), connect.NewRequest(&pb.CancelAutoMergeRequest{Repo: "", Number: 1}))
	requireCode(t, connect.CodeInvalidArgument, err)

	// Domain errors map like the other merge request calls.
	for _, tc := range []struct {
		err  error
		code connect.Code
	}{
		{domain.ErrPreconditionFailed, connect.CodeFailedPrecondition},
		{domain.ErrForbidden, connect.CodePermissionDenied},
		{domain.ErrNotFound, connect.CodeNotFound},
	} {
		m := mocks.NewMergeRequestService(t)
		m.EXPECT().EnableAutoMerge(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), mock.Anything).Return(domain.MergeRequest{}, tc.err).Once()
		_, err := (&mergeRequestService{m}).EnableAutoMerge(ownerCtx(), connect.NewRequest(&pb.EnableAutoMergeRequest{Repo: "demo", Number: 1, HeadId: strings.Repeat("a", domain.ObjectIDLen)}))
		requireCode(t, tc.code, err)
		m.EXPECT().CancelAutoMerge(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1)).Return(domain.MergeRequest{}, tc.err).Once()
		_, err = (&mergeRequestService{m}).CancelAutoMerge(ownerCtx(), connect.NewRequest(&pb.CancelAutoMergeRequest{Repo: "demo", Number: 1}))
		requireCode(t, tc.code, err)
	}
}

func TestAutoMergeWatchEventsAndSettingsToPB(t *testing.T) {
	armed := domain.MergeRequest{Repo: mustRepoName(t, "demo"), Number: 1, State: domain.MRStateOpen,
		AutoMerge: &domain.AutoMerge{Mode: domain.MergeModeMerge, EnablerLabel: "owner"}}
	got := watchEventToPB(ports.WatchEvent{ID: "1", Event: domain.MergeRequestAutoMergeEnabled{MergeRequest: armed}})
	require.NotNil(t, got.GetMergeRequestAutoMergeEnabled())
	assert.Equal(t, pb.MergeMode_MERGE_MODE_MERGE_COMMIT, got.GetMergeRequestAutoMergeEnabled().GetMergeRequest().GetAutoMerge().GetMode())
	got = watchEventToPB(ports.WatchEvent{ID: "2", Event: domain.MergeRequestAutoMergeCancelled{MergeRequest: armed, Reason: "the branch moved"}})
	require.NotNil(t, got.GetMergeRequestAutoMergeCancelled())
	assert.Equal(t, "the branch moved", got.GetMergeRequestAutoMergeCancelled().GetReason())
	got = watchEventToPB(ports.WatchEvent{ID: "3", Event: domain.MergeRequestMerged{MergeRequest: armed, Auto: true}})
	assert.True(t, got.GetMergeRequestMerged().GetAuto())
	got = watchEventToPB(ports.WatchEvent{ID: "4", Event: domain.MergeRequestMerged{MergeRequest: armed}})
	assert.False(t, got.GetMergeRequestMerged().GetAuto())

	assert.True(t, settingsToPB(domain.RepoSettings{AllowAutoMerge: true}).GetAllowAutoMerge())
	assert.False(t, settingsToPB(domain.RepoSettings{}).GetAllowAutoMerge())
}
