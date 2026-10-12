// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
)

func evt(id string, kind func(*ermite.Event)) *ermite.WatchResponse {
	e := &ermite.Event{Id: id, Time: ts("2026-10-10T12:00:00Z")}
	kind(e)
	return &ermite.WatchResponse{Event: e}
}

// sampleEvents is one of each kind, plus a kind this client does not know.
func sampleEvents() []*ermite.WatchResponse {
	return []*ermite.WatchResponse{
		evt("1", func(e *ermite.Event) {
			e.Kind = &ermite.Event_RepoCreated{RepoCreated: &ermite.RepoCreated{Repo: repoBeta}}
		}),
		evt("2", func(e *ermite.Event) {
			e.Kind = &ermite.Event_RepoUpdated{RepoUpdated: &ermite.RepoUpdated{Repo: repoAlpha}}
		}),
		evt("3", func(e *ermite.Event) {
			e.Kind = &ermite.Event_Pushed{Pushed: &ermite.Pushed{Repo: "alpha", RefUpdates: []*ermite.RefUpdate{
				{Ref: "refs/heads/main", OldId: "1111111111111111", NewId: "2222222222222222"},
				{Ref: "refs/heads/new", NewId: "3333333333333333"},
				{Ref: "refs/tags/old", OldId: "4444444444444444"},
			}}}
		}),
		evt("4", func(e *ermite.Event) {
			e.Kind = &ermite.Event_RepoDeleted{RepoDeleted: &ermite.RepoDeleted{Repo: "beta"}}
		}),
		{Event: &ermite.Event{Kind: &ermite.Event_Gap{Gap: &ermite.Gap{}}}},
		{Event: &ermite.Event{Id: "future"}}, // unknown kind: skipped
	}
}

// finishing streams events, then answers permission_denied to end the command.
func finishing(events []*ermite.WatchResponse) func(context.Context, *ermite.WatchRequest, *connect.ServerStream[ermite.WatchResponse]) error {
	return func(_ context.Context, _ *ermite.WatchRequest, st *connect.ServerStream[ermite.WatchResponse]) error {
		for _, e := range events {
			if err := st.Send(e); err != nil {
				return err
			}
		}
		return connect.NewError(connect.CodePermissionDenied, errors.New("done"))
	}
}

func TestWatchHumanLines(t *testing.T) {
	f := &fakeAPI{watch: finishing(sampleEvents())}
	res := runCLI(t, envFor(f.serve(t)), "", "watch")
	require.Error(t, res.err)
	assert.Equal(t, ""+
		"2026-10-10T12:00:00Z  repo_created  beta (private)\n"+
		"2026-10-10T12:00:00Z  repo_updated  alpha (public, default branch main)\n"+
		"2026-10-10T12:00:00Z  pushed  alpha refs/heads/main 11111111..22222222, refs/heads/new created at 33333333, refs/tags/old deleted (was 44444444)\n"+
		"2026-10-10T12:00:00Z  repo_deleted  beta\n"+
		"-  gap  events were lost, reload what you display\n", res.stdout)
}

func TestWatchTopics(t *testing.T) {
	f := &fakeAPI{watch: finishing(nil)}
	url := f.serve(t)
	_ = runCLI(t, envFor(url), "", "watch")
	_ = runCLI(t, envFor(url), "", "watch", "alpha")
	_, reqs := f.snapshot()
	require.Len(t, reqs, 2)
	assert.Equal(t, []string{"home"}, reqs[0].(*ermite.WatchRequest).GetTopics())
	assert.Equal(t, []string{"repo:alpha"}, reqs[1].(*ermite.WatchRequest).GetTopics())
	assert.Empty(t, reqs[0].(*ermite.WatchRequest).GetResumeAfter())
}

func TestWatchNDJSON(t *testing.T) {
	f := &fakeAPI{watch: finishing(sampleEvents())}
	res := runCLI(t, envFor(f.serve(t)), "", "watch", "--json")
	require.Error(t, res.err)

	var lines []string
	sc := bufio.NewScanner(strings.NewReader(res.stdout))
	for sc.Scan() {
		lines = append(lines, sc.Text())
		assert.True(t, json.Valid([]byte(sc.Text())), "one JSON value per line: %s", sc.Text())
	}
	require.Len(t, lines, 5)
	assert.JSONEq(t, `{"id":"1","time":"2026-10-10T12:00:00Z","type":"repo_created","repo":"beta","repository":{"name":"beta","description":"","visibility":"private","default_branch":"trunk","created_at":"2026-02-02T00:00:00Z","pushed_at":null,"merge_modes":["squash"],"failing_checks_block":false}}`, lines[0])
	assert.JSONEq(t, `{"id":"3","time":"2026-10-10T12:00:00Z","type":"pushed","repo":"alpha","ref_updates":[
		{"ref":"refs/heads/main","old_id":"1111111111111111","new_id":"2222222222222222"},
		{"ref":"refs/heads/new","old_id":"","new_id":"3333333333333333"},
		{"ref":"refs/tags/old","old_id":"4444444444444444","new_id":""}]}`, lines[2])
	assert.JSONEq(t, `{"id":"4","time":"2026-10-10T12:00:00Z","type":"repo_deleted","repo":"beta"}`, lines[3])
	assert.JSONEq(t, `{"id":"","time":null,"type":"gap"}`, lines[4])
}

