// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Every value of the domain maps to a real value of the contract, never to
// UNSPECIFIED, and the mappers that have an inverse round-trip. A value added
// to the domain and forgotten here would reach clients as "not set".
func TestEnumMappersCoverEveryDomainValue(t *testing.T) {
	t.Parallel()
	t.Run("visibility", func(t *testing.T) {
		t.Parallel()
		for _, v := range []domain.Visibility{domain.VisibilityPublic, domain.VisibilityPrivate} {
			got := visibilityToPB(v)
			assert.NotEqual(t, pb.Visibility_VISIBILITY_UNSPECIFIED, got, v)
			back, err := visibilityFromPB(got)
			require.NoError(t, err, v)
			assert.Equal(t, v, back)
		}
		assert.Equal(t, pb.Visibility_VISIBILITY_UNSPECIFIED, visibilityToPB(""))
		_, err := visibilityFromPB(pb.Visibility(99))
		assert.ErrorIs(t, err, domain.ErrInvalidVisibility)
	})

	t.Run("scope", func(t *testing.T) {
		t.Parallel()
		for _, s := range domain.AllScopes() {
			got, ok := scopeToPB[s]
			require.True(t, ok, "scope %s has no counterpart in the contract", s)
			assert.NotEqual(t, pb.Scope_SCOPE_UNSPECIFIED, got, s)
			back, err := scopeFromPB(got)
			require.NoError(t, err, s)
			assert.Equal(t, s, back)
		}
		_, err := scopeFromPB(pb.Scope_SCOPE_UNSPECIFIED)
		assert.ErrorIs(t, err, domain.ErrInvalidScope)
		// Every scope of the contract is a scope of the domain.
		for v := range pb.Scope_name {
			if pb.Scope(v) == pb.Scope_SCOPE_UNSPECIFIED {
				continue
			}
			_, err := scopeFromPB(pb.Scope(v))
			assert.NoError(t, err, pb.Scope(v))
		}
	})

	t.Run("ref kind", func(t *testing.T) {
		t.Parallel()
		for _, k := range []domain.RefKind{domain.RefKindHead, domain.RefKindBranch, domain.RefKindTag} {
			got := refKindToPB(k)
			assert.NotEqual(t, pb.RefKind_REF_KIND_UNSPECIFIED, got, k)
			back, err := refKindFromPB(got)
			require.NoError(t, err, k)
			assert.Equal(t, k, back)
		}
		_, err := refKindFromPB(pb.RefKind_REF_KIND_UNSPECIFIED)
		assert.ErrorIs(t, err, domain.ErrInvalidRefKind)
	})

	t.Run("entry kind", func(t *testing.T) {
		t.Parallel()
		seen := map[pb.EntryKind]domain.EntryKind{}
		for _, k := range []domain.EntryKind{
			domain.EntryFile, domain.EntryExecutable, domain.EntryDirectory, domain.EntrySymlink, domain.EntrySubmodule,
		} {
			got := entryKindToPB(k)
			assert.NotEqual(t, pb.EntryKind_ENTRY_KIND_UNSPECIFIED, got, k)
			assert.NotContains(t, seen, got, "%s and %s share a value", k, seen[got])
			seen[got] = k
		}
	})

	t.Run("file status", func(t *testing.T) {
		t.Parallel()
		seen := map[pb.FileStatus]domain.FileStatus{}
		for _, s := range []domain.FileStatus{
			domain.FileAdded, domain.FileDeleted, domain.FileModified, domain.FileRenamed, domain.FileCopied, domain.FileTypeChanged,
		} {
			got := fileStatusToPB(s)
			assert.NotEqual(t, pb.FileStatus_FILE_STATUS_UNSPECIFIED, got, s)
			assert.NotContains(t, seen, got, "%s and %s share a value", s, seen[got])
			seen[got] = s
		}
	})

	t.Run("line kind", func(t *testing.T) {
		t.Parallel()
		seen := map[pb.LineKind]domain.LineKind{}
		for _, k := range []domain.LineKind{domain.LineContext, domain.LineAddition, domain.LineDeletion} {
			got := lineKindToPB(k)
			assert.NotEqual(t, pb.LineKind_LINE_KIND_UNSPECIFIED, got, k)
			assert.NotContains(t, seen, got, "%s and %s share a value", k, seen[got])
			seen[got] = k
		}
	})

	// Every value of the contract except UNSPECIFIED is produced by some value
	// of the domain: nothing in the contract is unreachable.
	t.Run("the contract has no value the domain cannot produce", func(t *testing.T) {
		t.Parallel()
		produced := func(names map[int32]string, from func(v int32) bool) {
			for v, n := range names {
				if v == 0 {
					continue
				}
				assert.True(t, from(v), "%s is never produced", n)
			}
		}
		produced(pb.EntryKind_name, func(v int32) bool {
			for _, k := range []domain.EntryKind{domain.EntryFile, domain.EntryExecutable, domain.EntryDirectory, domain.EntrySymlink, domain.EntrySubmodule} {
				if entryKindToPB(k) == pb.EntryKind(v) {
					return true
				}
			}
			return false
		})
		produced(pb.FileStatus_name, func(v int32) bool {
			for _, k := range []domain.FileStatus{domain.FileAdded, domain.FileDeleted, domain.FileModified, domain.FileRenamed, domain.FileCopied, domain.FileTypeChanged} {
				if fileStatusToPB(k) == pb.FileStatus(v) {
					return true
				}
			}
			return false
		})
		produced(pb.LineKind_name, func(v int32) bool {
			for _, k := range []domain.LineKind{domain.LineContext, domain.LineAddition, domain.LineDeletion} {
				if lineKindToPB(k) == pb.LineKind(v) {
					return true
				}
			}
			return false
		})
	})
}

