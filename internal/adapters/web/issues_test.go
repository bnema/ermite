// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func issueRepoView(t testing.TB, mode domain.IssuesMode) ports.RepoView {
	t.Helper()
	v := publicRepo(t, "proj")
	v.Settings.Issues = mode
	return v
}

func testIssue(t testing.TB, n int64, state domain.IssueState, title, body string) domain.Issue {
	t.Helper()
	ti, err := domain.ParseIssueTitle(title)
	require.NoError(t, err)
	b, err := domain.ParseIssueBody(body)
	require.NoError(t, err)
	return domain.Issue{
		Repo: repoName(t, "proj"), Number: n, Title: ti, Body: b, State: state, Author: "owner",
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Minute),
	}
}

type issueRig struct {
	srv    *Server
	issues *mocks.IssueService
	nums   *mocks.IssueNumberResolver
	mrs    *mocks.MergeRequestService
}

// newIssueRig serves proj with the given mode, signed in as the owner when
// the cookie "tok" is sent.
func newIssueRig(t *testing.T, mode domain.IssuesMode, withNumbers bool) *issueRig {
	t.Helper()
	r := &issueRig{issues: mocks.NewIssueService(t), mrs: mocks.NewMergeRequestService(t)}
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RefPage{}, nil).Maybe()
	r.mrs.EXPECT().CountOpen(mock.Anything, mock.Anything, mock.Anything).Return(0, nil).Maybe()
	cfg := Config{
		Repos: reposServing(t, issueRepoView(t, mode)), Browse: browse, MergeRequests: r.mrs, Issues: r.issues,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	}
	if withNumbers {
		r.nums = mocks.NewIssueNumberResolver(t)
		cfg.IssueNumbers = r.nums
	}
	r.srv = newTestServer(t, cfg)
	return r
}

func (r *issueRig) do(method, path string, owner bool, form url.Values) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", testPublicURL)
	}
	if owner {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	}
	rec := httptest.NewRecorder()
	r.srv.ServeHTTP(rec, req)
	return rec
}

func TestIssueList(t *testing.T) {
	t.Parallel()
	r := newIssueRig(t, domain.IssuesOwner, false)
	r.issues.EXPECT().List(mock.Anything, mock.Anything, repoName(t, "proj"), ports.ListIssuesParams{State: domain.IssueStateOpen, PageSize: pageSize}).
		Return(ports.IssuePage{Issues: []domain.Issue{testIssue(t, 3, domain.IssueStateOpen, "crash <b>on</b> start", "")}, NextPageToken: "tok2"}, nil)
	r.issues.EXPECT().List(mock.Anything, mock.Anything, repoName(t, "proj"), ports.ListIssuesParams{State: domain.IssueStateClosed, PageSize: pageSize}).
		Return(ports.IssuePage{Issues: []domain.Issue{testIssue(t, 2, domain.IssueStateClosed, "old", "")}}, nil)

	rec := r.do("GET", "/proj/issues", false, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, `href="/proj/issues/3"`)
	assert.Contains(t, body, "crash &lt;b&gt;on&lt;/b&gt; start")
	assert.Contains(t, body, `href="/proj/issues?page=tok2"`, "older page")
	assert.Contains(t, body, `href="/proj/issues?state=closed"`, "closed tab")
	assert.Contains(t, body, `href="/proj/issues" data-key="i"`, "the Issues tab")
	assert.NotContains(t, body, "New issue", "no form for an anonymous visitor")
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))

	closed := r.do("GET", "/proj/issues?state=closed", false, nil)
	require.Equal(t, 200, closed.Code)
	assert.Contains(t, closed.Body.String(), `href="/proj/issues/2"`)

	assert.Equal(t, 404, r.do("GET", "/proj/issues?state=weird", false, nil).Code)

	owner := r.do("GET", "/proj/issues", true, nil)
	require.Equal(t, 200, owner.Code)
	assert.Contains(t, owner.Body.String(), `href="/proj/issues/new"`)
	assert.Equal(t, "private, no-store", owner.Header().Get("Cache-Control"))
}

