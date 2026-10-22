// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
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

type formRig struct {
	*issueRig
	form *mocks.IssueSubmissionService
}

func newFormRig(t *testing.T, mode domain.IssuesMode) *formRig {
	t.Helper()
	r := &formRig{issueRig: &issueRig{issues: mocks.NewIssueService(t), mrs: mocks.NewMergeRequestService(t)}, form: mocks.NewIssueSubmissionService(t)}
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RefPage{}, nil).Maybe()
	r.mrs.EXPECT().CountOpen(mock.Anything, mock.Anything, mock.Anything).Return(0, nil).Maybe()
	r.srv = newTestServer(t, Config{
		Repos: reposServing(t, issueRepoView(t, mode)), Browse: browse, MergeRequests: r.mrs, Issues: r.issues,
		IssueForm: r.form, Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
	return r
}

func challenge(n int) ports.IssueChallenge {
	return ports.IssueChallenge{
		Token: fmt.Sprintf("tok-%d.sig", n), Nonce: "AAECAwQFBgcICQoLDA0ODw", Difficulty: 18,
		FormToken: fmt.Sprintf("form-%d", n),
		IssuedAt:  time.UnixMilli(1_700_000_000_000), MinSubmitAt: time.UnixMilli(1_700_000_003_000),
		ExpiresAt: time.UnixMilli(1_700_000_600_000),
	}
}

// post sends a submission as a browser of this site would.
func (r *formRig) post(path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.srv.ServeHTTP(rec, req)
	return rec
}

var fromSite = map[string]string{"Origin": testPublicURL}

func goodForm() url.Values {
	return url.Values{"title": {"It crashes"}, "body": {"line1\r\nline2"}, "author": {"Alex"}, "token": {"tok-1.sig"}, "form_token": {"form-1"}, "solution": {"42"}, "website": {""}}
}

func TestIssueFormGet(t *testing.T) {
	t.Parallel()
	r := newFormRig(t, domain.IssuesPublic)
	r.form.EXPECT().Challenge(mock.Anything, repoName(t, "proj"), mock.Anything).Return(challenge(1), nil).Once()

	rec := r.do("GET", "/proj/issues/new", false, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	body := rec.Body.String()
	for _, want := range []string{
		`<noscript>`, "This form needs JavaScript: it runs a short proof of work so that bots cannot flood the tracker.",
		`action="/proj/issues/new"`, ` hidden`, `data-issue-form`, `data-nonce="AAECAwQFBgcICQoLDA0ODw"`,
		`data-difficulty="18"`, `data-min-submit-at="1700000003000"`, `data-worker="/-/static/js/pow-worker.`,
		`name="token" value="tok-1.sig"`, `name="form_token" value="form-1"`, `name="solution" value=""`,
		`name="title" type="text" required`, `<output id="issue-body-count" aria-live="off" data-for="issue-body" data-max="10000"`, `<output id="issue-author-count" aria-live="off" data-for="issue-author" data-max="40"`,
		`<div class="hp" aria-hidden="true">`, `name="website" type="text" tabindex="-1" autocomplete="off"`,
		`<output id="issue-title-count" aria-live="off" data-for="issue-title" data-max="120"`, `0 / 10000`, `Checking your browser… (this takes a few seconds)`,
		`/-/static/js/issue-form.`,
	} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, `maxlength`, "the limits count code points, not UTF-16 units")
	assert.NotContains(t, body, `name="website" type="hidden"`)
	assert.NotContains(t, body, `type="hidden" name="website"`)
	noscript := body[strings.Index(body, "<noscript>"):strings.Index(body, "</noscript>")]
	assert.NotContains(t, noscript, "<form", "no usable form without JavaScript")
	assert.NotContains(t, body, "style=", "no inline style")
	for _, tag := range strings.Split(body, "<script")[1:] {
		head, _, _ := strings.Cut(tag, ">")
		assert.Contains(t, head, `src="`, "no inline script")
	}
}

func TestIssueFormIsNotThereUnlessPublic(t *testing.T) {
	t.Parallel()
	want := ""
	for i, tc := range []struct {
		name string
		mode domain.IssuesMode
		err  error
	}{{"unknown", domain.IssuesPublic, domain.ErrNotFound}, {"owner mode", domain.IssuesOwner, domain.ErrNotFound}, {"off", domain.IssuesOff, domain.ErrNotFound}} {
		r := newFormRig(t, tc.mode)
		r.form.EXPECT().Challenge(mock.Anything, mock.Anything, mock.Anything).Return(ports.IssueChallenge{}, tc.err).Maybe()
		path := "/proj/issues/new"
		if tc.name == "unknown" {
			path = "/nope/issues/new"
		}
		rec := r.do("GET", path, false, nil)
		assert.Equal(t, 404, rec.Code, tc.name)
		if i == 0 {
			want = rec.Body.String()
			continue
		}
		assert.Equal(t, want, rec.Body.String(), tc.name+": the same page")
	}
}

func TestIssueFormOwnerKeepsHisForm(t *testing.T) {
	t.Parallel()
	r := newFormRig(t, domain.IssuesPublic) // no Challenge expected
	rec := r.do("GET", "/proj/issues/new", true, nil)
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `action="/proj/issues"`)
	assert.NotContains(t, rec.Body.String(), "data-issue-form")
}