func TestMergeModeMappers(t *testing.T) {
	t.Parallel()
	for _, m := range domain.MergeModes() {
		got := mergeModeToPB(m)
		assert.NotEqual(t, pb.MergeMode_MERGE_MODE_UNSPECIFIED, got, m)
		back, err := mergeModeFromPB(got)
		require.NoError(t, err, m)
		assert.Equal(t, m, back)
	}
	// Unspecified in a merge request is the default mode, left to the use case;
	// in the settings it is refused.
	m, err := mergeModeFromPB(pb.MergeMode_MERGE_MODE_UNSPECIFIED)
	require.NoError(t, err)
	assert.Empty(t, m)
	_, err = mergeModesFromPB([]pb.MergeMode{pb.MergeMode_MERGE_MODE_UNSPECIFIED})
	assert.ErrorIs(t, err, domain.ErrInvalid)
	_, err = mergeModeFromPB(pb.MergeMode(99))
	assert.ErrorIs(t, err, domain.ErrInvalid)
	// Every mode of the contract is known.
	for v := range pb.MergeMode_name {
		if pb.MergeMode(v) != pb.MergeMode_MERGE_MODE_UNSPECIFIED {
			_, err := mergeModeFromPB(pb.MergeMode(v))
			assert.NoError(t, err, pb.MergeMode(v))
		}
	}
}