func TestWatchReconnectsAndResumes(t *testing.T) {
	calls := 0
	f := &fakeAPI{}
	f.watch = func(_ context.Context, req *ermite.WatchRequest, st *connect.ServerStream[ermite.WatchResponse]) error {
		calls++
		switch calls {
		case 1:
			_ = st.Send(sampleEvents()[0])
			return connect.NewError(connect.CodeUnavailable, errors.New("restarting"))
		case 2:
			assert.Equal(t, "1", req.GetResumeAfter(), "resumes after the last printed event")
			_ = st.Send(&ermite.WatchResponse{Event: &ermite.Event{Kind: &ermite.Event_Gap{Gap: &ermite.Gap{}}}})
			_ = st.Send(sampleEvents()[3])
			return connect.NewError(connect.CodePermissionDenied, errors.New("done"))
		}
		return connect.NewError(connect.CodeInternal, nil)
	}
	res := runCLI(t, envFor(f.serve(t)), "", "watch")
	require.Error(t, res.err)
	assert.Equal(t, 2, calls)
	assert.Contains(t, res.stderr, "ermite: connection lost")
	assert.Contains(t, res.stderr, "restarting")
	assert.Contains(t, res.stderr, "ermite: reconnected")
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], "repo_created")
	assert.Contains(t, lines[1], "gap")
	assert.Contains(t, lines[2], "repo_deleted")
}

func TestWatchPermanentErrorIsFriendly(t *testing.T) {
	f := &fakeAPI{watch: func(context.Context, *ermite.WatchRequest, *connect.ServerStream[ermite.WatchResponse]) error {
		return connect.NewError(connect.CodeNotFound, errors.New("repository \"zzz\" not found"))
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "watch", "zzz")
	require.Error(t, res.err)
	assert.Equal(t, "not found: repository \"zzz\" not found", res.err.Error())
}

func TestWatchRejectsTooManyArgs(t *testing.T) {
	res := runCLI(t, envFor("http://127.0.0.1:1"), "", "watch", "a", "b")
	require.Error(t, res.err)
}

func TestWatchMergeRequestEvents(t *testing.T) {
	cmt := &ermite.Comment{Id: "2", Author: "agent-7", Path: "a.txt", Line: 5, Side: ermite.DiffSide_DIFF_SIDE_NEW, Text: "why?"}
	events := []*ermite.WatchResponse{
		evt("1", func(e *ermite.Event) {
			e.Kind = &ermite.Event_MergeRequestOpened{MergeRequestOpened: &ermite.MergeRequestOpened{MergeRequest: mrOne}}
		}),
		evt("2", func(e *ermite.Event) {
			e.Kind = &ermite.Event_CommentAdded{CommentAdded: &ermite.CommentAdded{Repo: "alpha", Number: 1, Comment: cmt}}
		}),
		evt("3", func(e *ermite.Event) {
			e.Kind = &ermite.Event_MergeRequestMerged{MergeRequestMerged: &ermite.MergeRequestMerged{MergeRequest: mrOne}}
		}),
		evt("4", func(e *ermite.Event) {
			e.Kind = &ermite.Event_MergeRequestClosed{MergeRequestClosed: &ermite.MergeRequestClosed{MergeRequest: mrOne}}
		}),
	}
	f := &fakeAPI{watch: finishing(events)}
	url := f.serve(t)

	res := runCLI(t, envFor(url), "", "watch")
	require.Error(t, res.err)
	assert.Equal(t, ""+
		"2026-10-10T12:00:00Z  merge_request_opened  alpha!1 feat/x -> main\n"+
		"2026-10-10T12:00:00Z  comment_added  alpha!1 by agent-7\n"+
		"2026-10-10T12:00:00Z  merge_request_merged  alpha!1 feat/x -> main\n"+
		"2026-10-10T12:00:00Z  merge_request_closed  alpha!1 feat/x -> main\n", res.stdout)

	res = runCLI(t, envFor(url), "", "watch", "--json")
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	require.Len(t, lines, 4)
	assert.Equal(t, "merge_request_opened", decode[eventOut](t, lines[0]).Type)
	opened := decode[eventOut](t, lines[0])
	assert.Equal(t, "alpha", opened.Repo)
	require.NotNil(t, opened.MergeRequest)
	assert.EqualValues(t, 1, opened.MergeRequest.Number)
	assert.Equal(t, headA, opened.MergeRequest.HeadID)
	var added struct {
		Type    string     `json:"type"`
		Repo    string     `json:"repo"`
		Number  int64      `json:"number"`
		Comment commentOut `json:"comment"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &added))
	assert.Equal(t, "comment_added", added.Type)
	assert.Equal(t, "alpha", added.Repo)
	assert.EqualValues(t, 1, added.Number)
	assert.Equal(t, "a.txt", added.Comment.Path)
	assert.Equal(t, "new", added.Comment.Side)
	assert.Equal(t, "merge_request_merged", decode[eventOut](t, lines[2]).Type)
	assert.Equal(t, "merge_request_closed", decode[eventOut](t, lines[3]).Type)
}