func TestIssueShow(t *testing.T) {
	t.Parallel()
	r := newIssueRig(t, domain.IssuesPublic, false)
	c := domain.IssueComment{ID: 1, Author: "owner", CreatedAt: time.Now()}
	c.Text, _ = domain.ParseIssueBody("see https://example.org <script>x</script>")
	r.issues.EXPECT().Get(mock.Anything, mock.Anything, repoName(t, "proj"), int64(5)).Return(ports.IssueDetail{
		Issue:    testIssue(t, 5, domain.IssueStateOpen, "bug", "# Title\n\n**bold**\n\n<script>alert(1)</script>"),
		Comments: []domain.IssueComment{c},
	}, nil)

	anon := r.do("GET", "/proj/issues/5", false, nil)
	require.Equal(t, 200, anon.Code, anon.Body.String())
	body := anon.Body.String()
	assert.Contains(t, body, "<strong>bold</strong>", "Markdown body")
	assert.NotContains(t, body, "<script>alert")
	assert.Contains(t, body, `id="c1"`)
	assert.Contains(t, body, `href="https://example.org"`)
	assert.Contains(t, body, "&lt;script&gt;x&lt;/script&gt;")
	assert.NotContains(t, body, `class="cform"`)
	assert.NotContains(t, body, "/proj/issues/5/close")
	assert.Contains(t, body, `view=issue`)

	owner := r.do("GET", "/proj/issues/5", true, nil)
	require.Equal(t, 200, owner.Code)
	assert.Contains(t, owner.Body.String(), `action="/proj/issues/5/comments"`)
	assert.Contains(t, owner.Body.String(), `action="/proj/issues/5/close"`)
	assert.NotContains(t, owner.Body.String(), "/proj/issues/5/reopen")

	assert.Equal(t, 404, r.do("GET", "/proj/issues/05", false, nil).Code)
	assert.Equal(t, 404, r.do("GET", "/proj/issues/x", false, nil).Code)
}

func TestIssueShowClosedOffersReopen(t *testing.T) {
	t.Parallel()
	r := newIssueRig(t, domain.IssuesOwner, false)
	r.issues.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything, int64(5)).Return(ports.IssueDetail{
		Issue: testIssue(t, 5, domain.IssueStateClosed, "bug", ""),
	}, nil)
	owner := r.do("GET", "/proj/issues/5", true, nil)
	require.Equal(t, 200, owner.Code)
	assert.Contains(t, owner.Body.String(), `action="/proj/issues/5/reopen"`)
	assert.NotContains(t, owner.Body.String(), "/proj/issues/5/close")
}

func TestIssueNotFound(t *testing.T) {
	t.Parallel()
	// Mode off: the pages and the tab are not there; the use case is not asked.
	off := newIssueRig(t, domain.IssuesOff, false)
	for _, p := range []string{"/proj/issues", "/proj/issues/1", "/proj/issues/new"} {
		assert.Equal(t, 404, off.do("GET", p, true, nil).Code, p)
	}
	summary := off.do("GET", "/proj", false, nil)
	require.Equal(t, 200, summary.Code)
	assert.NotContains(t, summary.Body.String(), `data-key="i"`)

	// The mode is unset in the settings: off as well.
	unset := newIssueRig(t, "", false)
	assert.Equal(t, 404, unset.do("GET", "/proj/issues", false, nil).Code)

	// An unknown repository.
	assert.Equal(t, 404, off.do("GET", "/nope/issues", false, nil).Code)

	// A private repository, for an anonymous visitor.
	priv := mocks.NewRepoService(t)
	priv.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).Return(domain.Repo{}, domain.ErrNotFound)
	browse := mocks.NewBrowseService(t)
	srv := newTestServer(t, Config{Repos: priv, Browse: browse, Issues: mocks.NewIssueService(t)})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/proj/issues/1", nil))
	assert.Equal(t, 404, rec.Code)

	// The use case says not found for a number nobody has.
	r := newIssueRig(t, domain.IssuesOwner, false)
	r.issues.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything, int64(9)).Return(ports.IssueDetail{}, domain.ErrNotFound)
	assert.Equal(t, 404, r.do("GET", "/proj/issues/9", false, nil).Code)
}