func TestMergeRequestEnumMappers(t *testing.T) {
	t.Parallel()
	for _, s := range []domain.MRState{domain.MRStateOpen, domain.MRStateMerged, domain.MRStateClosed} {
		got := mergeRequestStateToPB(s)
		assert.NotEqual(t, pb.MergeRequestState_MERGE_REQUEST_STATE_UNSPECIFIED, got, s)
		back, err := mergeRequestStateFromPB(got)
		require.NoError(t, err, s)
		assert.Equal(t, s, back)
	}
	all, err := mergeRequestStateFromPB(pb.MergeRequestState_MERGE_REQUEST_STATE_UNSPECIFIED)
	require.NoError(t, err)
	assert.Empty(t, all, "unspecified means every state")
	_, err = mergeRequestStateFromPB(99)
	assert.ErrorIs(t, err, domain.ErrInvalid)

	for _, s := range []domain.DiffSide{domain.SideOld, domain.SideNew, domain.SideNone} {
		back, err := diffSideFromPB(diffSideToPB(s))
		require.NoError(t, err, s)
		assert.Equal(t, s, back)
	}
	assert.Equal(t, pb.DiffSide_DIFF_SIDE_UNSPECIFIED, diffSideToPB(domain.SideNone), "a general comment")
	_, err = diffSideFromPB(99)
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

func TestCommentID(t *testing.T) {
	t.Parallel()
	assert.EqualValues(t, 7, commentID("7"))
	for _, bad := range []string{"", "0", "-1", "07", "+7", "x", "7 ", "9223372036854775808"} {
		assert.Zero(t, commentID(bad), "%q names no comment", bad)
	}
}

func TestRepoToPBAlwaysHasSettings(t *testing.T) {
	t.Parallel()
	// A repository value without settings (never stored with them) reads as the defaults.
	r := repoToPB(ports.RepoView{Repo: domain.Repo{Name: mustName(t, "x")}})
	assert.Equal(t, []pb.MergeMode{pb.MergeMode_MERGE_MODE_FAST_FORWARD, pb.MergeMode_MERGE_MODE_SQUASH, pb.MergeMode_MERGE_MODE_MERGE_COMMIT}, r.GetSettings().GetAllowedMergeModes())
	assert.False(t, r.GetSettings().GetAllowAutoMerge())

	custom := domain.RepoSettings{AllowedMergeModes: []domain.MergeMode{domain.MergeModeSquash}}
	r = repoToPB(ports.RepoView{Repo: domain.Repo{Name: mustName(t, "x"), Settings: custom}})
	assert.Equal(t, []pb.MergeMode{pb.MergeMode_MERGE_MODE_SQUASH}, r.GetSettings().GetAllowedMergeModes())
	assert.False(t, r.GetSettings().GetAllowAutoMerge())
}

func mustName(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}

func TestUpdateParamsSettingsPaths(t *testing.T) {
	t.Parallel()
	msg := func(paths ...string) *pb.UpdateRepoRequest {
		return &pb.UpdateRepoRequest{
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
			Settings: &pb.RepoSettings{
				AllowedMergeModes: []pb.MergeMode{pb.MergeMode_MERGE_MODE_SQUASH},
				AllowAutoMerge:    true,
			},
		}
	}
	p, err := updateParams(msg("settings"))
	require.NoError(t, err)
	require.NotNil(t, p.AllowedMergeModes)
	assert.Equal(t, []domain.MergeMode{domain.MergeModeSquash}, *p.AllowedMergeModes)
	require.NotNil(t, p.AllowAutoMerge)
	assert.True(t, *p.AllowAutoMerge)

	p, err = updateParams(msg("settings.allowed_merge_modes"))
	require.NoError(t, err)
	assert.NotNil(t, p.AllowedMergeModes)
	assert.Nil(t, p.AllowAutoMerge, "not named: left alone")

	p, err = updateParams(msg("settings.allow_auto_merge"))
	require.NoError(t, err)
	assert.Nil(t, p.AllowedMergeModes)
	assert.NotNil(t, p.AllowAutoMerge)

	// Modes named with nothing to take them from are an empty list, which the use case refuses.
	p, err = updateParams(&pb.UpdateRepoRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"settings.allowed_merge_modes"}}})
	require.NoError(t, err)
	require.NotNil(t, p.AllowedMergeModes)
	assert.Empty(t, *p.AllowedMergeModes)

	_, err = updateParams(msg("settings.other"))
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

func TestConnectErrorCarriesTheFieldsOfARejection(t *testing.T) {
	t.Parallel()
	var v domain.Violations
	v.Add("title", "must be one line")
	v.Add("source_branch", "does not exist")
	err := connectError(context.Background(), fmt.Errorf("create: %w", v.Err()))
	var ce *connect.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, connect.CodeInvalidArgument, ce.Code())
	require.Len(t, ce.Details(), 1)
	val, derr := ce.Details()[0].Value()
	require.NoError(t, derr)
	br, ok := val.(*errdetails.BadRequest)
	require.True(t, ok)
	require.Len(t, br.GetFieldViolations(), 2)
	assert.Equal(t, "title", br.GetFieldViolations()[0].GetField())
	assert.Equal(t, "must be one line", br.GetFieldViolations()[0].GetDescription())

	// Other errors carry no detail, and a text error alone is invalid_argument.
	err = connectError(context.Background(), domain.ErrNotFound)
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Details())
	_, perr := domain.ParsePlainText("\x07")
	err = connectError(context.Background(), perr)
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, connect.CodeInvalidArgument, ce.Code())
}

