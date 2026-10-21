// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bnema.dev/ermite/internal/ports/mocks"
	"bytes"
	"context"
	"github.com/stretchr/testify/mock"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOnRange(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"new:0-2:a", "new:2-2:a", "new:3-2:a", "new:1-x:a", "new:1-2:", "other:1-2:a"} {
		assert.Nil(t, parseOn(raw), raw)
	}
	assert.Equal(t, &lineRef{side: "new", start: 10, line: 24, path: "dir:a.go"}, parseOn("new:10-24:dir:a.go"))
	assert.Equal(t, &lineRef{side: "old", line: 10, path: "a"}, parseOn("old:10:a"))
}

func TestNoteThreads(t *testing.T) {
	t.Parallel()
	text, err := domain.ParsePlainText("**hello** ^2")
	require.NoError(t, err)
	s := &Server{cfg: Config{Now: time.Now}, md: newMarkdown()}
	comments := []domain.Comment{
		{ID: 1, Author: "alice", Text: text},
		{ID: 2, Author: "bob", ReplyTo: 1, Text: text},
		{ID: 3, Author: "alice", Deleted: true},
		{ID: 4, Author: "alice", Deleted: true},
		{ID: 5, Author: "bob", ReplyTo: 4, Text: text},
		{ID: 6, Author: "alice", Text: text, Verdict: domain.VerdictApprove, Resolved: true},
	}
	p := MRPage{NoteAuthor: "alice"}
	s.placeComments(t.Context(), &p, "demo", 1, comments, "", true, true, nil)
	require.Len(t, p.General, 3)
	assert.Equal(t, 4, p.NComments)
	assert.Equal(t, 1, p.NUnresolved)
	require.Len(t, p.General[0].Replies, 1)
	assert.NotNil(t, p.General[0].Edit)
	assert.Nil(t, p.General[0].Replies[0].Edit)
	assert.Nil(t, p.General[0].Replies[0].Resolve)
	assert.Contains(t, string(p.General[0].Body), "<strong>hello</strong>")
	assert.Contains(t, string(p.General[0].Body), `href="#c2"`)
	assert.True(t, p.General[1].Deleted)
	var b bytes.Buffer
	require.NoError(t, commentView(p.General[0], "").Render(context.Background(), &b))
	assert.Contains(t, b.String(), `name="reply_to" value="1"`)
	assert.Contains(t, b.String(), `data-js data-quote=`)
	assert.Contains(t, b.String(), `/comments/1/edit`)
	assert.NotContains(t, b.String(), `/comments/2/edit`)
}

func TestNoteLiveEvents(t *testing.T) {
	t.Parallel()
	l := liveStream{view: "mr", n: "1"}
	events := []domain.Event{domain.CommentEdited{Number: 1}, domain.CommentDeleted{Number: 1}, domain.CommentResolved{Number: 1}}
	for _, ev := range events {
		we := ports.WatchEvent{Event: ev}
		assert.True(t, liveEvent(we))
		fs, err := l.repoPage(context.Background(), we)
		require.NoError(t, err)
		require.Len(t, fs, 1)
		assert.Contains(t, fs[0].html, "reload")
	}
}