func TestIssueNewForm(t *testing.T) {
	t.Parallel()
	r := newIssueRig(t, domain.IssuesOwner, false)
	assert.Equal(t, 404, r.do("GET", "/proj/issues/new", false, nil).Code, "owner only")
	rec := r.do("GET", "/proj/issues/new", true, nil)
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `action="/proj/issues"`)
	assert.Contains(t, rec.Body.String(), `name="title"`)
}

func TestIssuePosts(t *testing.T) {
	t.Parallel()
	r := newIssueRig(t, domain.IssuesOwner, false)
	name := repoName(t, "proj")
	r.issues.EXPECT().Create(mock.Anything, domain.Owner(), name, ports.CreateIssueParams{Title: "t", Body: "a\nb"}).
		Return(testIssue(t, 7, domain.IssueStateOpen, "t", ""), nil)
	r.issues.EXPECT().AddComment(mock.Anything, domain.Owner(), name, int64(7), "hi\nthere").
		Return(domain.IssueComment{ID: 4}, nil)
	r.issues.EXPECT().Close(mock.Anything, domain.Owner(), name, int64(7)).Return(domain.Issue{}, nil)
	r.issues.EXPECT().Reopen(mock.Anything, domain.Owner(), name, int64(7)).Return(domain.Issue{}, domain.ErrPreconditionFailed)

	rec := r.do("POST", "/proj/issues", true, url.Values{"title": {"t"}, "body": {"a\r\nb"}})
	assert.Equal(t, 303, rec.Code)
	assert.Equal(t, "/proj/issues/7", rec.Header().Get("Location"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	rec = r.do("POST", "/proj/issues/7/comments", true, url.Values{"text": {"hi\r\nthere"}})
	assert.Equal(t, 303, rec.Code)
	assert.Equal(t, "/proj/issues/7#c4", rec.Header().Get("Location"))

	rec = r.do("POST", "/proj/issues/7/close", true, nil)
	assert.Equal(t, 303, rec.Code)
	assert.Equal(t, "/proj/issues/7", rec.Header().Get("Location"))

	rec = r.do("POST", "/proj/issues/7/reopen", true, nil)
	assert.Equal(t, 409, rec.Code, "a refused action is a state page")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestIssuePostsRefusedWithoutSession(t *testing.T) {
	t.Parallel()
	r := newIssueRig(t, domain.IssuesPublic, false) // no write use case call is expected
	for _, p := range []string{"/proj/issues", "/proj/issues/7/comments", "/proj/issues/7/close", "/proj/issues/7/reopen"} {
		assert.Equal(t, 403, r.do("POST", p, false, url.Values{"title": {"t"}}).Code, p+" anonymous")
		req := httptest.NewRequest("POST", p, nil)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
		rec := httptest.NewRecorder()
		r.srv.ServeHTTP(rec, req)
		assert.Equal(t, 403, rec.Code, p+" without Origin")
	}
}

func TestIssueRoutesAbsentWithoutService(t *testing.T) {
	t.Parallel()
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RefPage{}, nil).Maybe()
	srv := newTestServer(t, Config{Repos: reposServing(t, issueRepoView(t, domain.IssuesOwner)), Browse: browse})
	for _, p := range []string{"/proj/issues", "/proj/issues/1"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		assert.Equal(t, 404, rec.Code, p)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/proj", nil))
	assert.NotContains(t, rec.Body.String(), `data-key="i"`)
}

func TestIssueNumberRedirects(t *testing.T) {
	t.Parallel()
	r := newIssueRig(t, domain.IssuesOwner, true)
	name := repoName(t, "proj")
	r.issues.EXPECT().Get(mock.Anything, mock.Anything, name, mock.Anything).Return(ports.IssueDetail{}, domain.ErrNotFound)
	r.mrs.EXPECT().Get(mock.Anything, mock.Anything, name, mock.Anything, mock.Anything).Return(ports.MergeRequestDetail{}, domain.ErrNotFound)
	r.nums.EXPECT().Kind(mock.Anything, mock.Anything, name, int64(1)).Return(domain.NumberMergeRequest, nil)
	r.nums.EXPECT().Kind(mock.Anything, mock.Anything, name, int64(2)).Return(domain.NumberIssue, nil)
	r.nums.EXPECT().Kind(mock.Anything, mock.Anything, name, int64(3)).Return(domain.NumberNone, nil)

	rec := r.do("GET", "/proj/issues/1", false, nil)
	assert.Equal(t, 303, rec.Code)
	assert.Equal(t, "/proj/merge-requests/1", rec.Header().Get("Location"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	rec = r.do("GET", "/proj/merge-requests/2", false, nil)
	assert.Equal(t, 303, rec.Code)
	assert.Equal(t, "/proj/issues/2", rec.Header().Get("Location"))

	assert.Equal(t, 404, r.do("GET", "/proj/issues/3", false, nil).Code)
	assert.Equal(t, 404, r.do("GET", "/proj/merge-requests/3", false, nil).Code)
}

func TestIssueNumberNoRedirectWithoutResolver(t *testing.T) {
	t.Parallel()
	r := newIssueRig(t, domain.IssuesOwner, false)
	r.issues.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything, int64(1)).Return(ports.IssueDetail{}, domain.ErrNotFound)
	r.mrs.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything, int64(1), mock.Anything).Return(ports.MergeRequestDetail{}, domain.ErrNotFound)
	assert.Equal(t, 404, r.do("GET", "/proj/issues/1", false, nil).Code)
	assert.Equal(t, 404, r.do("GET", "/proj/merge-requests/1", false, nil).Code)
}

func TestIssueLiveNotices(t *testing.T) {
	t.Parallel()
	r := newIssueRig(t, domain.IssuesOwner, false)
	issue := testIssue(t, 5, domain.IssueStateOpen, "bug", "")
	cases := []struct {
		ev   domain.Event
		view string
		n    string
		want string // "" : nothing
	}{
		{domain.IssueOpened{Issue: issue}, "issues", "", "issues changed"},
		{domain.IssueClosed{Issue: issue}, "issues", "", "issues changed"},
		{domain.IssueOpened{Issue: issue}, "issue", "5", "an issue was opened"},
		{domain.IssueClosed{Issue: issue}, "issue", "5", "this issue was closed"},
		{domain.IssueReopened{Issue: issue}, "issue", "5", "this issue was reopened"},
		{domain.IssueCommented{Repo: issue.Repo, Number: 5}, "issue", "5", "a comment was added"},
		{domain.IssueCommented{Repo: issue.Repo, Number: 6}, "issue", "5", ""},
		{domain.IssueClosed{Issue: issue}, "mrs", "", ""},
		{domain.IssueClosed{Issue: issue}, "summary", "", ""},
	}
	for _, c := range cases {
		ev := ports.WatchEvent{Event: c.ev}
		assert.True(t, liveEvent(ev), c.ev.EventKind())
		got, err := (&liveStream{s: r.srv, repo: "proj", view: c.view, n: c.n}).render(t.Context(), ev)
		require.NoError(t, err)
		if c.want == "" {
			assert.Empty(t, got, "%s on %s", c.ev.EventKind(), c.view)
			continue
		}
		require.Len(t, got, 1, "%s on %s", c.ev.EventKind(), c.view)
		assert.Equal(t, "mr-notice", got[0].event)
		assert.Contains(t, got[0].html, c.want)
	}
	// MR events do not touch the issue pages.
	got, err := (&liveStream{s: r.srv, repo: "proj", view: "issues"}).render(t.Context(), ports.WatchEvent{Event: domain.MergeRequestClosed{}})
	require.NoError(t, err)
	assert.Empty(t, got)
}