func TestWatchEventToPBMergeRequestEvents(t *testing.T) {
	t.Parallel()
	repo := mustName(t, "proj")
	title, err := domain.ParseMRTitle("Add x")
	require.NoError(t, err)
	mr := domain.MergeRequest{Repo: repo, Number: 4, Title: title, Source: "feat", Target: "main", State: domain.MRStateMerged, MergeMode: domain.MergeModeSquash}
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		ev   domain.Event
		pick func(*pb.Event) *pb.MergeRequest
	}{
		"opened": {domain.MergeRequestOpened{MergeRequest: mr, At: at}, func(e *pb.Event) *pb.MergeRequest { return e.GetMergeRequestOpened().GetMergeRequest() }},
		"merged": {domain.MergeRequestMerged{MergeRequest: mr, At: at}, func(e *pb.Event) *pb.MergeRequest { return e.GetMergeRequestMerged().GetMergeRequest() }},
		"closed": {domain.MergeRequestClosed{MergeRequest: mr, At: at}, func(e *pb.Event) *pb.MergeRequest { return e.GetMergeRequestClosed().GetMergeRequest() }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := watchEventToPB(ports.WatchEvent{ID: "e1", Event: tc.ev})
			require.NotNil(t, got)
			assert.Equal(t, "e1", got.GetId())
			assert.Equal(t, at, got.GetTime().AsTime())
			m := tc.pick(got)
			require.NotNil(t, m)
			assert.Equal(t, "proj", m.GetRepo())
			assert.EqualValues(t, 4, m.GetNumber())
			assert.Equal(t, "Add x", m.GetTitle())
			assert.Equal(t, pb.MergeMode_MERGE_MODE_SQUASH, m.GetMergeMode())
		})
	}

	c := domain.Comment{ID: 9, Author: "root", Path: "a.txt", Line: 3, Side: domain.SideOld, CreatedAt: at}
	got := watchEventToPB(ports.WatchEvent{ID: "e2", Event: domain.CommentAdded{Repo: repo, Number: 4, Comment: c, At: at}})
	added := got.GetCommentAdded()
	require.NotNil(t, added)
	assert.Equal(t, "proj", added.GetRepo())
	assert.EqualValues(t, 4, added.GetNumber())
	assert.Equal(t, "9", added.GetComment().GetId())
	assert.Equal(t, pb.DiffSide_DIFF_SIDE_OLD, added.GetComment().GetSide())
	assert.EqualValues(t, 3, added.GetComment().GetLine())
}

func TestIssueMappers(t *testing.T) {
	t.Parallel()
	for _, m := range domain.IssuesModes() {
		got, err := issuesModeFromPB(issuesModeToPB(m))
		require.NoError(t, err)
		assert.Equal(t, m, got)
	}
	assert.Equal(t, pb.IssuesMode_ISSUES_MODE_OFF, issuesModeToPB(""), "the empty mode is off")
	_, err := issuesModeFromPB(pb.IssuesMode_ISSUES_MODE_UNSPECIFIED)
	assert.ErrorIs(t, err, domain.ErrInvalid)

	s, err := issueStateFromPB(pb.IssueState_ISSUE_STATE_UNSPECIFIED)
	require.NoError(t, err)
	assert.Empty(t, s)
	_, err = issueStateFromPB(pb.IssueState_ISSUE_STATE_PENDING)
	assert.ErrorIs(t, err, domain.ErrInvalid)

	n, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	ev := watchEventToPB(ports.WatchEvent{ID: "1", Event: domain.IssueClosed{Issue: domain.Issue{Repo: n, Number: 4, State: domain.IssueStateClosed}}})
	require.NotNil(t, ev.GetIssueClosed())
	assert.EqualValues(t, 4, ev.GetIssueClosed().GetIssue().GetNumber())
	assert.Equal(t, pb.IssueState_ISSUE_STATE_CLOSED, ev.GetIssueClosed().GetIssue().GetState())
	ev = watchEventToPB(ports.WatchEvent{ID: "2", Event: domain.IssueCommented{Repo: n, Number: 4, Comment: domain.IssueComment{ID: 3}}})
	assert.Equal(t, "3", ev.GetIssueCommented().GetComment().GetId())
	assert.NotNil(t, watchEventToPB(ports.WatchEvent{Event: domain.IssueOpened{}}).GetIssueOpened())
	assert.NotNil(t, watchEventToPB(ports.WatchEvent{Event: domain.IssueReopened{}}).GetIssueReopened())
}

func TestWatchEventToPBSkipsPendingRemoved(t *testing.T) {
	t.Parallel()
	assert.Nil(t, watchEventToPB(ports.WatchEvent{Event: domain.IssuePendingRemoved{}}))
}

func TestIssueStateToPBNeverPending(t *testing.T) {
	t.Parallel()
	assert.Equal(t, pb.IssueState_ISSUE_STATE_UNSPECIFIED, issueStateToPB(domain.IssueState("pending")))
	assert.Equal(t, pb.IssueState_ISSUE_STATE_OPEN, issueStateToPB(domain.IssueStateOpen))
}