func TestIssueFormGetPausedAndLimited(t *testing.T) {
	t.Parallel()
	r := newFormRig(t, domain.IssuesPublic)
	r.form.EXPECT().Challenge(mock.Anything, mock.Anything, mock.Anything).Return(ports.IssueChallenge{}, domain.ErrSubmissionsPaused).Once()
	rec := r.do("GET", "/proj/issues/new", false, nil)
	assert.Equal(t, 503, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Contains(t, rec.Body.String(), "Submissions are paused, try later.")
}

func TestIssueFormPostOutcomes(t *testing.T) {
	t.Parallel()
	generic := "The submission could not be verified. Reload the page and try again."
	tests := []struct {
		name   string
		err    error
		status int
		body   string
		hdr    map[string]string
	}{
		{"not found", domain.ErrNotFound, 404, "no such page", nil},
		{"rate limited", &domain.SubmissionRetryError{After: 25 * time.Minute}, 429, "Too many submissions from your network, try again in 25 minutes.", map[string]string{"Retry-After": "1500"}},
		{"rate limited, rounded up", &domain.SubmissionRetryError{After: 90 * time.Second}, 429, "try again in 2 minutes.", map[string]string{"Retry-After": "90"}},
		{"paused", domain.ErrSubmissionsPaused, 503, "Submissions are paused, try later.", nil},
		{"invalid", domain.ErrSubmissionInvalid, 400, generic, nil},
		{"wrapped invalid", fmt.Errorf("%w: x", domain.ErrSubmissionInvalid), 400, generic, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newFormRig(t, domain.IssuesPublic)
			r.form.EXPECT().Submit(mock.Anything, repoName(t, "proj"), mock.Anything, mock.Anything).Return(tc.err).Once()
			rec := r.post("/proj/issues/new", fromSite, goodForm().Encode())
			assert.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.body)
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			for k, v := range tc.hdr {
				assert.Equal(t, v, rec.Header().Get(k))
			}
			assert.NotContains(t, rec.Body.String(), "tok-1.sig", "the token is never echoed")
			for _, leak := range []string{"honeypot", "proof", "too fast", "replay", "banned"} {
				assert.NotContains(t, rec.Body.String(), leak, "no detail")
			}
		})
	}
}