func TestNotePosts(t *testing.T) {
	t.Parallel()
	svc := mocks.NewMergeRequestService(t)
	repo := repoName(t, "demo")
	s := &Server{cfg: Config{MergeRequests: svc}}
	ctx := domain.ContextWithPrincipal(context.Background(), domain.Owner())
	request := func(values url.Values) *http.Request {
		r := httptest.NewRequest("POST", "/demo/merge-requests/1/comments", strings.NewReader(values.Encode())).WithContext(ctx)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetPathValue("repo", "demo")
		r.SetPathValue("n", "1")
		r.SetPathValue("id", "2")
		require.NoError(t, r.ParseForm())
		return r
	}
	svc.EXPECT().AddComment(mock.Anything, mock.Anything, repo, int64(1), mock.MatchedBy(func(p ports.AddCommentParams) bool { return p.ReplyTo == 2 && p.Text == "reply" })).Return(domain.Comment{ID: 7}, nil).Once()
	to, _, err := s.mrComment(ctx, request(url.Values{"reply_to": {"2"}, "text": {"reply"}}))
	require.NoError(t, err)
	assert.Equal(t, "/demo/merge-requests/1#c7", to)
	svc.EXPECT().AddComment(mock.Anything, mock.Anything, repo, int64(1), mock.MatchedBy(func(p ports.AddCommentParams) bool {
		return p.StartLine == 10 && p.Line == 24 && p.Path == "a.go" && p.Verdict == domain.VerdictApprove
	})).Return(domain.Comment{ID: 8}, nil).Once()
	to, _, err = s.mrComment(ctx, request(url.Values{"path": {"a.go"}, "line": {"24"}, "start_line": {"10"}, "side": {"new"}, "verdict": {"approve"}}))
	require.NoError(t, err)
	assert.Equal(t, "/demo/merge-requests/1#c8", to)
	svc.EXPECT().EditComment(mock.Anything, mock.Anything, repo, int64(1), int64(2), "new\ntext").Return(domain.Comment{ID: 2}, nil).Once()
	to, _, err = s.mrEditComment(ctx, request(url.Values{"text": {"new\r\ntext"}}))
	require.NoError(t, err)
	assert.Equal(t, "/demo/merge-requests/1#c2", to)
	svc.EXPECT().DeleteComment(mock.Anything, mock.Anything, repo, int64(1), int64(2)).Return(domain.Comment{ID: 2}, nil).Once()
	to, _, err = s.mrDeleteComment(ctx, request(nil))
	require.NoError(t, err)
	assert.Equal(t, "/demo/merge-requests/1#c2", to)
}

func TestDeletedNoteRendering(t *testing.T) {
	t.Parallel()
	text, err := domain.ParsePlainText("surviving reply")
	require.NoError(t, err)
	s := &Server{cfg: Config{Now: time.Now}, md: newMarkdown()}
	p := MRPage{}
	s.placeComments(t.Context(), &p, "demo", 1, []domain.Comment{
		{ID: 1, Author: "reviewer", Deleted: true, Verdict: domain.VerdictApprove},
		{ID: 2, Deleted: true},
		{ID: 3, ReplyTo: 2, Deleted: true},
		{ID: 4, Deleted: true},
		{ID: 5, ReplyTo: 4, Text: text},
	}, "", true, true, nil)
	require.Len(t, p.General, 2)
	assert.Equal(t, "1", p.General[0].ID)
	assert.Equal(t, "4", p.General[1].ID)
	var b bytes.Buffer
	require.NoError(t, commentView(p.General[0], "").Render(context.Background(), &b))
	assert.Contains(t, b.String(), `<span class="c-who">owner</span>`)
	assert.Contains(t, b.String(), "approve")
	assert.Contains(t, b.String(), "text deleted")
	b.Reset()
	require.NoError(t, commentView(p.General[1], "").Render(context.Background(), &b))
	assert.Contains(t, b.String(), "surviving reply")
	assert.NotContains(t, b.String(), "Quote")
	assert.NotContains(t, b.String(), "data-quote")
}

func TestInlineNoteHeadingIDs(t *testing.T) {
	t.Parallel()
	// "S H" would get the id the split copy of "H" gets if the prefixes of the
	// two copies overlapped.
	text, err := domain.ParsePlainText("# H\n\n# S H\n\n[jump](#h)")
	require.NoError(t, err)
	s := &Server{cfg: Config{Now: time.Now}, md: newMarkdown()}
	p := MRPage{Files: []DiffFile{{Key: "a", ID: "f1", Hunks: []DiffHunk{{Lines: []DiffLine{{Kind: "add", newN: 1}}}}}}}
	s.placeComments(t.Context(), &p, "demo", 1, []domain.Comment{{ID: 12, Path: "a", Side: domain.SideNew, Line: 1, Text: text}}, "", true, true, nil)
	notes := p.Files[0].Hunks[0].Lines[0].Notes
	require.Len(t, notes, 1)
	var b bytes.Buffer
	require.NoError(t, commentView(notes[0], "").Render(context.Background(), &b))
	require.NoError(t, commentView(notes[0], "-s").Render(context.Background(), &b))
	html := b.String()
	for _, id := range []string{"c12-h", "c12-s-h", "c12s-h", "c12s-s-h"} {
		assert.Equal(t, 1, strings.Count(html, `id="`+id+`"`), id)
	}
	assert.Contains(t, html, `href="#c12-h"`)
	assert.Contains(t, html, `href="#c12s-h"`)
}