func TestMergeRequestToPBMergeabilityOnlyWhileOpen(t *testing.T) {
	t.Parallel()
	repo := mustName(t, "proj")
	for state, want := range map[domain.MRState]bool{domain.MRStateOpen: true, domain.MRStateMerged: false, domain.MRStateClosed: false} {
		mr := domain.MergeRequest{Repo: repo, Number: 4, State: state}
		got := mergeRequestToPB(mr).GetMergeability()
		if !want {
			assert.Nil(t, got, state)
			continue
		}
		require.NotNil(t, got)
		assert.Equal(t, pb.MergeabilityState_MERGEABILITY_STATE_CHECKING, got.GetState())
		ev := watchEventToPB(ports.WatchEvent{ID: "e", Event: domain.MergeRequestOpened{MergeRequest: mr}})
		assert.Equal(t, pb.MergeabilityState_MERGEABILITY_STATE_CHECKING, ev.GetMergeRequestOpened().GetMergeRequest().GetMergeability().GetState())
	}
	for _, ev := range []domain.Event{domain.MergeRequestMerged{MergeRequest: domain.MergeRequest{Repo: repo, State: domain.MRStateMerged}}, domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: repo, State: domain.MRStateClosed}}} {
		got := watchEventToPB(ports.WatchEvent{ID: "e", Event: ev})
		assert.Nil(t, got.GetMergeRequestMerged().GetMergeRequest().GetMergeability())
		assert.Nil(t, got.GetMergeRequestClosed().GetMergeRequest().GetMergeability())
	}
}

func TestFileDiffOmissionMapping(t *testing.T) {
	for o, want := range map[domain.FileOmission]pb.FileOmission{
		domain.OmitNone:        pb.FileOmission_FILE_OMISSION_UNSPECIFIED,
		domain.OmitGenerated:   pb.FileOmission_FILE_OMISSION_GENERATED,
		domain.OmitLarge:       pb.FileOmission_FILE_OMISSION_LARGE,
		domain.OmitBudget:      pb.FileOmission_FILE_OMISSION_BUDGET,
		domain.OmitUnavailable: pb.FileOmission_FILE_OMISSION_UNAVAILABLE,
	} {
		f := fileDiffToPB(domain.FileDiff{NewPath: "a", Omitted: o})
		assert.Equal(t, want, f.GetOmitted())
		assert.Equal(t, o != domain.OmitNone, f.GetTruncated())
		d := diffToPB(domain.Diff{Files: []domain.FileDiff{{Omitted: o}}})
		assert.Equal(t, o != domain.OmitNone, d.GetTruncated())
	}
	assert.True(t, diffToPB(domain.Diff{Truncated: true}).GetTruncated())
	assert.False(t, diffToPB(domain.Diff{Files: []domain.FileDiff{{}}}).GetTruncated())
}

func TestResolvedIssueContract(t *testing.T) {
	title, err := domain.ParseMRTitle("Fix")
	require.NoError(t, err)
	for _, state := range []domain.IssueState{domain.IssueStateOpen, domain.IssueStateClosed, domain.IssueStateResolved} {
		back, err := issueStateFromPB(issueStateToPB(state))
		require.NoError(t, err)
		assert.Equal(t, state, back)
		assert.NotEqual(t, pb.MergeRequestIssueState_MERGE_REQUEST_ISSUE_STATE_UNSPECIFIED, mergeRequestIssueToPB(ports.MergeRequestIssue{State: state}).GetState())
	}
	for kind, want := range map[domain.IssueEventKind]pb.IssueEventKind{
		domain.IssueEventReferenced: pb.IssueEventKind_ISSUE_EVENT_KIND_REFERENCED,
		domain.IssueEventClosed:     pb.IssueEventKind_ISSUE_EVENT_KIND_CLOSED,
		domain.IssueEventReopened:   pb.IssueEventKind_ISSUE_EVENT_KIND_REOPENED,
		domain.IssueEventResolved:   pb.IssueEventKind_ISSUE_EVENT_KIND_RESOLVED,
	} {
		at := time.Now().UTC()
		got := issueEventToPB(ports.IssueEventDetail{Event: domain.IssueEvent{ID: 3, Kind: kind, Issue: 4, MergeRequest: 5, Closes: true, Actor: "merger", CreatedAt: at}, MergeRequestTitle: title})
		assert.Equal(t, want, got.GetKind())
		assert.Equal(t, "3", got.GetId())
		assert.EqualValues(t, 4, got.GetIssue())
		assert.EqualValues(t, 5, got.GetMergeRequest())
		assert.True(t, got.GetCloses())
		assert.Equal(t, "merger", got.GetActor())
		assert.Equal(t, "Fix", got.GetMergeRequestTitle())
		assert.Equal(t, at, got.GetCreatedAt().AsTime())
	}
	ev := watchEventToPB(ports.WatchEvent{Event: domain.IssueResolved{Issue: domain.Issue{Number: 4, State: domain.IssueStateResolved}}})
	require.NotNil(t, ev.GetIssueResolved())
	assert.Equal(t, pb.IssueState_ISSUE_STATE_RESOLVED, ev.GetIssueResolved().GetIssue().GetState())
}