// TestIssueFormPostInvalidTokenLooksTheSame: the use case answers an invalid
// submission the same whatever the client's ban, limit or the queue; the page
// must not differ either.
func TestIssueFormPostInvalidTokenLooksTheSame(t *testing.T) {
	t.Parallel()
	var first *httptest.ResponseRecorder
	for _, state := range []string{"banned", "rate limited", "queue full"} {
		r := newFormRig(t, domain.IssuesPublic)
		r.form.EXPECT().Submit(mock.Anything, repoName(t, "proj"), mock.Anything, mock.Anything).Return(domain.ErrSubmissionInvalid).Once()
		f := goodForm()
		f.Set("token", "garbage")
		rec := r.post("/proj/issues/new", fromSite, f.Encode())
		assert.Equal(t, 400, rec.Code, state)
		if first == nil {
			first = rec
			continue
		}
		assert.Equal(t, first.Body.String(), rec.Body.String(), state)
		assert.Equal(t, first.Header().Get("Retry-After"), rec.Header().Get("Retry-After"), state)
	}
}

func TestIssueFormPostSuccess(t *testing.T) {
	t.Parallel()
	r := newFormRig(t, domain.IssuesPublic)
	r.form.EXPECT().Submit(mock.Anything, repoName(t, "proj"), mock.Anything, ports.IssueSubmission{
		Token: "tok-1.sig", Solution: "42", FormToken: "form-1", Title: "It crashes", Body: "line1\nline2", Author: "Alex",
	}).Return(nil).Once()
	rec := r.post("/proj/issues/new", fromSite, goodForm().Encode())
	assert.Equal(t, 303, rec.Code)
	assert.Equal(t, "/proj/issues/submitted", rec.Header().Get("Location"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	thanks := r.do("GET", "/proj/issues/submitted", false, nil)
	assert.Equal(t, 200, thanks.Code)
	assert.Equal(t, "no-store", thanks.Header().Get("Cache-Control"))
	assert.Contains(t, thanks.Body.String(), "Thanks, your issue is waiting for moderation.")
}

func TestIssueSubmittedPageOnlyForPublic(t *testing.T) {
	t.Parallel()
	r := newFormRig(t, domain.IssuesOwner)
	assert.Equal(t, 404, r.do("GET", "/proj/issues/submitted", false, nil).Code)
}

func TestIssueFormTextErrorShowsTheFormAgain(t *testing.T) {
	t.Parallel()
	r := newFormRig(t, domain.IssuesPublic)
	r.form.EXPECT().Submit(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(fmt.Errorf("title: %w", &domain.TextError{Reason: "is empty"})).Once()
	r.form.EXPECT().Challenge(mock.Anything, mock.Anything, mock.Anything).Return(challenge(2), nil).Once()
	f := goodForm()
	f.Set("title", "<b>mine</b>")
	rec := r.post("/proj/issues/new", fromSite, f.Encode())
	require.Equal(t, 422, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	body := rec.Body.String()
	assert.Contains(t, body, "&lt;b&gt;mine&lt;/b&gt;", "the title is kept")
	assert.Contains(t, body, "line1\nline2", "the body is kept")
	assert.Contains(t, body, `value="Alex"`)
	assert.Contains(t, body, "The title is empty.")
	assert.Contains(t, body, `name="token" value="tok-2.sig"`, "a new challenge")
	assert.NotContains(t, body, "tok-1.sig", "the spent token is not echoed")
}

func TestIssueFormPostLimits(t *testing.T) {
	t.Parallel()
	t.Run("oversize", func(t *testing.T) {
		t.Parallel()
		r := newFormRig(t, domain.IssuesPublic) // Submit is never called
		big := url.Values{"title": {"x"}, "body": {strings.Repeat("a", maxIssueFormBytes+10)}}
		rec := r.post("/proj/issues/new", fromSite, big.Encode())
		assert.Equal(t, 413, rec.Code)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	})
	t.Run("duplicated fields", func(t *testing.T) {
		t.Parallel()
		for _, f := range issueFormFields {
			r := newFormRig(t, domain.IssuesPublic)
			rec := r.post("/proj/issues/new", fromSite, goodForm().Encode()+"&"+f+"=again")
			assert.Equal(t, 400, rec.Code, f)
			assert.Contains(t, rec.Body.String(), "could not be verified", f)
		}
	})
}

// The gate still demands the Origin of this site for the anonymous POST.
func TestIssueFormPostNeedsTheOrigin(t *testing.T) {
	t.Parallel()
	r := newFormRig(t, domain.IssuesPublic) // Submit is never called
	for name, hdr := range map[string]map[string]string{
		"missing":                 nil,
		"cross origin":            {"Origin": "https://evil.example"},
		"null":                    {"Origin": "null"},
		"same host, other scheme": {"Origin": "http://forge.example"},
	} {
		rec := r.post("/proj/issues/new", hdr, goodForm().Encode())
		assert.Equal(t, 403, rec.Code, name)
	}
}

func TestIssueFormPostOnOwnerModeIsNotFound(t *testing.T) {
	t.Parallel()
	r := newFormRig(t, domain.IssuesOwner)
	r.form.EXPECT().Submit(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(domain.ErrNotFound).Once()
	rec := r.post("/proj/issues/new", fromSite, goodForm().Encode())
	assert.Equal(t, 404, rec.Code)
}

// anonymousPost opens exactly /<repo>/issues/new and nothing else.
func TestAnonymousPostOpensOnlyTheIssueForm(t *testing.T) {
	t.Parallel()
	r := newFormRig(t, domain.IssuesPublic)
	for _, p := range []string{
		"/x/issues/new/extra", "/x/issues/123", "/x/issues", "/x/issues/new/", "/x/issues/123/close", "/x/issues/new/close",
		"/x%2Fissues/new", "/x%2Fy/issues/new", "/%2Fissues/new", "/issues/new", "//issues/new", "/X/issues/new",
		"/a/b/issues/new", "/x.git/issues/new", "/../issues/new", "/-/inbox/pending/x/y/approve", "/x/merge-requests",
		"/x/issues%2Fnew", "/x/issues/new%2Fextra", "/x/issues/%6Eew",
	} {
		u := &url.URL{Path: p}
		if dec, err := url.PathUnescape(p); err == nil {
			u = &url.URL{Path: dec, RawPath: p}
		}
		req := &http.Request{Method: http.MethodPost, URL: u}
		got := r.srv.anonymousPost(req)
		want := false
		assert.Equal(t, want, got, p)
	}
	for _, p := range []string{"/x/issues/new", "/proj/issues/new", "/a.b_c-d/issues/new"} {
		req := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: p}}
		assert.True(t, r.srv.anonymousPost(req), p)
		req.Method = http.MethodPut
		assert.False(t, r.srv.anonymousPost(req), p+" PUT")
	}
	// Without the use case the path is not opened at all.
	plain := newIssueRig(t, domain.IssuesPublic, false)
	req := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/proj/issues/new"}}
	assert.False(t, plain.srv.anonymousPost(req))
}

func TestIssueListShowsNewIssueToVisitorsOfPublicRepos(t *testing.T) {
	t.Parallel()
	for mode, want := range map[domain.IssuesMode]bool{domain.IssuesPublic: true, domain.IssuesOwner: false} {
		r := newFormRig(t, mode)
		r.issues.EXPECT().List(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.IssuePage{}, nil)
		rec := r.do("GET", "/proj/issues", false, nil)
		require.Equal(t, 200, rec.Code)
		assert.Equal(t, want, strings.Contains(rec.Body.String(), `href="/proj/issues/new"`), string(mode))
	}
}

func TestExternalIssueIsRenderedUntrusted(t *testing.T) {
	t.Parallel()
	r := newFormRig(t, domain.IssuesPublic)
	i := testIssue(t, 5, domain.IssueStateOpen, "bug", "[a](javascript:alert(1)) [b](https://example.org/) <script>x</script>")
	i.External, i.Author = true, "Eve <b>"
	r.issues.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything, int64(5)).Return(ports.IssueDetail{Issue: i}, nil)
	rec := r.do("GET", "/proj/issues/5", false, nil)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "javascript:")
	assert.NotContains(t, body, "<script>x")
	assert.Contains(t, body, `rel="nofollow noopener noreferrer ugc"`)
	assert.Contains(t, body, "Eve &lt;b&gt;")
}
